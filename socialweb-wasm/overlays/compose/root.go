package compose

import (
	"context"
	"sort"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
)

const (
	ExportApplications     = "applications"
	ExportPolicy           = "policy"
	ExportExposures        = "exposures"
	ExportHostProcesses    = "hostprocesses"
	ExportMarketIdentities = "marketidentities"
	ExportComputeVMs       = "computevms"
	ExportComputeContracts = "computecontracts"
	ExportEvents           = "events"
	ExportPDS              = "pds"
)

type Root struct {
	APIVersion string

	Kind string

	Name string

	UID types.UID

	Generation int64

	Workspace string

	Cluster string

	Deleting bool

	Suspend bool
}

func RootOf(req ctlruntime.Request, kind string, obj ctlruntime.Object, suspend bool) Root {
	return Root{APIVersion: v1alpha1.GroupVersion.String(), Kind: kind, Name: obj.GetName(), UID: obj.GetUID(),
		Generation: obj.GetGeneration(), Workspace: req.Workspace, Cluster: req.Cluster,
		Deleting: !obj.GetDeletionTimestamp().IsZero(), Suspend: suspend}
}

func OwnerName(kind, name string) string {
	return strings.ToLower(kind) + "." + name
}

func (r Root) Owner() string { return OwnerName(r.Kind, r.Name) }

func (r Root) Manager() string { return ctlruntime.FieldManagerPrefix + strings.ToLower(r.Kind) }

func (r Root) AdoptManager() string { return r.Manager() + "/adopt" }

func (r Root) IdentityManager() string { return r.Manager() + "/identity" }

func (r Root) ManagedBy() string { return "socialweb.computer." + strings.ToLower(r.Kind) }

func (r Root) Ref() commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: r.Kind, Name: r.Name, Workspace: r.Workspace}
}

func (r Root) OwnerRef() metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: r.APIVersion, Kind: r.Kind, Name: r.Name, UID: r.UID, Controller: new(true)}
}

func (r Root) Labels(componentKind string) map[string]string {
	out := map[string]string{
		OwnerLabel:     r.Owner(),
		RootKindLabel:  r.Kind,
		ManagedByLabel: r.ManagedBy(),
	}
	if componentKind != "" {
		out[ComponentLabel] = componentKind
	}
	if r.Cluster != "" {
		out[RootClusterLabel] = r.Cluster
	}
	return out
}

type rootIndex struct {
	mu sync.Mutex

	byCluster map[string]map[string]map[string]bool

	deleting map[types.UID]Root
}

func (c *Composer) Observe(r Root) {
	c.roots.mu.Lock()
	defer c.roots.mu.Unlock()
	kinds := c.roots.byCluster[r.Cluster]
	if kinds == nil {
		kinds = map[string]map[string]bool{}
		c.roots.byCluster[r.Cluster] = kinds
	}
	if kinds[r.Kind] == nil {
		kinds[r.Kind] = map[string]bool{}
	}
	kinds[r.Kind][r.Name] = true
	if r.Deleting && r.UID != "" {
		if c.roots.deleting == nil {
			c.roots.deleting = map[types.UID]Root{}
		}
		c.roots.deleting[r.UID] = r
	} else {
		delete(c.roots.deleting, r.UID)
	}
}

func (c *Composer) Drop(r Root) {
	c.roots.mu.Lock()
	defer c.roots.mu.Unlock()
	if kinds := c.roots.byCluster[r.Cluster]; kinds != nil && kinds[r.Kind] != nil {
		delete(kinds[r.Kind], r.Name)
	}
	delete(c.roots.deleting, r.UID)
	c.mu.Lock()
	delete(c.applied, string(r.UID))
	c.mu.Unlock()
}

func (c *Composer) rootsIn(cluster, kind string) []string {
	c.roots.mu.Lock()
	defer c.roots.mu.Unlock()
	var out []string
	for name := range c.roots.byCluster[cluster][kind] {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (c *Composer) deletingIn(rootKind, workspace string) []ctlruntime.Request {
	c.roots.mu.Lock()
	defer c.roots.mu.Unlock()
	var out []ctlruntime.Request
	for _, r := range c.roots.deleting {
		if r.Kind == rootKind && (r.Workspace == workspace || r.Workspace+":"+ComponentWorkspace == workspace) {
			out = append(out, ctlruntime.Request{Cluster: r.Cluster, Name: r.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cluster != out[j].Cluster {
			return out[i].Cluster < out[j].Cluster
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func DrainWatch[C ctlruntime.Object](c *Composer, export, rootKind string) ctlruntime.Option {
	return ctlruntime.WatchesIn(export, func(_ context.Context, src ctlruntime.Request, _ C) []ctlruntime.Request {
		return c.deletingIn(rootKind, src.Workspace)
	})
}

func (c *Composer) Mine(req ctlruntime.Request) bool {
	return strings.TrimSpace(req.Workspace) == c.Workspace()
}

func mapByLabel(rootKind string, src ctlruntime.Request, labels map[string]string) []ctlruntime.Request {
	if labels[RootKindLabel] != rootKind {
		return nil
	}
	owner := labels[OwnerLabel]
	prefix := strings.ToLower(rootKind) + "."
	name, ok := strings.CutPrefix(owner, prefix)
	if !ok || name == "" {
		return nil
	}
	cluster := src.Cluster
	if lc := strings.TrimSpace(labels[RootClusterLabel]); lc != "" {
		cluster = lc
	}
	return []ctlruntime.Request{{Cluster: cluster, Name: name}}
}

func childWatch[C ctlruntime.Object](export, rootKind string) ctlruntime.Option {
	return ctlruntime.WatchesIn(export, func(_ context.Context, src ctlruntime.Request, obj C) []ctlruntime.Request {
		return mapByLabel(rootKind, src, obj.GetLabels())
	})
}

func intentWatch[C ctlruntime.Object](c *Composer, export, rootKind string) ctlruntime.Option {
	return ctlruntime.WatchesIn(export, func(_ context.Context, src ctlruntime.Request, obj C) []ctlruntime.Request {
		if out := mapByLabel(rootKind, src, obj.GetLabels()); len(out) > 0 {
			return out
		}
		if len(obj.GetOwnerReferences()) > 0 || !obj.GetDeletionTimestamp().IsZero() {
			return nil
		}
		var out []ctlruntime.Request
		for _, name := range c.rootsIn(src.Cluster, rootKind) {
			out = append(out, ctlruntime.Request{Cluster: src.Cluster, Name: name})
		}
		return out
	})
}

func (c *Composer) Watches(rootKind string) []ctlruntime.Option {
	return []ctlruntime.Option{
		childWatch[*v1alpha1.Bidder]("", rootKind),
		childWatch[*v1alpha1.GuestControlPlane]("", rootKind),
		childWatch[*v1alpha1.Pds]("", rootKind),
		childWatch[*v1alpha1.Requester]("", rootKind),
		childWatch[*v1alpha1.GatewayAPI]("", rootKind),
		childWatch[*v1alpha1.SshServer]("", rootKind),
		childWatch[*v1alpha1.Plc]("", rootKind),
		childWatch[*v1alpha1.Relay]("", rootKind),
		childWatch[*v1alpha1.IngressProxy]("", rootKind),
		childWatch[*v1alpha1.DispatcherTLS]("", rootKind),
		childWatch[*v1alpha1.LocalRepo]("", rootKind),
		childWatch[*v1alpha1.RemoteRepo]("", rootKind),
		childWatch[*v1alpha1.AtprotoRepo]("", rootKind),
		intentWatch[*v1alpha1.MarketPresence](c, "", rootKind),
		intentWatch[*v1alpha1.StateSync](c, "", rootKind),
		childWatch[*policyv1alpha1.PolicyEngine](ExportPolicy, rootKind),
		childWatch[*exposurev1alpha1.Service](ExportExposures, rootKind),
		childWatch[*exposurev1alpha1.Exposure](ExportExposures, rootKind),
		childWatch[*exposurev1alpha1.ComponentLink](ExportExposures, rootKind),
		childWatch[*v1alpha1.OpenBaoServer](ExportHostProcesses, rootKind),
		childWatch[*v1alpha1.Vault](ExportHostProcesses, rootKind),
		childWatch[*v1alpha1.MarketIdentity](ExportMarketIdentities, rootKind),
		childWatch[*v1alpha1.AtprotoRecordWatch](ExportMarketIdentities, rootKind),
		intentWatch[*v1alpha1.Bid](c, ExportComputeVMs, rootKind),
		intentWatch[*v1alpha1.ComputeContract](c, ExportComputeContracts, rootKind),
		childWatch[*eventsv1alpha1.EventStream](ExportEvents, rootKind),
		childWatch[*eventsv1alpha1.EventSource](ExportEvents, rootKind),
		childWatch[*eventsv1alpha1.PdsEventSource](ExportEvents, rootKind),
		childWatch[*eventsv1alpha1.RelayEventSource](ExportEvents, rootKind),
		childWatch[*eventsv1alpha1.JetstreamEventSource](ExportEvents, rootKind),
		childWatch[*pdsv1alpha1.RemotePDS](ExportPDS, rootKind),
	}
}
