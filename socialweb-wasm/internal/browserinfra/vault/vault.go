// Package vault is the browser-side, in-memory substitute for the OpenBao
// vault contract. It implements vault.KV, atproto.SecretStore, and the
// component credential resolver over one concurrency-safe store, with
// Snapshot/Restore for persistence. No JavaScript and no network.
package vault

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	contractvault "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/marketidentity/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

// ErrNotFound means no entry (or no matching version) exists. It wraps
// openbao.ErrNotFound so server-side callers that test for the OpenBao
// sentinel keep working against the browser substitute.
var ErrNotFound = fmt.Errorf("browser vault: secret not found: %w", openbao.ErrNotFound)

// ErrInvalidRef means a call named no mount or no path.
var ErrInvalidRef = errors.New("browser vault: a KV call needs a mount and a path")

type kvKey struct {
	mount string
	path  string
}

type entry struct {
	data    map[string]any
	version int
}

// Store is a concurrency-safe in-memory KV. The zero value is ready to use.
type Store struct {
	mu      sync.RWMutex
	entries map[kvKey]entry
	// onChange is called after every write or delete, outside the store lock,
	// so the browser can persist a snapshot. Restore does not call it.
	onChange func()
}

// SetOnChange installs the callback invoked after writes and deletes. A nil
// callback disables it.
func (s *Store) SetOnChange(fn func()) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

var _ contractvault.KV = (*Store)(nil)

// NewStore returns an empty store.
func NewStore() *Store { return &Store{entries: map[kvKey]entry{}} }

// Read returns a copy of the data at mount/path. A positive version must
// match the entry's current version: historical versions are not retained.
func (s *Store) Read(ctx context.Context, mount, path string, version int) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	k, err := kvKeyOf(mount, path)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	e, ok := s.entries[k]
	s.mu.RUnlock()
	if !ok || (version > 0 && version != e.version) {
		return nil, fmt.Errorf("browser vault: %s/%s: %w", k.mount, k.path, ErrNotFound)
	}
	return cloneData(e.data), nil
}

// Write stores a copy of data at mount/path and bumps the entry version.
func (s *Store) Write(ctx context.Context, mount, path string, data map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	k, err := kvKeyOf(mount, path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.entries == nil {
		s.entries = map[kvKey]entry{}
	}
	s.entries[k] = entry{data: cloneData(data), version: s.entries[k].version + 1}
	fn := s.onChange
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
	return nil
}

// Delete removes mount/path. It is idempotent, matching the OpenBao KV
// metadata delete that a missing path does not fail.
func (s *Store) Delete(ctx context.Context, mount, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	k, err := kvKeyOf(mount, path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.entries, k)
	fn := s.onChange
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
	return nil
}

// List returns the immediate children under mount/path, sorted; directories
// carry a trailing slash, as the OpenBao LIST response does. The vault.KV
// contract has no List, so this is for the substitute controller's own
// listing surface.
func (s *Store) List(ctx context.Context, mount, path string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	path = strings.Trim(strings.TrimSpace(path), "/")
	if mount == "" {
		return nil, ErrInvalidRef
	}
	prefix := path
	if prefix != "" {
		prefix += "/"
	}
	seen := map[string]struct{}{}
	s.mu.RLock()
	for k := range s.entries {
		if k.mount != mount || !strings.HasPrefix(k.path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(k.path, prefix)
		if rest == "" {
			continue
		}
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			rest = rest[:i+1]
		}
		seen[rest] = struct{}{}
	}
	s.mu.RUnlock()
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// SnapshotEntry is one persisted KV entry.
type SnapshotEntry struct {
	Mount   string         `json:"mount"`
	Path    string         `json:"path"`
	Version int            `json:"version"`
	Data    map[string]any `json:"data"`
}

// Snapshot is a JSON-round-trippable copy of the whole store.
type Snapshot struct {
	Entries []SnapshotEntry `json:"entries"`
}

// Snapshot returns a copy of the store, ordered by mount and path.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := Snapshot{Entries: make([]SnapshotEntry, 0, len(s.entries))}
	for k, e := range s.entries {
		out.Entries = append(out.Entries, SnapshotEntry{
			Mount: k.mount, Path: k.path, Version: e.version, Data: cloneData(e.data),
		})
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		if out.Entries[i].Mount != out.Entries[j].Mount {
			return out.Entries[i].Mount < out.Entries[j].Mount
		}
		return out.Entries[i].Path < out.Entries[j].Path
	})
	return out
}

// Restore replaces the store with a copy of snapshot. Invalid entries are
// skipped; a nil snapshot clears the store.
func (s *Store) Restore(snapshot Snapshot) {
	entries := make(map[kvKey]entry, len(snapshot.Entries))
	for _, e := range snapshot.Entries {
		k, err := kvKeyOf(e.Mount, e.Path)
		if err != nil {
			continue
		}
		version := e.Version
		if version < 1 {
			version = 1
		}
		entries[k] = entry{data: cloneData(e.Data), version: version}
	}
	s.mu.Lock()
	s.entries = entries
	s.mu.Unlock()
}

// Secrets is an atproto.SecretStore over a Store.
type Secrets struct {
	store *Store
}

var _ atproto.SecretStore = (*Secrets)(nil)

// NewSecrets returns a secret store backed by store. A nil store allocates
// one.
func NewSecrets(store *Store) *Secrets {
	if store == nil {
		store = NewStore()
	}
	return &Secrets{store: store}
}

// Get returns the secret named by ref. SecretRef.Vault and SecretRef.Key are
// ignored: the browser has one store, and whole-secret reads match the
// server-side identity vault. A missing or empty secret yields
// atproto.ErrSecretNotFound.
func (s *Secrets) Get(ctx context.Context, ref commonv1.SecretRef) (map[string]any, error) {
	data, err := s.store.Read(ctx, ref.Mount, ref.Path, 0)
	if errors.Is(err, ErrNotFound) {
		return nil, atproto.ErrSecretNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, atproto.ErrSecretNotFound
	}
	return data, nil
}

// Put writes data as the secret named by ref.
func (s *Secrets) Put(ctx context.Context, ref commonv1.SecretRef, data map[string]any) error {
	return s.store.Write(ctx, ref.Mount, ref.Path, data)
}

// Delete removes the secret named by ref. Deleting a missing secret is not
// an error.
func (s *Secrets) Delete(ctx context.Context, ref commonv1.SecretRef) error {
	return s.store.Delete(ctx, ref.Mount, ref.Path)
}

// Snapshot returns the backing store's snapshot.
func (s *Secrets) Snapshot() Snapshot { return s.store.Snapshot() }

// Restore replaces the backing store with snapshot.
func (s *Secrets) Restore(snapshot Snapshot) { s.store.Restore(snapshot) }

// Credentials is a component.CredentialResolver, CredentialWriter, and
// CredentialDeleter over a Store. CredentialRef.Server is ignored and
// CredentialRef.Version, when positive, must match the current entry version.
type Credentials struct {
	store *Store
}

var (
	_ component.CredentialResolver = (*Credentials)(nil)
	_ component.CredentialWriter   = (*Credentials)(nil)
	_ component.CredentialDeleter  = (*Credentials)(nil)
)

// NewCredentials returns a credential resolver backed by store. A nil store
// allocates one.
func NewCredentials(store *Store) *Credentials {
	if store == nil {
		store = NewStore()
	}
	return &Credentials{store: store}
}

// Resolve reads the credential at ref.Mount/ref.Path and filters ref.Keys.
func (c *Credentials) Resolve(ctx context.Context, ref component.CredentialRef) (component.Credential, error) {
	if err := ref.Check(); err != nil {
		return component.Credential{}, err
	}
	data, err := c.store.Read(ctx, ref.Mount, ref.Path, ref.Version)
	if err != nil {
		return component.Credential{}, fmt.Errorf("browser vault: reading %s/%s: %w", ref.Mount, ref.Path, err)
	}
	if len(data) == 0 {
		return component.Credential{}, fmt.Errorf("browser vault: %s/%s: %w", ref.Mount, ref.Path, ErrNotFound)
	}
	return component.Credential{Ref: ref, Data: filterKeys(data, ref.Keys)}, nil
}

// WriteCredential stores data at ref.Mount/ref.Path.
func (c *Credentials) WriteCredential(ctx context.Context, ref component.CredentialRef, data map[string]any) error {
	if err := ref.Check(); err != nil {
		return err
	}
	if data == nil {
		return errors.New("browser vault: a credential write needs data")
	}
	return c.store.Write(ctx, ref.Mount, ref.Path, data)
}

// DeleteCredential removes the credential at ref.Mount/ref.Path. Deleting a
// missing credential is not an error.
func (c *Credentials) DeleteCredential(ctx context.Context, ref component.CredentialRef) error {
	if err := ref.Check(); err != nil {
		return err
	}
	return c.store.Delete(ctx, ref.Mount, ref.Path)
}

// kvKeyOf normalizes a call the way openbao.Handle does: trim slashes and
// accept paths written as mount/data/... or mount/metadata/....
func kvKeyOf(mount, path string) (kvKey, error) {
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	path = strings.Trim(strings.TrimSpace(path), "/")
	path = strings.TrimPrefix(strings.TrimPrefix(path, mount+"/data/"), mount+"/metadata/")
	if mount == "" || path == "" {
		return kvKey{}, ErrInvalidRef
	}
	return kvKey{mount: mount, path: path}, nil
}

func filterKeys(data map[string]any, keys []string) map[string]any {
	if len(keys) == 0 {
		return data
	}
	out := make(map[string]any, len(keys))
	for _, key := range keys {
		if value, ok := data[key]; ok {
			out[key] = value
		}
	}
	return out
}

// cloneData copies the JSON-shaped values the vault holds so callers cannot
// mutate stored secrets through Returned maps or byte slices.
func cloneData(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch x := v.(type) {
	case []byte:
		return append([]byte(nil), x...)
	case map[string]any:
		return cloneData(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = cloneValue(item)
		}
		return out
	default:
		return v
	}
}
