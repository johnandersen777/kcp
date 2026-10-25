package socialweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketrequester"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
	reporemote "github.com/johnandersen777/socialweb-computer-kcp/internal/atprotorepo/remote"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	componentlink "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/componentlink"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/contractevent"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/infra"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/marketidentity"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/marketintents"
	policyenginectl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/policyengine"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/repos"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kcpapply"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/manifests"
	contractexposure "github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/marketidentity/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/oauthcallback"
	pecontroller "github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/controller"

	"github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/eventstream"
	"github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/exposure"
	bvault "github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/vaulthost"
)

type Config struct {
	Rest *rest.Config

	Bus *bus.Bus

	StateDir string

	ProviderWorkspace string

	Origin string

	IdentityMode v1alpha1.MarketIdentityMode

	Handle string

	DID string

	PDSURL string

	OAuthClientID string

	OAuthRedirectURI string

	// ForceApply re-applies every provider manifest set even when the snapshot
	// already holds ready bindings. Dev loops leave it off; schema changes and
	// first boots turn it on.
	ForceApply bool

	// Restore is the socialweb vault snapshot a previous worker persisted.
	Restore json.RawMessage

	// OnSnapshot receives a fresh vault snapshot after every write or delete.
	OnSnapshot func([]byte)

	// IngressProxyHost is the xrpc-subscription relay ingress-mode Exposures
	// register with (e.g. xrpc.fedproxy.com). Empty serves them on Origin.
	IngressProxyHost string

	// OnIngress receives the ingress Exposures the page must subscribe for.
	OnIngress func([]exposure.Ingress)

	Log *slog.Logger
}

type Orchestrator struct {
	Manager *ctlruntime.Manager

	Composer *compose.Composer

	Vault *vaulthost.Controller

	Exposure *exposure.Controller

	Events *eventstream.Controller

	Identity *marketidentity.Controller

	cancel context.CancelFunc
	done   chan struct{}
}

func Start(parent context.Context, cfg Config) (*Orchestrator, error) {
	if cfg.Rest == nil {
		return nil, errors.New("socialweb: a kcp rest config is required")
	}
	if cfg.Bus == nil {
		return nil, errors.New("socialweb: a bus is required")
	}
	if cfg.StateDir == "" {
		cfg.StateDir = "/kcp/socialweb"
	}
	if cfg.ProviderWorkspace == "" {
		cfg.ProviderWorkspace = "root:socialweb-provider"
	}
	if cfg.Origin == "" {
		cfg.Origin = "http://127.0.0.1:8090"
	}
	if cfg.IdentityMode == "" {
		cfg.IdentityMode = v1alpha1.MarketIdentityModeOAuthBrowser
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}

	// The loopback user is privileged and bypasses workspace CreatedBy
	// admission. Use a non-privileged cluster-admin identity so workspaces get
	// spec.createdBy and APIBindings can initialize.
	impersonated := rest.CopyConfig(cfg.Rest)
	impersonated.Impersonate = rest.ImpersonationConfig{
		UserName: "system:kcp:admin",
		Groups:   []string{"system:kcp:admin"},
	}
	cfg.Rest = impersonated

	if !cfg.ForceApply && providerReady(parent, cfg.Rest) {
		fmt.Fprintln(os.Stderr, "socialweb: provider manifests already applied; skipping")
	} else {
		fmt.Fprintln(os.Stderr, "socialweb: applying provider manifests")
		applyDone := make(chan error, 1)
		go func() { applyDone <- applyProviderSets(parent, cfg, log) }()
		select {
		case err := <-applyDone:
			if err != nil {
				fmt.Fprintln(os.Stderr, "socialweb: provider manifests:", err)
				return nil, err
			}
		case <-time.After(10 * time.Minute):
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			fmt.Fprintln(os.Stderr, "socialweb: provider manifest apply stalled\n"+string(buf[:n]))
			return nil, errors.New("socialweb: provider manifest apply stalled")
		}
		fmt.Fprintln(os.Stderr, "socialweb: provider manifests applied")
	}

	composer, err := compose.New(compose.Config{
		Rest:      cfg.Rest,
		Workspace: "root",
		Bus:       cfg.Bus,
		Log:       log,
	})
	if err != nil {
		return nil, fmt.Errorf("socialweb: composer: %w", err)
	}

	mgr, err := ctlruntime.NewManager(ctlruntime.Config{
		Rest:            cfg.Rest,
		ExportWorkspace: cfg.ProviderWorkspace,
		AddToScheme: []func(*kruntime.Scheme) error{
			v1alpha1.AddToScheme,
			exposurev1alpha1.AddToScheme,
			eventsv1alpha1.AddToScheme,
			policyv1alpha1.AddToScheme,
			pdsv1alpha1.AddToScheme,
		},
		Workspaces: func(ws string) bool { return ws == "root" || strings.HasPrefix(ws, "root:") },
		Log:        log,
	})
	if err != nil {
		return nil, fmt.Errorf("socialweb: manager: %w", err)
	}
	scope := func(ws string) bool { return ws == "root" || strings.HasPrefix(ws, "root:") }

	vaultCtl, err := vaulthost.Register(mgr, cfg.Bus, vaulthost.Options{Scope: scope})
	if err != nil {
		return nil, err
	}
	// Restore before any controller reads the vault: MarketIdentity restores
	// its OAuth session from it on the first reconcile.
	if len(cfg.Restore) > 0 {
		snapshot, err := ParseSnapshot(cfg.Restore)
		if err != nil {
			log.Warn("ignoring the restored socialweb snapshot", "err", err.Error())
		} else {
			vaultCtl.Restore(snapshot.Vault)
			fmt.Fprintln(os.Stderr, "socialweb: restored browser vault")
		}
	}
	if cfg.OnSnapshot != nil {
		vaultCtl.Provider().SetOnChange(func() {
			raw, err := json.Marshal(Snapshot{Version: SnapshotVersion, Vault: vaultCtl.Snapshot()})
			if err != nil {
				log.Warn("encoding the socialweb snapshot", "err", err.Error())
				return
			}
			cfg.OnSnapshot(raw)
		})
	}
	exposureCtl, err := exposure.Register(mgr, exposure.Options{
		Origin: cfg.Origin, Handlers: cfg.Bus, Scope: scope, Log: log,
		IngressProxyHost: cfg.IngressProxyHost, Keys: vaultCtl.Provider().Store, OnIngress: cfg.OnIngress,
	})
	if err != nil {
		return nil, err
	}
	eventsCtl, err := eventstream.Register(mgr, eventstream.Options{Scope: scope})
	if err != nil {
		return nil, err
	}

	callbacks := oauthcallback.NewRegistry()
	identityCtl, err := marketidentity.Register(mgr, marketidentity.Options{
		Scope:      scope,
		Vault:      func(ws string) atproto.SecretStore { return bvault.NewSecrets(vaultCtl.Provider().Store(ws)) },
		Bus:        cfg.Bus,
		Log:        log,
		HTTPClient: http.DefaultClient,
		OAuth: atproto.OAuthOptions{
			ClientID:    cfg.OAuthClientID,
			RedirectURI: cfg.OAuthRedirectURI,
			Scopes:      oauth.RequesterOAuthScope,
			// Pending PKCE states live in the vault so a reload during the
			// OAuth dance still completes the callback.
			StateRef:   &commonv1.SecretRef{Mount: "requester", Path: "oauth/requester"},
			Directory:  BrowserDirectory(),
			HTTPClient: http.DefaultClient,
		},
	})
	if err != nil {
		return nil, err
	}
	callbacks.Register("requester", func(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
		return identityCtl.CompleteBrowserAuthorization(ctx, "root", "requester", r)
	})

	// The repo controllers provide repo.write and the record readers the
	// requester signs its market records with (AtprotoRepo bound to the
	// account's RemoteRepo). The browser has no local PDS and reads secrets
	// from its own vault.
	if err := repos.Register(mgr, repos.Options{
		Bus: cfg.Bus,
		Log: log,
		Dynamic: func(workspace string) (*kube.Client, error) {
			return kube.ForWorkspace(cfg.Rest, workspace)
		},
		Network: reporemote.NewNetwork(reporemote.NetworkConfig{Directory: BrowserDirectory(), HTTP: http.DefaultClient}),
		Pds:     func(string, string) (*atpds.Server, bool) { return nil, false },
		Secrets: func(ctx context.Context, workspace string, ref commonv1.SecretRef) (map[string]any, error) {
			return bvault.NewSecrets(vaultCtl.Provider().Store(workspace)).Get(ctx, ref)
		},
		Scope: scope,
	}); err != nil {
		return nil, fmt.Errorf("socialweb: repo controllers: %w", err)
	}

	if _, err := componentlink.Register(mgr, componentlink.Options{
		Policy:    contractexposure.NewLinkPolicy(cfg.Bus),
		Registry:  component.NewRegistry(),
		Providers: cfg.Bus,
		Scope:     scope,
		Log:       log,
	}); err != nil {
		return nil, err
	}
	engines := pecontroller.NewEngineManager(pecontroller.NewDefaultEngineFactory(pecontroller.Config{
		DenoPath:       "wasm-unavailable",
		DisableActions: true,
		Log: func(level, msg string, meta map[string]any) {
			log.Info(msg, "level", level)
		},
	}))
	if _, err := policyenginectl.Register(mgr, policyenginectl.Options{
		Bus: cfg.Bus, Engines: engines, Log: log,
	}); err != nil {
		return nil, err
	}
	if _, err := marketintents.Register(mgr, marketintents.Options{
		Bus:     cfg.Bus,
		Scope:   scope,
		Log:     log,
		Clients: workspaceClients(cfg.Rest),
	}); err != nil {
		return nil, err
	}
	// The requester resolves its market.contractEvents handle (WebRTC
	// signaling, guest events) from this controller and exits without it.
	if _, err := contractevent.Register(mgr, contractevent.Options{
		Bus:     cfg.Bus,
		Scope:   scope,
		Log:     log,
		Clients: workspaceClients(cfg.Rest),
	}); err != nil {
		return nil, err
	}
	// One EphemeralInfra per page session owns that session's ComputeContracts.
	if _, err := infra.Register(mgr, infra.Options{
		Bus:     cfg.Bus,
		Scope:   scope,
		Log:     log,
		Dynamic: func(workspace string) (*kube.Client, error) { return kube.ForWorkspace(cfg.Rest, workspace) },
	}); err != nil {
		return nil, err
	}
	if err := marketrequester.Register(mgr, composer); err != nil {
		return nil, err
	}
	if _, err := RegisterRequesterRunner(mgr, RequesterRunnerOptions{
		Bus:            cfg.Bus,
		OAuthCallbacks: callbacks,
		StateDir:       cfg.StateDir,
		Scope:          scope,
		Log:            log,
	}); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := mgr.Start(ctx); err != nil && ctx.Err() == nil {
			log.Error("socialweb manager exited", "err", err)
		}
	}()

	orc := &Orchestrator{
		Manager: mgr, Composer: composer, Vault: vaultCtl, Exposure: exposureCtl,
		Events: eventsCtl, Identity: identityCtl, cancel: cancel, done: done,
	}
	fmt.Fprintln(os.Stderr, "socialweb: manager started; creating MarketRequester")
	if err := orc.createRoot(ctx, cfg); err != nil {
		_ = orc.Close()
		return nil, err
	}
	return orc, nil
}

// providerReady reports whether a restored snapshot already holds the provider
// exports and root bindings every set installs. That turns a restart with
// persisted state into a ~40s boot instead of a ~2.5 minute apply pass.
func providerReady(ctx context.Context, restCfg *rest.Config) bool {
	kc, err := kube.ForWorkspace(restCfg, "root")
	if err != nil {
		return false
	}
	items, err := kube.List[map[string]any](ctx, kc.Resource(schema.GroupVersionResource{
		Group: "apis.kcp.io", Version: "v1alpha2", Resource: "apibindings",
	}), metav1.ListOptions{})
	if err != nil {
		return false
	}
	ready := map[string]bool{}
	for _, item := range items {
		meta, _ := item["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if name != "" {
			ready[name] = bindingReady(item)
		}
	}
	for _, name := range []string{
		"applications", "computevms", "events", "exposures",
		"hostprocesses", "pds", "policy",
	} {
		if !ready[name] {
			return false
		}
	}
	return true
}

func bindingReady(item map[string]any) bool {
	status, _ := item["status"].(map[string]any)
	conditions, _ := status["conditions"].([]any)
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if condition["type"] == "Ready" && condition["status"] == "True" {
			return true
		}
	}
	return false
}

func applyProviderSets(ctx context.Context, cfg Config, log *slog.Logger) error {
	applier, err := kcpapply.New(cfg.Rest, kcpapply.Options{
		Base:         "root",
		FieldManager: "socialweb-wasm",
		WaitTimeout:  3 * time.Minute,
		Log:          log,
	})
	if err != nil {
		return fmt.Errorf("socialweb: manifest applier: %w", err)
	}
	ix, err := manifests.Load()
	if err != nil {
		return err
	}
	applySet := func(name string, base context.Context) error {
		set, ok := ix.Set(name)
		if !ok {
			return fmt.Errorf("socialweb: deploy/sets.yaml names no %s set", name)
		}
		fmt.Fprintf(os.Stderr, "socialweb: applying set %s\n", name)
		values, err := manifests.DecodeValues(set.Values, nil)
		if err != nil {
			return fmt.Errorf("socialweb: decoding values for %s: %w", name, err)
		}
		applyCtx, cancel := context.WithTimeout(base, 4*time.Minute)
		defer cancel()
		start := time.Now()
		rep, err := applier.Apply(applyCtx, set, values)
		if err != nil {
			return fmt.Errorf("socialweb: applying the %s set: %w", name, err)
		}
		fmt.Fprintf(os.Stderr, "socialweb: set %s applied in %s\n", name, time.Since(start).Round(time.Millisecond))
		for _, phase := range rep.Phases {
			for _, wait := range phase.Waits {
				fmt.Fprintf(os.Stderr, "socialweb: wait set=%s phase=%s kind=%s name=%s ws=%s met=%t seconds=%.2f\n",
					phase.Set, phase.Phase, wait.Kind, wait.Name, wait.Workspace, wait.Met, wait.Seconds)
			}
		}
		return nil
	}
	// Serial on purpose: the apiserver and all apply clients share the single
	// Go/wasm thread, so concurrent SSA overloads discovery and watch delivery.
	for _, name := range []string{"provider", "hostprocesses", "applications", "bidder"} {
		if err := applySet(name, ctx); err != nil {
			return err
		}
	}
	return nil
}

func workspaceClients(base *rest.Config) func(string) (ctrlclient.Client, error) {
	scheme := kruntime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	_ = exposurev1alpha1.AddToScheme(scheme)
	_ = eventsv1alpha1.AddToScheme(scheme)
	_ = policyv1alpha1.AddToScheme(scheme)
	_ = pdsv1alpha1.AddToScheme(scheme)
	return func(ws string) (ctrlclient.Client, error) {
		cfg := rest.CopyConfig(base)
		cfg.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + ws
		return ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
	}
}

func (o *Orchestrator) createRoot(ctx context.Context, cfg Config) error {
	kc, err := kube.ForWorkspace(cfg.Rest, "root")
	if err != nil {
		return err
	}
	obj := &v1alpha1.MarketRequester{
		Spec: v1alpha1.MarketRequesterSpec{
			Profile: marketrequester.RequesterProfileDefault,
			Identity: v1alpha1.ApplicationIdentity{
				Mode:        cfg.IdentityMode,
				Handle:      cfg.Handle,
				Did:         cfg.DID,
				PdsURL:      cfg.PDSURL,
				Association: v1alpha1.AssociationSkip,
			},
			Network: &v1alpha1.MarketNetworkValue{PLCDirectoryURL: "https://plc.directory"},
			// getSecrets over the ingress stays primary; when the guest cannot
			// reach it, the requester seals the bundle to the guest's key.
			SecretsFallback: v1alpha1.SecretsFallbackSealed,
		},
	}
	obj.APIVersion = v1alpha1.GroupVersion.String()
	obj.Kind = "MarketRequester"
	obj.Name = "app"
	// Never overwrite an existing app: the UI edits spec.identity at runtime
	// and the worker config may be empty on a reload with restored state.
	if _, err := kube.Get[*v1alpha1.MarketRequester](ctx, kc.Resource(v1alpha1.MarketRequesterGVR), "app"); err == nil {
		return nil
	}
	_, err = kube.Apply[*v1alpha1.MarketRequester](ctx, kc.Resource(v1alpha1.MarketRequesterGVR), obj,
		kube.ApplyOptions{FieldManager: "socialweb-wasm", Force: true})
	return err
}

func (o *Orchestrator) Close() error {
	if o == nil {
		return nil
	}
	if o.cancel != nil {
		o.cancel()
	}
	select {
	case <-o.done:
	case <-time.After(15 * time.Second):
	}
	return nil
}

// CompleteRequesterOAuth finishes the browser authorization before the
// Requester component exists. The app waits for its identity to sign in, so the
// callback cannot live only on the component's Exposure.
func (o *Orchestrator) CompleteRequesterOAuth(ctx context.Context, r *http.Request) error {
	if o == nil || o.Identity == nil {
		return errors.New("socialweb: the identity controller is not running")
	}
	return o.Identity.CompleteBrowserAuthorization(ctx, "root", "requester", r)
}

// Lookup routes a page-origin request to the registered Exposure handler.
func (o *Orchestrator) Lookup(host, path string) (http.Handler, bool) {
	if o == nil || o.Exposure == nil {
		return nil, false
	}
	return o.Exposure.Lookup(host, path)
}
