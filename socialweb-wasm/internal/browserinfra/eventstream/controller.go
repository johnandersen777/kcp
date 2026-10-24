// Package eventstream is the browser substitute for the EventStream,
// EventSource, and source-kind controllers. It opens no firehose, relay, or
// PDS connection: the backend kinds turn Ready as soon as their spec parses,
// EventSources mirror them, and EventStreams report Ready so the rest of the
// in-browser app can proceed. Poller supplies the market events that would
// otherwise arrive through a watched EventStream.
package eventstream

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
)

const (
	// StreamControllerName and the source controller names are unique on the
	// manager; the real tree's names are already taken by real controllers.
	StreamControllerName = "browser-eventstream"

	SourceControllerName = "browser-eventsource"

	PdsControllerName = "browser-pdseventsource"

	RelayControllerName = "browser-relayeventsource"

	JetstreamControllerName = "browser-jetstreameventsource"
)

// Options scopes the controllers to a set of workspaces. A nil Scope keeps the
// manager's workspaces.
type Options struct {
	Scope func(workspace string) bool
}

// Controller indexes EventStreams by EventSource and EventSources by backend so
// a status change on either side requeues its dependants.
type Controller struct {
	mu sync.Mutex

	streamSource map[string]string

	streamsBySource map[string]map[string]ctlruntime.Request

	sourceBackend map[string]string

	sourcesByBackend map[string]map[string]ctlruntime.Request
}

// Register registers the five controllers on m. None of them dial anything.
func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	c := &Controller{
		streamSource:     map[string]string{},
		streamsBySource:  map[string]map[string]ctlruntime.Request{},
		sourceBackend:    map[string]string{},
		sourcesByBackend: map[string]map[string]ctlruntime.Request{},
	}
	streamOptions := controllerOptions(opts.Scope,
		ctlruntime.Watches[*eventsv1.EventSource](func(_ context.Context, source ctlruntime.Request, _ *eventsv1.EventSource) []ctlruntime.Request {
			return c.streamsOf(source)
		}))
	if err := ctlruntime.For[*eventsv1.EventStream](m, StreamControllerName, c, streamOptions...); err != nil {
		return nil, fmt.Errorf("eventstream: registering the EventStream controller: %w", err)
	}
	sourceOptions := controllerOptions(opts.Scope,
		ctlruntime.Watches[*eventsv1.PdsEventSource](func(_ context.Context, source ctlruntime.Request, _ *eventsv1.PdsEventSource) []ctlruntime.Request {
			return c.sourcesOf(source, eventsv1.PdsEventSourceKind)
		}),
		ctlruntime.Watches[*eventsv1.RelayEventSource](func(_ context.Context, source ctlruntime.Request, _ *eventsv1.RelayEventSource) []ctlruntime.Request {
			return c.sourcesOf(source, eventsv1.RelayEventSourceKind)
		}),
		ctlruntime.Watches[*eventsv1.JetstreamEventSource](func(_ context.Context, source ctlruntime.Request, _ *eventsv1.JetstreamEventSource) []ctlruntime.Request {
			return c.sourcesOf(source, eventsv1.JetstreamEventSourceKind)
		}))
	if err := ctlruntime.For[*eventsv1.EventSource](m, SourceControllerName, sourceReconciler{c: c}, sourceOptions...); err != nil {
		return nil, fmt.Errorf("eventstream: registering the EventSource controller: %w", err)
	}
	if err := ctlruntime.For[*eventsv1.PdsEventSource](m, PdsControllerName, pdsReconciler{}, controllerOptions(opts.Scope)...); err != nil {
		return nil, fmt.Errorf("eventstream: registering the PdsEventSource controller: %w", err)
	}
	if err := ctlruntime.For[*eventsv1.RelayEventSource](m, RelayControllerName, relayReconciler{}, controllerOptions(opts.Scope)...); err != nil {
		return nil, fmt.Errorf("eventstream: registering the RelayEventSource controller: %w", err)
	}
	if err := ctlruntime.For[*eventsv1.JetstreamEventSource](m, JetstreamControllerName, jetstreamReconciler{}, controllerOptions(opts.Scope)...); err != nil {
		return nil, fmt.Errorf("eventstream: registering the JetstreamEventSource controller: %w", err)
	}
	return c, nil
}

func controllerOptions(scope func(string) bool, extra ...ctlruntime.Option) []ctlruntime.Option {
	out := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.InWorkspaces(scope)}
	return append(out, extra...)
}

func requestKey(workspace, name string) string {
	return strings.TrimSpace(workspace) + "/" + strings.TrimSpace(name)
}

func backendKey(workspace, kind, name string) string {
	return requestKey(workspace, "") + strings.ToLower(strings.TrimSpace(kind)) + "/" + strings.TrimSpace(name)
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, stream *eventsv1.EventStream) (ctlruntime.Result, error) {
	key := requestKey(req.Workspace, stream.Name)
	if !stream.DeletionTimestamp.IsZero() {
		c.indexStream(key, req, commonv1.ObjectRef{})
		return ctlruntime.Result{}, nil
	}
	sourceRef := commonv1.ObjectRef{Group: eventsv1.GroupName, Kind: eventsv1.EventSourceKind,
		Name: strings.TrimSpace(stream.Spec.SourceRef.Name), Workspace: stream.Spec.SourceRef.InWorkspace(req.Workspace)}
	c.indexStream(key, req, sourceRef)
	var live *eventsv1.EventSource
	if sourceRef.Name != "" && sourceRef.Workspace == req.Workspace {
		found := &eventsv1.EventSource{}
		err := req.Get(ctx, "", sourceRef.Name, found)
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return ctlruntime.Result{}, fmt.Errorf("eventstream: reading %s: %w", sourceRef.String(), err)
		default:
			live = found
		}
	}
	next := stream.DeepCopy()
	next.Status = EventStreamStatus(stream, req.Workspace, live)
	return ctlruntime.Result{}, req.ApplyStatus(ctx, next)
}

func (c *Controller) indexStream(streamKey string, req ctlruntime.Request, source commonv1.ObjectRef) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if prior, ok := c.streamSource[streamKey]; ok {
		delete(c.streamsBySource[prior], streamKey)
		if len(c.streamsBySource[prior]) == 0 {
			delete(c.streamsBySource, prior)
		}
		delete(c.streamSource, streamKey)
	}
	if source.Name == "" {
		return
	}
	sourceKey := requestKey(source.Workspace, source.Name)
	if c.streamsBySource[sourceKey] == nil {
		c.streamsBySource[sourceKey] = map[string]ctlruntime.Request{}
	}
	c.streamsBySource[sourceKey][streamKey] = ctlruntime.Request{Cluster: req.Cluster, Namespace: req.Namespace, Name: req.Name}
	c.streamSource[streamKey] = sourceKey
}

func (c *Controller) streamsOf(source ctlruntime.Request) []ctlruntime.Request {
	key := requestKey(source.Workspace, source.Name)
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ctlruntime.Request, 0, len(c.streamsBySource[key]))
	for _, req := range c.streamsBySource[key] {
		out = append(out, req)
	}
	return out
}

type sourceReconciler struct {
	c *Controller
}

func (r sourceReconciler) Reconcile(ctx context.Context, req ctlruntime.Request, source *eventsv1.EventSource) (ctlruntime.Result, error) {
	key := requestKey(req.Workspace, source.Name)
	if !source.DeletionTimestamp.IsZero() {
		r.c.indexSource(key, req, commonv1.ObjectRef{})
		return ctlruntime.Result{}, nil
	}
	ref, refErr := BackendRef(req.Workspace, source.Spec.BackendRef)
	if refErr != nil {
		r.c.indexSource(key, req, commonv1.ObjectRef{})
	} else {
		r.c.indexSource(key, req, ref)
	}
	var backendStatus *eventsv1.EventSourceStatus
	var backendGeneration int64
	if refErr == nil && ref.Name != "" {
		obj, statusOf := newBackend(ref.Kind)
		if obj != nil {
			err := req.Get(ctx, "", ref.Name, obj)
			switch {
			case apierrors.IsNotFound(err):
			case err != nil:
				return ctlruntime.Result{}, fmt.Errorf("eventstream: reading %s: %w", ref.String(), err)
			default:
				backendStatus, backendGeneration = statusOf()
			}
		}
	}
	next := source.DeepCopy()
	next.Status = EventSourceStatus(source, req.Workspace, backendStatus, backendGeneration)
	return ctlruntime.Result{}, req.ApplyStatus(ctx, next)
}

func newBackend(kind string) (ctlruntime.Object, func() (*eventsv1.EventSourceStatus, int64)) {
	switch kind {
	case eventsv1.PdsEventSourceKind:
		obj := &eventsv1.PdsEventSource{}
		return obj, func() (*eventsv1.EventSourceStatus, int64) { return &obj.Status, obj.Generation }
	case eventsv1.RelayEventSourceKind:
		obj := &eventsv1.RelayEventSource{}
		return obj, func() (*eventsv1.EventSourceStatus, int64) { return &obj.Status, obj.Generation }
	case eventsv1.JetstreamEventSourceKind:
		obj := &eventsv1.JetstreamEventSource{}
		return obj, func() (*eventsv1.EventSourceStatus, int64) { return &obj.Status, obj.Generation }
	}
	return nil, nil
}

func (c *Controller) indexSource(sourceKey string, req ctlruntime.Request, backend commonv1.ObjectRef) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if prior, ok := c.sourceBackend[sourceKey]; ok {
		delete(c.sourcesByBackend[prior], sourceKey)
		if len(c.sourcesByBackend[prior]) == 0 {
			delete(c.sourcesByBackend, prior)
		}
		delete(c.sourceBackend, sourceKey)
	}
	if backend.Kind == "" || backend.Name == "" {
		return
	}
	key := backendKey(backend.Workspace, backend.Kind, backend.Name)
	if c.sourcesByBackend[key] == nil {
		c.sourcesByBackend[key] = map[string]ctlruntime.Request{}
	}
	c.sourcesByBackend[key][sourceKey] = ctlruntime.Request{Cluster: req.Cluster, Namespace: req.Namespace, Name: req.Name}
	c.sourceBackend[sourceKey] = key
}

func (c *Controller) sourcesOf(source ctlruntime.Request, kind string) []ctlruntime.Request {
	key := backendKey(source.Workspace, kind, source.Name)
	c.mu.Lock()
	out := make([]ctlruntime.Request, 0, len(c.sourcesByBackend[key])+1)
	for _, req := range c.sourcesByBackend[key] {
		out = append(out, req)
	}
	c.mu.Unlock()
	// The real controller always includes the source itself: an EventSource may
	// share its backend's name.
	self := ctlruntime.Request{Cluster: source.Cluster, Namespace: source.Namespace, Name: source.Name}
	if !slices.ContainsFunc(out, func(req ctlruntime.Request) bool { return req.Name == self.Name }) {
		out = append(out, self)
	}
	return out
}

type pdsReconciler struct{}

func (pdsReconciler) Reconcile(ctx context.Context, req ctlruntime.Request, source *eventsv1.PdsEventSource) (ctlruntime.Result, error) {
	if !source.DeletionTimestamp.IsZero() {
		return ctlruntime.Result{}, nil
	}
	next := source.DeepCopy()
	next.Status = PdsSourceStatus(source, req.Workspace)
	return ctlruntime.Result{}, req.ApplyStatus(ctx, next)
}

type relayReconciler struct{}

func (relayReconciler) Reconcile(ctx context.Context, req ctlruntime.Request, source *eventsv1.RelayEventSource) (ctlruntime.Result, error) {
	if !source.DeletionTimestamp.IsZero() {
		return ctlruntime.Result{}, nil
	}
	next := source.DeepCopy()
	next.Status = RelaySourceStatus(source)
	return ctlruntime.Result{}, req.ApplyStatus(ctx, next)
}

type jetstreamReconciler struct{}

func (jetstreamReconciler) Reconcile(ctx context.Context, req ctlruntime.Request, source *eventsv1.JetstreamEventSource) (ctlruntime.Result, error) {
	if !source.DeletionTimestamp.IsZero() {
		return ctlruntime.Result{}, nil
	}
	next := source.DeepCopy()
	next.Status = JetstreamSourceStatus(source)
	return ctlruntime.Result{}, req.ApplyStatus(ctx, next)
}
