// Package vaulthost is the browser-side substitute for the OpenBaoServer and
// Vault controllers and for the vault controller's bus provider. There is no
// OpenBao process: vault.read and vault.write resolve to the in-memory KV in
// internal/browserinfra/vault, one Store per workspace, with the default mount
// taken from the bound OpenBaoServer's status.refs.kvMount.
package vaulthost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	contractvault "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	bvault "github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/vault"
)

const (
	// DefaultKVMount matches openbaoctl.KVMount, the core KV mount the real
	// OpenBao layout creates.
	DefaultKVMount = "secret"

	// Address is the non-empty status.address the substitute reports.
	Address = "memory://browser"

	// Version distinguishes the substitute from a real OpenBao version.
	Version = "browser-memory"
)

var (
	ErrUnbound = errors.New("vaulthost: the Vault is not bound to a ready local store")

	ErrOutOfScope = errors.New("vaulthost: the path is outside the handle's scope")

	ErrReadOnly = errors.New("vaulthost: the handle was granted vault.read only")
)

// VaultGroupKind is the GroupKind the real vault provider serves.
var VaultGroupKind = schema.GroupKind{Group: v1alpha1.GroupName, Kind: "Vault"}

type vaultKey struct {
	workspace string

	name string
}

func keyOf(ref commonv1.ObjectRef) vaultKey {
	return vaultKey{workspace: strings.TrimSpace(ref.Workspace), name: strings.TrimSpace(ref.Name)}
}

// Binding is one Vault object resolved to the workspace store and its default
// KV mount.
type Binding struct {
	Vault commonv1.ObjectRef

	Mount string
}

// Provider serves vault.read and vault.write for bound Vault objects from the
// browser memory store. Unlike the server provider it dials nothing: the
// binding is created by the Vault controller from the substitute
// OpenBaoServer status.
type Provider struct {
	mu sync.RWMutex

	bindings map[vaultKey]Binding

	stores map[string]*bvault.Store

	// onChange is installed on every workspace store so the browser can
	// persist the vault after a write.
	onChange func()
}

// NewProvider returns a provider with no bindings and no stores.
func NewProvider() *Provider {
	return &Provider{bindings: map[vaultKey]Binding{}, stores: map[string]*bvault.Store{}}
}

// Bind records the store resolution for a Vault. It reports whether the
// binding changed.
func (p *Provider) Bind(b Binding) bool {
	if strings.TrimSpace(b.Vault.Name) == "" || strings.TrimSpace(b.Vault.Workspace) == "" {
		return false
	}
	b.Mount = strings.Trim(strings.TrimSpace(b.Mount), "/")
	if b.Mount == "" {
		b.Mount = DefaultKVMount
	}
	k := keyOf(b.Vault)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bindings == nil {
		p.bindings = map[vaultKey]Binding{}
	}
	if old, ok := p.bindings[k]; ok && old.Mount == b.Mount {
		return false
	}
	p.bindings[k] = b
	return true
}

// Unbind forgets a Vault. The workspace store keeps its data.
func (p *Provider) Unbind(ref commonv1.ObjectRef) {
	p.mu.Lock()
	delete(p.bindings, keyOf(ref))
	p.mu.Unlock()
}

// Store returns the memory store shared by every Vault in workspace, creating
// it on first use. The pointer stays valid across Restore.
func (p *Provider) Store(workspace string) *bvault.Store {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.storeLocked(strings.TrimSpace(workspace))
}

func (p *Provider) storeLocked(workspace string) *bvault.Store {
	if p.stores == nil {
		p.stores = map[string]*bvault.Store{}
	}
	store := p.stores[workspace]
	if store == nil {
		store = bvault.NewStore()
		store.SetOnChange(p.onChange)
		p.stores[workspace] = store
	}
	return store
}

// SetOnChange installs a callback on every workspace store, existing and
// future. It is called after a write or delete, never under a store lock.
func (p *Provider) SetOnChange(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onChange = fn
	for _, store := range p.stores {
		store.SetOnChange(fn)
	}
}

// binding returns the binding and the workspace store for a Vault ref.
func (p *Provider) binding(ref commonv1.ObjectRef) (Binding, *bvault.Store, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.bindings[keyOf(ref)]
	if !ok {
		return Binding{}, nil, false
	}
	return b, p.storeLocked(strings.TrimSpace(ref.Workspace)), true
}

func (p *Provider) open(to commonv1.ObjectRef, scope []string) (*handle, error) {
	b, store, ok := p.binding(to)
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s", ErrUnbound, to.Workspace, to.Name)
	}
	cleaned := cleanScope(scope)
	if len(cleaned) == 0 {
		return nil, fmt.Errorf("%w: a handle needs a scope", ErrOutOfScope)
	}
	return &handle{store: store, mount: b.Mount, scope: cleaned}, nil
}

// Register provides the same GroupKind, capability names, and contractvault.KV
// type as internal/controllers/vault, resolving to the local store. Leases,
// PKI, key rotation, and Trust stay unavailable in the browser.
func (p *Provider) Register(b *bus.Bus) {
	bus.Provide(b, VaultGroupKind, contractvault.CapabilityRead, func(_ context.Context, to commonv1.ObjectRef, scope []string) (contractvault.KV, error) {
		h, err := p.open(to, scope)
		if err != nil {
			return nil, err
		}
		return readOnly{h}, nil
	})
	bus.Provide(b, VaultGroupKind, contractvault.CapabilityWrite, func(_ context.Context, to commonv1.ObjectRef, scope []string) (contractvault.KV, error) {
		return p.open(to, scope)
	})
}

// Snapshot is a JSON-round-trippable copy of every workspace store.
type Snapshot struct {
	Workspaces map[string]bvault.Snapshot `json:"workspaces"`
}

// Snapshot returns a copy of all stores, keyed by workspace.
func (p *Provider) Snapshot() Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := Snapshot{Workspaces: make(map[string]bvault.Snapshot, len(p.stores))}
	for workspace, store := range p.stores {
		out.Workspaces[workspace] = store.Snapshot()
	}
	return out
}

// Restore replaces every store with the snapshot's contents. Stores already
// handed to NewSecrets/NewCredentials keep working; a workspace missing from
// the snapshot is cleared.
func (p *Provider) Restore(snapshot Snapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stores == nil {
		p.stores = map[string]*bvault.Store{}
	}
	for workspace, store := range p.stores {
		store.Restore(snapshot.Workspaces[workspace])
	}
	for workspace, s := range snapshot.Workspaces {
		if _, ok := p.stores[workspace]; ok {
			continue
		}
		store := bvault.NewStore()
		store.SetOnChange(p.onChange)
		store.Restore(s)
		p.stores[workspace] = store
	}
}

// handle is the local equivalent of openbao.Handle: it fills the default
// mount and enforces the capability scope before touching the store.
type handle struct {
	store *bvault.Store

	mount string

	scope []string
}

var _ contractvault.KV = (*handle)(nil)

func (h *handle) target(mount, path string) (string, string, error) {
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	if mount == "" {
		mount = h.mount
	}
	path = strings.Trim(strings.TrimSpace(path), "/")
	path = strings.TrimPrefix(strings.TrimPrefix(path, mount+"/data/"), mount+"/metadata/")
	if mount == "" || path == "" {
		return "", "", bvault.ErrInvalidRef
	}
	if !allows(h.scope, mount+"/"+path) {
		return "", "", fmt.Errorf("%w: %s/%s", ErrOutOfScope, mount, path)
	}
	return mount, path, nil
}

func (h *handle) Read(ctx context.Context, mount, path string, version int) (map[string]any, error) {
	mount, path, err := h.target(mount, path)
	if err != nil {
		return nil, err
	}
	return h.store.Read(ctx, mount, path, version)
}

func (h *handle) Write(ctx context.Context, mount, path string, data map[string]any) error {
	mount, path, err := h.target(mount, path)
	if err != nil {
		return err
	}
	return h.store.Write(ctx, mount, path, data)
}

func (h *handle) Delete(ctx context.Context, mount, path string) error {
	mount, path, err := h.target(mount, path)
	if err != nil {
		return err
	}
	return h.store.Delete(ctx, mount, path)
}

var _ contractvault.KV = readOnly{}

type readOnly struct {
	*handle
}

func (readOnly) Write(context.Context, string, string, map[string]any) error { return ErrReadOnly }

func (readOnly) Delete(context.Context, string, string) error { return ErrReadOnly }

func cleanScope(scope []string) []string {
	out := make([]string, 0, len(scope))
	for _, s := range scope {
		if s = strings.Trim(strings.TrimSpace(s), "/"); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// allows mirrors openbao.Handle.allows.
func allows(scope []string, target string) bool {
	target = strings.Trim(target, "/")
	for _, s := range scope {
		if s == "*" || s == target {
			return true
		}
		if prefix, ok := strings.CutSuffix(s, "*"); ok && strings.HasPrefix(target, prefix) {
			return true
		}
		if strings.HasPrefix(target, s+"/") {
			return true
		}
	}
	return false
}
