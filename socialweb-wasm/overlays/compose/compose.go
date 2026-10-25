package compose

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

const (
	ManifestSet        = "applications"
	Finalizer          = "apps.socialweb.computer/components"
	ComponentWorkspace = "app"
	DefaultWorkspace   = "root"
	ApplyTimeout       = time.Minute
	ComponentsDir      = "components"
	AppsDir            = "apps"
)

type Config struct {
	Kubeconfig string

	Workspace string

	Layout devinstance.Layout

	Manager hostsvc.Manager

	Record func() (devinstance.Record, error)

	ReadyTimeout time.Duration

	Listeners func(owner string) []v1alpha1.ApplicationListener

	OnComponents func(owner string, listeners []v1alpha1.ApplicationListener)

	StoreUsage func() *v1alpha1.StoreUsage

	TTLReport func() *v1alpha1.TTLStatus

	Kube *kube.Client

	// Rest is an in-process client config for the browser. It replaces the
	// kubeconfig file used by the native binary.
	Rest *rest.Config

	Bus *bus.Bus

	RemotePDS *pdsv1alpha1.RemotePDS

	Log *slog.Logger
}

type Composer struct {
	cfg Config

	restCfg *rest.Config

	log *slog.Logger

	mu sync.Mutex

	clients map[string]*kube.Client

	applied map[string]int64

	forgotten map[string]bool

	samples samples

	adoptions adoptions

	parking parking

	roots rootIndex

	drift map[string]*driftState

	trusts Trusts

	requesterSets sync.Map

	requesters requesterIndex

	applies applyCache

	linkSets linkSets
}

func New(cfg Config) (*Composer, error) {
	var restCfg *rest.Config
	if cfg.Kube == nil {
		if cfg.Rest != nil {
			restCfg = rest.CopyConfig(cfg.Rest)
		} else {
			var err error
			restCfg, err = kcpserver.FrontRestConfig(cfg.Kubeconfig)
			if err != nil {
				return nil, err
			}
		}
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Composer{cfg: cfg, restCfg: restCfg, log: log.With("component", "app-compose"), clients: map[string]*kube.Client{},
		applied: map[string]int64{}, forgotten: map[string]bool{}, roots: rootIndex{byCluster: map[string]map[string]map[string]bool{}}}, nil
}

func (c *Composer) Config() Config { return c.cfg }

func (c *Composer) Log() *slog.Logger { return c.log }

func (c *Composer) Bus() *bus.Bus { return c.cfg.Bus }

func (c *Composer) ProvideTrust(gk schema.GroupKind) { c.trusts.Provide(c.cfg.Bus, gk) }

func (c *Composer) PublishTrust(ref commonv1.ObjectRef, bundle []byte) { c.trusts.Publish(ref, bundle) }

func (c *Composer) Workspace() string {
	return cmp.Or(strings.TrimSpace(c.cfg.Workspace), DefaultWorkspace)
}

func (c *Composer) StateRoot() string { return c.cfg.Layout.StateDir }

func (c *Composer) RestConfig() *rest.Config { return c.restCfg }

func (c *Composer) KcpServer() string {
	if c.restCfg == nil {
		return ""
	}
	server, _, _ := strings.Cut(c.restCfg.Host, "/clusters/")
	return server
}

func (c *Composer) Kube(ws string) (*kube.Client, error) {
	if c.cfg.Kube != nil {
		return c.cfg.Kube, nil
	}
	ws = cmp.Or(strings.TrimSpace(ws), c.Workspace())
	c.mu.Lock()
	kc, ok := c.clients[ws]
	c.mu.Unlock()
	if !ok {
		var err error
		kc, err = kube.ForWorkspace(c.restCfg, ws)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.clients[ws] = kc
		c.mu.Unlock()
	}
	return kc, nil
}

func (c *Composer) Client(ws string, gvr schema.GroupVersionResource) (kube.Resource, error) {
	kc, err := c.Kube(ws)
	if err != nil {
		return kube.Resource{}, err
	}
	return kc.Resource(gvr), nil
}

func (c *Composer) Applied(r Root) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.applied[string(r.UID)] == r.Generation
}

func (c *Composer) MarkApplied(r Root) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applied[string(r.UID)] = r.Generation
	delete(c.forgotten, string(r.UID))
}

func (c *Composer) AppliedBefore(r Root, recorded *v1alpha1.AppliedSet, digest string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.forgotten[string(r.UID)] && recorded != nil && digest != "" &&
		recorded.Generation == r.Generation && recorded.Digest == digest
}

func (c *Composer) Forget(r Root) {
	c.mu.Lock()
	delete(c.applied, string(r.UID))
	c.forgotten[string(r.UID)] = true
	c.mu.Unlock()
	c.adoptions.forget(r)
}

func ComponentStateDir(instanceState, component string) string {
	return filepath.Join(instanceState, ComponentsDir, component)
}

func ComponentCRStateDir(instanceState, kind, name string) string {
	return filepath.Join(instanceState, ComponentsDir, kind+"-"+name)
}

func failf(format string, args ...any) error {
	return fmt.Errorf("compose: "+format, args...)
}

type contextKey struct{}

func WithRoot(ctx context.Context, r Root) context.Context {
	return context.WithValue(ctx, contextKey{}, r)
}

func RootFrom(ctx context.Context) (Root, bool) {
	r, ok := ctx.Value(contextKey{}).(Root)
	return r, ok
}
