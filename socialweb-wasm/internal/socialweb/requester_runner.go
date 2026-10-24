package socialweb

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/types"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/oauthcallback"
	servicesrequester "github.com/johnandersen777/socialweb-computer-kcp/internal/services/requester"
)

// RequesterRunner starts the real requester service for each Requester object
// the MarketRequester app composes. It is a small replacement for
// componentcore that keeps the browser process free of listeners and ports.
type RequesterRunner struct {
	runtime *component.Runtime

	log *slog.Logger
}

type RequesterRunnerOptions struct {
	Bus *bus.Bus

	OAuthCallbacks *oauthcallback.Registry

	StateDir string

	Scope func(workspace string) bool

	Log *slog.Logger
}

func RegisterRequesterRunner(m *ctlruntime.Manager, opts RequesterRunnerOptions) (*RequesterRunner, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	registry := component.NewRegistry()
	rt := component.NewRuntime(component.RuntimeConfig{
		Log: log, StateDir: opts.StateDir, Registry: registry, Bus: opts.Bus,
	}, nil)
	if err := rt.Register(servicesrequester.Kind, component.Typed(func(ctx context.Context, in component.Instance, spec v1alpha1.RequesterSpec) (*servicesrequester.Service, error) {
		return servicesrequester.New(ctx, spec, servicesrequester.Deps{
			Instance:       in,
			Egress:         component.Egress{Log: log},
			OAuthCallbacks: opts.OAuthCallbacks,
		})
	})); err != nil {
		return nil, err
	}
	// Reconcile refuses to start a service before Start; the manager owns the
	// process lifetime, so the runtime lives for the whole worker.
	if err := rt.Start(context.Background()); err != nil {
		return nil, err
	}
	runner := &RequesterRunner{runtime: rt, log: log}
	options := []ctlruntime.Option{
		ctlruntime.Export(compose.ExportApplications),
		ctlruntime.FieldManager("socialweb.computer/browser-requester"),
		ctlruntime.SharedStatus(),
	}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*v1alpha1.Requester](m, "browser-requester", runner, options...); err != nil {
		return nil, fmt.Errorf("requester runner: %w", err)
	}
	return runner, nil
}

func (r *RequesterRunner) Reconcile(ctx context.Context, req ctlruntime.Request, obj *v1alpha1.Requester) (ctlruntime.Result, error) {
	raw, err := json.Marshal(obj.Spec)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	spec := component.ComponentSpec{
		UID:           types.UID(obj.UID),
		Group:         v1alpha1.GroupName,
		Kind:          servicesrequester.Kind,
		Name:          obj.Name,
		Workspace:     req.Workspace,
		Generation:    obj.Generation,
		Suspend:       obj.Spec.Suspend,
		Deleting:      !obj.DeletionTimestamp.IsZero(),
		StateDir:      obj.Spec.StateDir,
		Spec:        raw,
		Annotations: obj.Annotations,
		Labels:      obj.Labels,
	}
	status, err := r.runtime.Reconcile(ctx, spec)
	next := obj.DeepCopy()
	next.Status = status.APIStatus()
	if applyErr := req.ApplyStatus(ctx, next); applyErr != nil {
		return ctlruntime.Result{}, applyErr
	}
	if err != nil {
		return ctlruntime.Result{}, err
	}
	return ctlruntime.Result{}, nil
}

func (r *RequesterRunner) Remove(ctx context.Context, uid types.UID) error {
	return r.runtime.Remove(ctx, uid)
}
