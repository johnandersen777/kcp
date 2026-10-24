package vaulthost

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	// Name is the Vault controller name and the package's log identity.
	Name = "vaulthost"

	// Export serves both OpenBaoServer and Vault, as the real openbaoserver
	// and vault controllers do.
	Export = "hostprocesses"

	// Finalizer unbinds a Vault before it disappears.
	Finalizer = "socialweb.computer/vaulthost"

	// RequeuePending is how soon an unbound Vault retries. The substitute
	// OpenBaoServer turns Ready on its first reconcile, so this only covers
	// apply ordering; there is no watch from a backend onto its Vaults.
	RequeuePending = 2 * time.Second
)

// Options scopes the controllers to a set of workspaces. A nil Scope keeps the
// manager's workspaces.
type Options struct {
	Scope func(workspace string) bool
}

// Controller reconciles the substitute OpenBaoServer and Vault objects and
// owns the provider they bind.
type Controller struct {
	provider *Provider
}

// New returns a controller and its provider without registering anything.
func New() *Controller { return &Controller{provider: NewProvider()} }

// Register wires the provider into b and registers both controllers on m.
func Register(m *ctlruntime.Manager, b *bus.Bus, opts Options) (*Controller, error) {
	ctl := New()
	ctl.provider.Register(b)
	serverOptions := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.InWorkspaces(opts.Scope)}
	if err := ctlruntime.For[*v1alpha1.OpenBaoServer](m, Name+"-server", serverReconciler{ctl: ctl}, serverOptions...); err != nil {
		return nil, fmt.Errorf("vaulthost: registering the OpenBaoServer controller: %w", err)
	}
	vaultOptions := append(serverOptions, ctlruntime.Finalizer(Finalizer))
	if err := ctlruntime.For[*v1alpha1.Vault](m, Name, vaultReconciler{ctl: ctl}, vaultOptions...); err != nil {
		return nil, fmt.Errorf("vaulthost: registering the Vault controller: %w", err)
	}
	return ctl, nil
}

// Provider returns the bus provider so the orchestrator can share its stores.
func (c *Controller) Provider() *Provider { return c.provider }

// Snapshot returns every workspace store for persistence.
func (c *Controller) Snapshot() Snapshot { return c.provider.Snapshot() }

// Restore loads a previously taken Snapshot.
func (c *Controller) Restore(snapshot Snapshot) { c.provider.Restore(snapshot) }

type serverReconciler struct {
	ctl *Controller
}

func (r serverReconciler) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.OpenBaoServer) (ctlruntime.Result, error) {
	if !object.DeletionTimestamp.IsZero() {
		return ctlruntime.Result{}, nil
	}
	next := object.DeepCopy()
	next.Status = ServerStatus(object)
	return ctlruntime.Result{}, req.ApplyStatus(ctx, next)
}

// ServerStatus is the whole substitute status: the browser vault is always
// initialized, unsealed, and Ready.
func ServerStatus(object *v1alpha1.OpenBaoServer) v1alpha1.OpenBaoServerStatus {
	gen := object.Generation
	st := *object.Status.DeepCopy()
	st.ObservedGeneration = gen
	st.Ready = true
	st.Phase = v1alpha1.OpenBaoServerPhaseReady
	st.Initialized = true
	st.Sealed = false
	st.Address = Address
	st.Version = Version
	st.Message = "the in-browser vault serves memory KV"
	mount := DefaultKVMount
	if len(object.Spec.Clients) > 0 && object.Spec.Clients[0].KVMount != "" {
		mount = object.Spec.Clients[0].KVMount
	}
	st.Refs = &v1alpha1.OpenBaoServerRefs{KVMount: mount}
	st.Clients = nil
	for _, client := range object.Spec.Clients {
		clientMount := client.KVMount
		if clientMount == "" {
			clientMount = mount
		}
		st.Clients = append(st.Clients, v1alpha1.OpenBaoServerClientRefs{
			Name:       client.Name,
			KVMount:    clientMount,
			ClientCert: "memory://browser/" + client.Name,
		})
	}
	conds.Set(&st.Conditions, gen, commonv1.ConditionReady, true, "Ready",
		"the in-browser vault is initialized, unsealed, and serves memory KV")
	return st
}

type vaultReconciler struct {
	ctl *Controller
}

func (r vaultReconciler) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.Vault) (ctlruntime.Result, error) {
	self := commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: "Vault", Name: object.Name, Workspace: req.Workspace}
	if !object.DeletionTimestamp.IsZero() {
		r.ctl.provider.Unbind(self)
		return ctlruntime.Result{}, nil
	}
	server, err := r.backend(ctx, req, object)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	next := object.DeepCopy()
	next.Status = VaultStatus(object, server)
	if err := req.ApplyStatus(ctx, next); err != nil {
		return ctlruntime.Result{}, err
	}
	if server == nil || !next.Status.Ready {
		r.ctl.provider.Unbind(self)
		return ctlruntime.Result{RequeueAfter: RequeuePending}, nil
	}
	r.ctl.provider.Bind(Binding{Vault: self, Mount: MountOf(server)})
	return ctlruntime.Result{}, nil
}

// backend reads the local OpenBaoServer named by spec.backendRef. A nil server
// with a nil error means the ref is unsupported or the backend does not exist
// yet; the Vault then reports not Ready and retries.
func (r vaultReconciler) backend(ctx context.Context, req ctlruntime.Request, object *v1alpha1.Vault) (*v1alpha1.OpenBaoServer, error) {
	ref := object.Spec.BackendRef
	if g := strings.TrimSpace(ref.Group); g != "" && g != v1alpha1.GroupName {
		return nil, nil
	}
	if ws := strings.TrimSpace(ref.Workspace); ws != "" && ws != req.Workspace {
		return nil, nil
	}
	if ref.Kind != v1alpha1.KindOpenBaoServer {
		return nil, nil
	}
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return nil, nil
	}
	var server v1alpha1.OpenBaoServer
	if err := req.Get(ctx, "", name, &server); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &server, nil
}

// VaultStatus copies the backend's readiness and address, as the real Vault
// controller does. A nil server yields the unbound status.
func VaultStatus(object *v1alpha1.Vault, server *v1alpha1.OpenBaoServer) v1alpha1.VaultStatus {
	gen := object.Generation
	st := *object.Status.DeepCopy()
	st.ObservedGeneration = gen
	if server == nil {
		st.Address, st.Initialized, st.Sealed, st.CABundle, st.Version, st.Ready = "", false, false, "", "", false
		conds.Set(&st.Conditions, gen, v1alpha1.VaultConditionBackendBound, false, "BackendUnbound",
			"spec.backendRef names no local OpenBaoServer")
		conds.Set(&st.Conditions, gen, v1alpha1.VaultConditionReady, false, "BackendNotReady",
			"spec.backendRef names no local OpenBaoServer")
		return st
	}
	sst := server.Status
	st.Address, st.Initialized, st.Sealed, st.CABundle, st.Version = sst.Address, sst.Initialized, sst.Sealed, sst.CABundle, sst.Version
	st.Ready = sst.Ready && sst.ObservedGeneration == server.Generation
	conds.Set(&st.Conditions, gen, v1alpha1.VaultConditionBackendBound, true, "Bound",
		fmt.Sprintf("bound to %s %s", v1alpha1.KindOpenBaoServer, server.Name))
	if st.Ready {
		conds.Set(&st.Conditions, gen, v1alpha1.VaultConditionReady, true, "BackendReady",
			fmt.Sprintf("%s %s is ready", v1alpha1.KindOpenBaoServer, server.Name))
	} else {
		conds.Set(&st.Conditions, gen, v1alpha1.VaultConditionReady, false, "BackendNotReady",
			fmt.Sprintf("%s %s is not ready", v1alpha1.KindOpenBaoServer, server.Name))
	}
	return st
}

// MountOf returns the backend's KV mount, falling back to DefaultKVMount.
func MountOf(server *v1alpha1.OpenBaoServer) string {
	if server != nil && server.Status.Refs != nil {
		if mount := strings.Trim(strings.TrimSpace(server.Status.Refs.KVMount), "/"); mount != "" {
			return mount
		}
	}
	return DefaultKVMount
}
