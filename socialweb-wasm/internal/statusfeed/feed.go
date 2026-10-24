// Package statusfeed watches the socialweb CRs in the browser kcp and pushes a
// compact state snapshot to the page. It replaces the page's kubectl pollers:
// the page only needs current phases and endpoints, not a generic watch API.
package statusfeed

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const debounce = 100 * time.Millisecond

var crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// PendingAction is the subset of MarketIdentity.status.pendingAction the page
// needs to open the authorization popup.
type PendingAction struct {
	Kind      string `json:"kind,omitempty"`
	URL       string `json:"url,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// Resource is one watched object reduced to status fields.
type Resource struct {
	Name     string         `json:"name"`
	Phase    string         `json:"phase,omitempty"`
	Message  string         `json:"message,omitempty"`
	Ready    bool           `json:"ready,omitempty"`
	DID      string         `json:"did,omitempty"`
	Handle   string         `json:"handle,omitempty"`
	Address  string         `json:"address,omitempty"`
	Endpoint string         `json:"endpoint,omitempty"`
	Pending  *PendingAction `json:"pendingAction,omitempty"`
}

// Contract is the compute-contract projection rendered by the page.
type Contract struct {
	Name      string `json:"name"`
	Phase     string `json:"phase,omitempty"`
	Message   string `json:"message,omitempty"`
	ChosenDID string `json:"chosenDid,omitempty"`
	Bids      int    `json:"bids,omitempty"`

	SecretsFetchedAt string    `json:"secretsFetchedAt,omitempty"`
	Release          bool      `json:"release,omitempty"`
	Networks         []Network `json:"networks,omitempty"`
	// HostKeys are status.contract.hostKeys as authorized_keys lines, the
	// pins for the page's WebRTC SSH client.
	HostKeys []string `json:"hostKeys,omitempty"`
}

// Network is one tagged guest address from status.contract.networks (plan
// 0032), e.g. the opentunnel URL of a tunnel; kind webrtc (plan 0034) makes
// the address the guest agent's DTLS fingerprint.
type Network struct {
	Tag     string `json:"tag"`
	Kind    string `json:"kind,omitempty"`
	Address string `json:"address"`
}

// ContractEvent is a plan 0034 guest event: a publicKey or a sealed
// signalWebRTC. Only public material: keys and age ciphertext.
type ContractEvent struct {
	Name       string `json:"name"`
	Contract   string `json:"contract"`
	Direction  string `json:"direction,omitempty"`
	NSID       string `json:"nsid"`
	Kind       string `json:"kind,omitempty"`
	Tag        string `json:"tag,omitempty"`
	Key        string `json:"key,omitempty"`
	Ciphertext string `json:"ciphertext,omitempty"`
	Phase      string `json:"phase,omitempty"`
	CreatedAt  string `json:"createdAt,omitempty"`
}

// State is a full, idempotent UI snapshot.
type State struct {
	Seq             int64           `json:"seq"`
	TS              int64           `json:"ts,omitempty"`
	MarketRequester *Resource       `json:"marketRequester,omitempty"`
	MarketIdentity  *Resource       `json:"marketIdentity,omitempty"`
	Requester       *Resource       `json:"requester,omitempty"`
	Exposures       []Resource      `json:"exposures,omitempty"`
	Contracts       []Contract      `json:"contracts,omitempty"`
	ContractEvents  []ContractEvent `json:"contractEvents,omitempty"`
	CRDs            []string        `json:"crds,omitempty"`
}

// Feed watches all browser-relevant resources and calls Publish with each
// changed snapshot. It is safe for concurrent use.
type Feed struct {
	client dynamic.Interface

	informers map[schema.GroupVersionResource]cache.SharedIndexInformer

	mu      sync.Mutex
	timer   *time.Timer
	last    string
	seq     int64
	publish func([]byte)
	started bool
}

// Start begins the informers and waits for their initial cache sync. publish is
// called from a timer goroutine and must not block.
func Start(ctx context.Context, cfg *rest.Config, publish func([]byte)) error {
	if cfg == nil {
		return errors.New("statusfeed: a rest config is required")
	}
	if publish == nil {
		return errors.New("statusfeed: a publish function is required")
	}
	base := rest.CopyConfig(cfg)
	base.Host = strings.TrimSuffix(base.Host, "/") + "/clusters/root"
	client, err := dynamic.NewForConfig(base)
	if err != nil {
		return err
	}
	f := &Feed{client: client, publish: publish, informers: map[schema.GroupVersionResource]cache.SharedIndexInformer{}}
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, 0, metav1.NamespaceAll, nil)
	watch := func(gvr schema.GroupVersionResource) error {
		informer := factory.ForResource(gvr).Informer()
		_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(any) { f.changed() },
			UpdateFunc: func(any, any) { f.changed() },
			DeleteFunc: func(any) { f.changed() },
		})
		if err != nil {
			return err
		}
		f.mu.Lock()
		f.informers[gvr] = informer
		f.mu.Unlock()
		return nil
	}
	for _, gvr := range []schema.GroupVersionResource{
		v1alpha1.MarketRequesterGVR,
		v1alpha1.MarketIdentityGVR,
		v1alpha1.RequesterGVR,
		v1alpha1.ComputeContractGVR,
		exposurev1alpha1.ExposureGVR,
		crdGVR,
	} {
		if err := watch(gvr); err != nil {
			return err
		}
	}
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	// ContractEvent is newer than some served manifests: it starts after the
	// initial sync and is never waited on, so a kcp without the API still
	// boots; its reflector lists again with backoff until the API appears.
	if err := watch(v1alpha1.ContractEventGVR); err != nil {
		return err
	}
	factory.Start(ctx.Done())
	f.changed()
	return nil
}

func (f *Feed) changed() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.timer == nil {
		f.timer = time.AfterFunc(debounce, f.flush)
		return
	}
	f.timer.Reset(debounce)
}

func (f *Feed) flush() {
	state := f.build()
	state.Seq, state.TS = 0, 0
	raw, err := json.Marshal(state)
	if err != nil {
		return
	}
	f.mu.Lock()
	if string(raw) == f.last {
		f.mu.Unlock()
		return
	}
	f.last = string(raw)
	f.seq++
	state.Seq, state.TS = f.seq, time.Now().UnixMilli()
	f.mu.Unlock()
	if out, err := json.Marshal(state); err == nil {
		f.publish(out)
	}
}

func (f *Feed) objects(gvr schema.GroupVersionResource) []*unstructured.Unstructured {
	f.mu.Lock()
	informer, ok := f.informers[gvr]
	f.mu.Unlock()
	if !ok || informer.GetStore() == nil {
		return nil
	}
	items := informer.GetStore().List()
	out := make([]*unstructured.Unstructured, 0, len(items))
	for _, item := range items {
		if u, ok := item.(*unstructured.Unstructured); ok {
			out = append(out, u)
		}
	}
	return out
}

func (f *Feed) build() State {
	var state State
	state.MarketRequester = first(f.objects(v1alpha1.MarketRequesterGVR), requesterResource)
	state.MarketIdentity = first(f.objects(v1alpha1.MarketIdentityGVR), identityResource)
	state.Requester = first(f.objects(v1alpha1.RequesterGVR), requesterResource)
	for _, u := range f.objects(exposurev1alpha1.ExposureGVR) {
		state.Exposures = append(state.Exposures, *requesterResource(u))
	}
	sort.Slice(state.Exposures, func(i, j int) bool { return state.Exposures[i].Name < state.Exposures[j].Name })
	for _, u := range f.objects(v1alpha1.ComputeContractGVR) {
		state.Contracts = append(state.Contracts, contract(u))
	}
	sort.Slice(state.Contracts, func(i, j int) bool { return state.Contracts[i].Name < state.Contracts[j].Name })
	for _, u := range f.objects(v1alpha1.ContractEventGVR) {
		state.ContractEvents = append(state.ContractEvents, contractEvent(u))
	}
	sort.Slice(state.ContractEvents, func(i, j int) bool { return state.ContractEvents[i].Name < state.ContractEvents[j].Name })
	for _, u := range f.objects(crdGVR) {
		state.CRDs = append(state.CRDs, u.GetName())
	}
	sort.Strings(state.CRDs)
	return state
}

func first(items []*unstructured.Unstructured, conv func(*unstructured.Unstructured) *Resource) *Resource {
	if len(items) == 0 {
		return nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].GetName() < items[j].GetName() })
	return conv(items[0])
}

func requesterResource(u *unstructured.Unstructured) *Resource {
	r := &Resource{Name: u.GetName()}
	r.Phase, _, _ = unstructured.NestedString(u.Object, "status", "phase")
	r.Message, _, _ = unstructured.NestedString(u.Object, "status", "message")
	r.Ready, _, _ = unstructured.NestedBool(u.Object, "status", "ready")
	r.DID, _, _ = unstructured.NestedString(u.Object, "status", "did")
	r.Handle, _, _ = unstructured.NestedString(u.Object, "status", "handle")
	r.Address, _, _ = unstructured.NestedString(u.Object, "status", "address")
	r.Endpoint, _, _ = unstructured.NestedString(u.Object, "status", "endpoint")
	if action, ok, _ := unstructured.NestedMap(u.Object, "status", "pendingAction"); ok {
		r.Pending = &PendingAction{}
		r.Pending.Kind, _, _ = unstructured.NestedString(action, "kind")
		r.Pending.URL, _, _ = unstructured.NestedString(action, "url")
		r.Pending.ExpiresAt, _, _ = unstructured.NestedString(action, "expiresAt")
	}
	return r
}

func contract(u *unstructured.Unstructured) Contract {
	c := Contract{Name: u.GetName()}
	c.Phase, _, _ = unstructured.NestedString(u.Object, "status", "phase")
	c.Message, _, _ = unstructured.NestedString(u.Object, "status", "message")
	c.ChosenDID, _, _ = unstructured.NestedString(u.Object, "status", "chosen", "did")
	if bids, ok, _ := unstructured.NestedSlice(u.Object, "status", "bids"); ok {
		c.Bids = len(bids)
	}
	c.SecretsFetchedAt, _, _ = unstructured.NestedString(u.Object, "status", "secretsFetchedAt")
	c.Release, _, _ = unstructured.NestedBool(u.Object, "spec", "release")
	networks, _, _ := unstructured.NestedSlice(u.Object, "status", "contract", "networks")
	for _, raw := range networks {
		n, _ := raw.(map[string]any)
		tag, _ := n["tag"].(string)
		kind, _ := n["kind"].(string)
		address, _ := n["address"].(string)
		if tag != "" && address != "" {
			c.Networks = append(c.Networks, Network{Tag: tag, Kind: kind, Address: address})
		}
	}
	keys, _, _ := unstructured.NestedSlice(u.Object, "status", "contract", "hostKeys", "keys")
	for _, raw := range keys {
		k, _ := raw.(map[string]any)
		typ, _ := k["type"].(string)
		pub, _ := k["publicKey"].(string)
		if typ != "" && pub != "" {
			c.HostKeys = append(c.HostKeys, typ+" "+pub)
		}
	}
	return c
}

func contractEvent(u *unstructured.Unstructured) ContractEvent {
	e := ContractEvent{Name: u.GetName(), CreatedAt: u.GetCreationTimestamp().UTC().Format(time.RFC3339)}
	e.Contract, _, _ = unstructured.NestedString(u.Object, "spec", "contractRef", "name")
	e.Direction, _, _ = unstructured.NestedString(u.Object, "spec", "direction")
	e.NSID, _, _ = unstructured.NestedString(u.Object, "spec", "nsid")
	e.Kind, _, _ = unstructured.NestedString(u.Object, "spec", "kind")
	e.Tag, _, _ = unstructured.NestedString(u.Object, "spec", "tag")
	e.Key, _, _ = unstructured.NestedString(u.Object, "spec", "key")
	e.Phase, _, _ = unstructured.NestedString(u.Object, "status", "phase")
	// Only inbound ciphertext is for the page; outbound ones are its own.
	if e.Direction == "inbound" {
		e.Ciphertext, _, _ = unstructured.NestedString(u.Object, "spec", "ciphertext")
	}
	return e
}

// identityResource is requesterResource plus the identity-only status fields.
func identityResource(u *unstructured.Unstructured) *Resource {
	r := requesterResource(u)
	if r.DID == "" {
		r.DID, _, _ = unstructured.NestedString(u.Object, "status", "did")
	}
	if r.Handle == "" {
		r.Handle, _, _ = unstructured.NestedString(u.Object, "status", "handle")
	}
	return r
}
