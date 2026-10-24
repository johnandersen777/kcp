package exposure

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"

	bvault "github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/vault"
)

const (
	// Name is the controller name and log identity.
	Name = "exposure"

	// Export is the APIExport serving Exposure and Service, like the real
	// exposure and service controllers.
	Export = "exposures"

	// ServiceKind is the kind of the Service an Exposure points at.
	ServiceKind = "Service"

	// RequeuePending is how soon an Exposure missing its Service or handler
	// retries. Handler registration on the bus has no watch, so this is the
	// only way the route appears after the component starts.
	RequeuePending = 2 * time.Second
)

// Options configures the substitute.
type Options struct {
	// Origin is the public origin the browser serves the page from, e.g.
	// https://abc.fedproxy.com. It becomes status.address. Required.
	Origin string

	// Handlers is the bus componentcore registers component HTTP handlers on.
	// When nil the controller only writes status and registers no route.
	Handlers *bus.Bus

	// Scope limits the controller to a set of workspaces. Nil keeps the
	// manager's workspace filter.
	Scope func(workspace string) bool

	// IngressProxyHost turns on ingress mode: Exposures with mode ingress are
	// served through this xrpc-subscription relay (e.g. xrpc.fedproxy.com)
	// instead of the page origin. Empty keeps every Exposure on the origin.
	IngressProxyHost string

	// Keys returns the vault store holding a workspace's per-Exposure ingress
	// keys. Required with IngressProxyHost.
	Keys func(workspace string) *bvault.Store

	// OnIngress receives the ingress Exposures after every change, so the page
	// can open or close relay subscriptions.
	OnIngress func([]Ingress)

	Log *slog.Logger
}

// Controller reconciles Exposure objects and owns the in-process route
// registry.
type Controller struct {
	origin Origin

	handlers *bus.Bus

	registry *Registry

	refs *binding.Table[struct{}]

	ingress *ingressState

	log *slog.Logger
}

// New validates the origin and returns a controller without registering
// anything. It has no manager and no bus: use Register for the wiring.
func New(opts Options) (*Controller, error) {
	origin, err := ParseOrigin(opts.Origin)
	if err != nil {
		return nil, err
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctl := &Controller{origin: origin, handlers: opts.Handlers, registry: NewRegistry(), refs: binding.New[struct{}](),
		log: log.With("component", "exposure-controller")}
	if host := strings.TrimSpace(opts.IngressProxyHost); host != "" {
		ctl.ingress = &ingressState{proxyHost: host, keys: opts.Keys, notify: opts.OnIngress, entries: map[string]ingressEntry{}}
	}
	return ctl, nil
}

// Register adds the Exposure controller to m. It watches Service objects so an
// Exposure created before its Service (or whose Service starts later) is
// reconciled again.
func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if err := m.AddToScheme(exposurev1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl, err := New(opts)
	if err != nil {
		return nil, err
	}
	options := []ctlruntime.Option{
		ctlruntime.Export(Export),
		ctlruntime.Gone(ctl.gone),
		ctlruntime.Watches(func(_ context.Context, src ctlruntime.Request, obj *exposurev1alpha1.Service) []ctlruntime.Request {
			return ctl.users(binding.Key(src.Workspace, ServiceKind, obj.Name))
		}),
	}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*exposurev1alpha1.Exposure](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("exposure: registering the Exposure controller: %w", err)
	}
	return ctl, nil
}

// Registry returns the route registry for the wasm bridge.
func (c *Controller) Registry() *Registry { return c.registry }

// Lookup returns the handler for a page-origin request.
func (c *Controller) Lookup(host, path string) (http.Handler, bool) {
	return c.registry.Lookup(host, path)
}

// HandlerFor is Lookup under the name the bridge uses.
func (c *Controller) HandlerFor(host, path string) (http.Handler, bool) {
	return c.registry.Lookup(host, path)
}

// ServeHTTP dispatches by request host and path.
func (c *Controller) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	c.registry.ServeHTTP(w, req)
}

func (c *Controller) users(ref string) []ctlruntime.Request {
	var out []ctlruntime.Request
	for _, t := range c.refs.Users(ref) {
		out = append(out, ctlruntime.Request{Cluster: t.Cluster, Workspace: t.Workspace, Name: t.Name})
	}
	return out
}

func (c *Controller) gone(_ context.Context, req ctlruntime.Request) {
	c.remove(ownerOf(req))
	c.refs.Set(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}, nil)
}

// Reconcile writes the status compose reads and, when a bus handler is
// available, installs the in-process route. It never opens a listener.
func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *exposurev1alpha1.Exposure) (ctlruntime.Result, error) {
	self := binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}
	if !object.DeletionTimestamp.IsZero() {
		c.remove(ownerOf(req))
		c.refs.Set(self, nil)
		return ctlruntime.Result{}, nil
	}
	spec := object.Spec
	serviceName := strs.LastSegment(spec.ServiceRef)
	var refs []string
	if serviceName != "" {
		refs = append(refs, binding.Key(req.Workspace, ServiceKind, serviceName))
	}
	c.refs.Set(self, &struct{}{}, refs...)
	var svc *exposurev1alpha1.Service
	if serviceName != "" {
		var found exposurev1alpha1.Service
		switch err := req.Get(ctx, "", serviceName, &found); {
		case err == nil:
			svc = &found
		case apierrors.IsNotFound(err):
		default:
			return ctlruntime.Result{}, err
		}
	}
	switch {
	case spec.Suspend:
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposurePending, "Suspended", "the Exposure is suspended", false)
	case spec.Mode != exposurev1alpha1.ExposureModeLocal && spec.Mode != exposurev1alpha1.ExposureModeIngress:
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposureFailed, "InvalidMode", fmt.Sprintf("unknown mode %q", spec.Mode), false)
	case serviceName == "":
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposurePending, "ServiceMissing", "the Exposure has no serviceRef", false)
	case svc == nil:
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposurePending, "ServiceMissing",
			"the Service "+serviceName+" is not present in workspace "+req.Workspace, true)
	case svc.Spec.Suspend:
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposurePending, "ServiceSuspended",
			"the Service "+serviceName+" is suspended", true)
	}
	target, err := targetOf(svc.Spec.TargetRef, req.Workspace)
	if err != nil {
		return c.withdraw(ctx, req, object, exposurev1alpha1.ExposureFailed, "InvalidTarget", err.Error(), false)
	}
	owner := ownerOf(req)
	host, base := c.origin.Host, c.origin.URL
	if spec.Mode == exposurev1alpha1.ExposureModeIngress && c.ingress != nil {
		in, err := c.ingress.register(ctx, owner, req.Workspace, req.Name, spec)
		if err != nil {
			return c.withdraw(ctx, req, object, exposurev1alpha1.ExposureFailed, "IngressIdentity", err.Error(), true)
		}
		host, base = in.Host, in.URL
	} else if c.ingress != nil {
		c.ingress.drop(owner)
	}
	if c.handlers != nil && isHTTP(svc.Spec.Protocol) {
		_, ok := c.handlers.Handler(target, svc.Spec.Handler)
		if !ok {
			message := fmt.Sprintf("no handler %q is registered for %s/%s in workspace %s", handlerName(svc.Spec.Handler),
				target.Kind, target.Name, target.Workspace)
			return c.withdraw(ctx, req, object, exposurev1alpha1.ExposurePending, "HandlerMissing", message, true)
		}
		if err := c.registry.Register(owner, host, spec.Path, withCaller(c.current(target, svc.Spec.Handler), target)); err != nil {
			return ctlruntime.Result{}, err
		}
		c.log.Debug("the in-browser Exposure route is registered", "host", host, "path", spec.Path,
			"target", target.Kind+"/"+target.Name)
	} else {
		c.registry.Remove(owner)
	}
	next := object.DeepCopy()
	next.Status = Status(object.Generation, base, spec.Path)
	return ctlruntime.Result{}, req.ApplyStatus(ctx, next)
}

func (c *Controller) withdraw(ctx context.Context, req ctlruntime.Request, object *exposurev1alpha1.Exposure,
	phase exposurev1alpha1.ExposurePhase, reason, message string, retry bool) (ctlruntime.Result, error) {
	c.remove(ownerOf(req))
	next := object.DeepCopy()
	next.Status = NotReady(object.Generation, phase, reason, message)
	res := ctlruntime.Result{}
	if retry {
		res.RequeueAfter = RequeuePending
	}
	return res, req.ApplyStatus(ctx, next)
}

func (c *Controller) remove(owner string) {
	c.registry.Remove(owner)
	if c.ingress != nil {
		c.ingress.drop(owner)
	}
}

func ownerOf(req ctlruntime.Request) string { return req.Workspace + "/" + req.Name }

func targetOf(ref commonv1.ObjectRef, workspace string) (bus.Key, error) {
	kind := strings.TrimSpace(ref.Kind)
	name := strings.TrimSpace(ref.Name)
	if kind == "" {
		return bus.Key{}, errors.New("the targetRef has no kind")
	}
	if name == "" {
		return bus.Key{}, errors.New("the targetRef has no name")
	}
	targetWorkspace := strings.TrimSpace(ref.Workspace)
	if targetWorkspace == "" {
		targetWorkspace = workspace
	}
	return bus.Key{Workspace: targetWorkspace, Kind: kind, Name: name}, nil
}

func handlerName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return bus.DefaultHandlerName
	}
	return value
}

func isHTTP(protocol exposurev1alpha1.ServiceProtocol) bool {
	switch exposurev1alpha1.ServiceProtocol(strings.ToLower(strings.TrimSpace(string(protocol)))) {
	case exposurev1alpha1.ServiceProtocolHTTP, exposurev1alpha1.ServiceProtocolWS:
		return true
	}
	return false
}

// withCaller mirrors the real local publisher: bus-authorized calls made by the
// handler are attributed to the target component.
// current resolves the target's handler on every request. A component that
// restarts registers a new handler under the same key; capturing the one seen
// at reconcile time would keep answering for the stopped instance.
func (c *Controller) current(target bus.Key, name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler, ok := c.handlers.Handler(target, name)
		if !ok {
			http.Error(w, "the exposed component has no handler registered", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	})
}

func withCaller(handler http.Handler, target bus.Key) http.Handler {
	ref := target.Ref()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(bus.WithCaller(r.Context(), ref)))
	})
}
