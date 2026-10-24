package vaulthost

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	contractvault "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	bvault "github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/vault"
)

func TestServerStatus(t *testing.T) {
	object := &v1alpha1.OpenBaoServer{ObjectMeta: metav1.ObjectMeta{Name: "bao", Generation: 7}}
	st := ServerStatus(object)
	if st.ObservedGeneration != 7 || !st.Ready || st.Phase != v1alpha1.OpenBaoServerPhaseReady {
		t.Fatalf("status = %#v, want observedGeneration 7, Ready", st)
	}
	if !st.Initialized || st.Sealed || st.Address != Address || st.Version != Version {
		t.Fatalf("status = %#v, want initialized, unsealed, %q, %q", st, Address, Version)
	}
	if st.Refs == nil || st.Refs.KVMount != DefaultKVMount {
		t.Fatalf("refs = %#v, want kvMount %q", st.Refs, DefaultKVMount)
	}
	cond := meta.FindStatusCondition(st.Conditions, commonv1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.ObservedGeneration != 7 {
		t.Fatalf("Ready condition = %#v", cond)
	}
	if len(object.Status.Conditions) != 0 {
		t.Fatalf("ServerStatus mutated the object: %#v", object.Status)
	}
}

func TestVaultStatus(t *testing.T) {
	server := &v1alpha1.OpenBaoServer{ObjectMeta: metav1.ObjectMeta{Name: "bao", Generation: 2}}
	server.Status = ServerStatus(server)
	vault := &v1alpha1.Vault{ObjectMeta: metav1.ObjectMeta{Name: "app", Generation: 5}}

	st := VaultStatus(vault, server)
	if st.ObservedGeneration != 5 || !st.Ready || st.Address != Address || !st.Initialized || st.Sealed {
		t.Fatalf("status = %#v, want Ready copy of the backend", st)
	}
	for _, kind := range []string{v1alpha1.VaultConditionBackendBound, v1alpha1.VaultConditionReady} {
		cond := meta.FindStatusCondition(st.Conditions, kind)
		if cond == nil || cond.Status != metav1.ConditionTrue || cond.ObservedGeneration != 5 {
			t.Fatalf("%s condition = %#v", kind, cond)
		}
	}

	stale := server.DeepCopy()
	stale.Status.ObservedGeneration = 1
	st = VaultStatus(vault, stale)
	if st.Ready {
		t.Fatal("a stale backend still reports the Vault Ready")
	}
	if cond := meta.FindStatusCondition(st.Conditions, v1alpha1.VaultConditionReady); cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("Ready condition = %#v, want False", cond)
	}

	st = VaultStatus(vault, nil)
	if st.Ready || st.Address != "" {
		t.Fatalf("unbound status = %#v", st)
	}
	for _, kind := range []string{v1alpha1.VaultConditionBackendBound, v1alpha1.VaultConditionReady} {
		if cond := meta.FindStatusCondition(st.Conditions, kind); cond == nil || cond.Status != metav1.ConditionFalse {
			t.Fatalf("%s condition = %#v, want False", kind, cond)
		}
	}
}

func TestMountOf(t *testing.T) {
	if got := MountOf(nil); got != DefaultKVMount {
		t.Fatalf("MountOf(nil) = %q", got)
	}
	server := &v1alpha1.OpenBaoServer{}
	if got := MountOf(server); got != DefaultKVMount {
		t.Fatalf("MountOf(no refs) = %q", got)
	}
	server.Status.Refs = &v1alpha1.OpenBaoServerRefs{KVMount: "/apps/"}
	if got := MountOf(server); got != "apps" {
		t.Fatalf("MountOf = %q, want apps", got)
	}
}

func TestProviderCapabilities(t *testing.T) {
	ctx := context.Background()
	ref := commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: "Vault", Name: "app", Workspace: "root:org:ws"}
	provider := NewProvider()
	b := bus.New(ctx)
	t.Cleanup(func() { _ = b.Stop() })
	provider.Register(b)
	if !provider.Bind(Binding{Vault: ref}) {
		t.Fatal("Bind reported no change")
	}
	if provider.Bind(Binding{Vault: ref}) {
		t.Fatal("binding the same mount reported a change")
	}

	write := resolve(t, b, ref, contractvault.CapabilityWrite, DefaultKVMount+"/apps/*")
	if err := write.Write(ctx, "", "apps/alice/session", map[string]any{"token": "abc"}); err != nil {
		t.Fatalf("Write through the default mount: %v", err)
	}
	read := resolve(t, b, ref, contractvault.CapabilityRead, DefaultKVMount+"/apps/*")
	got, err := read.Read(ctx, DefaultKVMount, "apps/alice/session", 0)
	if err != nil || got["token"] != "abc" {
		t.Fatalf("Read = %#v, %v", got, err)
	}
	if _, err := read.Read(ctx, DefaultKVMount, DefaultKVMount+"/data/apps/alice/session", 0); err != nil {
		t.Fatalf("Read with the data prefix: %v", err)
	}
	if err := read.Write(ctx, DefaultKVMount, "apps/alice/session", nil); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only Write error = %v, want ErrReadOnly", err)
	}
	if err := read.Delete(ctx, DefaultKVMount, "apps/alice/session"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only Delete error = %v, want ErrReadOnly", err)
	}
	if _, err := read.Read(ctx, DefaultKVMount, "other/key", 0); !errors.Is(err, ErrOutOfScope) {
		t.Fatalf("out-of-scope Read error = %v, want ErrOutOfScope", err)
	}
	if err := write.Delete(ctx, "", "apps/alice/session"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := read.Read(ctx, "", "apps/alice/session", 0); !errors.Is(err, bvault.ErrNotFound) {
		t.Fatalf("Read after Delete error = %v, want bvault.ErrNotFound", err)
	}

	provider.Unbind(ref)
	if _, err := bus.Resolve[contractvault.KV](ctx, b, ref, ref,
		bus.Capability{Name: contractvault.CapabilityRead, Scope: []string{"*"}}); !errors.Is(err, ErrUnbound) {
		t.Fatalf("Resolve after Unbind error = %v, want ErrUnbound", err)
	}
}

func TestProviderSnapshotRestore(t *testing.T) {
	ctx := context.Background()
	provider := NewProvider()
	store := provider.Store("ws")
	if err := store.Write(ctx, DefaultKVMount, "a", map[string]any{"v": "one"}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Store("other").Write(ctx, DefaultKVMount, "b", map[string]any{"v": "two"}); err != nil {
		t.Fatal(err)
	}
	snapshot := provider.Snapshot()
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded Snapshot
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if err := store.Write(ctx, DefaultKVMount, "c", map[string]any{"v": "three"}); err != nil {
		t.Fatal(err)
	}
	extra := provider.Store("extra")
	if err := extra.Write(ctx, DefaultKVMount, "d", map[string]any{"v": "four"}); err != nil {
		t.Fatal(err)
	}
	provider.Restore(decoded)

	if provider.Store("ws") != store {
		t.Fatal("Restore replaced a store the orchestrator already holds")
	}
	if data, err := store.Read(ctx, DefaultKVMount, "a", 0); err != nil || data["v"] != "one" {
		t.Fatalf("restored a = %#v, %v", data, err)
	}
	if _, err := store.Read(ctx, DefaultKVMount, "c", 0); !errors.Is(err, bvault.ErrNotFound) {
		t.Fatalf("c survived Restore: %v", err)
	}
	if _, err := extra.Read(ctx, DefaultKVMount, "d", 0); !errors.Is(err, bvault.ErrNotFound) {
		t.Fatalf("a workspace missing from the snapshot survived Restore: %v", err)
	}
	if data, err := provider.Store("other").Read(ctx, DefaultKVMount, "b", 0); err != nil || data["v"] != "two" {
		t.Fatalf("restored other/b = %#v, %v", data, err)
	}
}

func resolve(t *testing.T, b *bus.Bus, ref commonv1.ObjectRef, capability string, scope ...string) contractvault.KV {
	t.Helper()
	ctx := bus.WithCaller(context.Background(), ref)
	kv, err := bus.Resolve[contractvault.KV](ctx, b, ref, ref, bus.Capability{Name: capability, Scope: scope})
	if err != nil {
		t.Fatalf("Resolve %s: %v", capability, err)
	}
	return kv
}
