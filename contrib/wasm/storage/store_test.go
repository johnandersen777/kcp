/*
Copyright 2026 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	kstorage "k8s.io/apiserver/pkg/storage"

	"github.com/kcp-dev/logicalcluster/v3"

	fetchv1alpha1 "github.com/kcp-dev/kcp/contrib/wasm/apis/fetch/v1alpha1"
)

const prefix = "/fetches/"

func key(name string) string { return prefix + name }

func newFetch(name string) *fetchv1alpha1.Fetch {
	return &fetchv1alpha1.Fetch{
		TypeMeta:   metav1.TypeMeta{APIVersion: fetchv1alpha1.GroupVersion, Kind: fetchv1alpha1.Kind},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     fetchv1alpha1.FetchStatus{Phase: fetchv1alpha1.FetchPhaseUnresolved},
	}
}

func decodeFetch(raw []byte) (runtime.Object, error) {
	var f fetchv1alpha1.Fetch
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func expectEvent(t *testing.T, w watch.Interface, typ watch.EventType, name string) {
	t.Helper()
	select {
	case ev, ok := <-w.ResultChan():
		if !ok {
			t.Fatalf("watch closed while waiting for %s %s", typ, name)
		}
		if ev.Type != typ {
			t.Fatalf("event type = %s, want %s", ev.Type, typ)
		}
		accessor, err := meta.Accessor(ev.Object)
		if err != nil {
			t.Fatalf("accessor: %v", err)
		}
		if accessor.GetName() != name {
			t.Fatalf("event object = %q, want %q", accessor.GetName(), name)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s %s", typ, name)
	}
}

func TestCreateGetUpdateDelete(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer s.Close()

	in := newFetch("a")
	var created fetchv1alpha1.Fetch
	if err := s.Create(ctx, key("a"), in, &created, 0); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ResourceVersion != "2" {
		t.Fatalf("resourceVersion = %q, want 2", created.ResourceVersion)
	}
	if err := s.Create(ctx, key("a"), newFetch("a"), nil, 0); !kstorage.IsExist(err) {
		t.Fatalf("duplicate Create error = %v, want AlreadyExists", err)
	}

	var got fetchv1alpha1.Fetch
	if err := s.Get(ctx, key("a"), kstorage.GetOptions{}, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "a" || got.ResourceVersion != "2" {
		t.Fatalf("Get returned %+v", got.ObjectMeta)
	}

	var updated fetchv1alpha1.Fetch
	err := s.GuaranteedUpdate(ctx, key("a"), &updated, false, nil,
		func(input runtime.Object, res kstorage.ResponseMeta) (runtime.Object, *uint64, error) {
			f := input.(*fetchv1alpha1.Fetch).DeepCopy()
			f.Status.Phase = fetchv1alpha1.FetchPhaseFetching
			return f, nil, nil
		}, nil)
	if err != nil {
		t.Fatalf("GuaranteedUpdate: %v", err)
	}
	if updated.ResourceVersion != "3" {
		t.Fatalf("updated resourceVersion = %q, want 3", updated.ResourceVersion)
	}

	// Updating with identical content must not bump the resource version.
	var unchanged fetchv1alpha1.Fetch
	err = s.GuaranteedUpdate(ctx, key("a"), &unchanged, false, nil,
		func(input runtime.Object, res kstorage.ResponseMeta) (runtime.Object, *uint64, error) {
			return input.DeepCopyObject(), nil, nil
		}, nil)
	if err != nil {
		t.Fatalf("no-op GuaranteedUpdate: %v", err)
	}
	if unchanged.ResourceVersion != "3" {
		t.Fatalf("no-op update resourceVersion = %q, want 3", unchanged.ResourceVersion)
	}

	var deleted fetchv1alpha1.Fetch
	if err := s.Delete(ctx, key("a"), &deleted, nil, nil, nil, kstorage.DeleteOptions{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if deleted.Name != "a" {
		t.Fatalf("deleted object = %+v", deleted.ObjectMeta)
	}
	if err := s.Get(ctx, key("a"), kstorage.GetOptions{}, &got); !kstorage.IsNotFound(err) {
		t.Fatalf("Get after Delete = %v, want NotFound", err)
	}
}

func TestGetListPredicateAndPaging(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer s.Close()

	for _, name := range []string{"a", "b", "c"} {
		f := newFetch(name)
		if name != "b" {
			f.Labels = map[string]string{"app": "selected"}
		}
		if err := s.Create(ctx, key(name), f, nil, 0); err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
	}

	sel, err := labels.Parse("app=selected")
	if err != nil {
		t.Fatal(err)
	}

	var list fetchv1alpha1.FetchList
	if err := s.GetList(ctx, prefix, kstorage.ListOptions{
		Recursive: true,
		Predicate: kstorage.SelectionPredicate{Label: sel},
	}, &list); err != nil {
		t.Fatalf("GetList: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("selected items = %d, want 2 (%v)", len(list.Items), list.Items)
	}
	for _, item := range list.Items {
		if item.Name == "b" {
			t.Fatalf("label selector returned unselected object b")
		}
	}

	var page fetchv1alpha1.FetchList
	if err := s.GetList(ctx, prefix, kstorage.ListOptions{
		Recursive: true,
		Predicate: kstorage.SelectionPredicate{Limit: 2},
	}, &page); err != nil {
		t.Fatalf("GetList page 1: %v", err)
	}
	if len(page.Items) != 2 || page.Continue == "" {
		t.Fatalf("page 1 = %d items, continue %q", len(page.Items), page.Continue)
	}

	var page2 fetchv1alpha1.FetchList
	if err := s.GetList(ctx, prefix, kstorage.ListOptions{
		Recursive: true,
		Predicate: kstorage.SelectionPredicate{Limit: 2, Continue: page.Continue},
	}, &page2); err != nil {
		t.Fatalf("GetList page 2: %v", err)
	}
	if len(page2.Items) != 1 {
		t.Fatalf("page 2 = %d items, want 1", len(page2.Items))
	}
}

func TestWatchEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := New()
	defer s.Close()

	if err := s.Create(ctx, key("existing"), newFetch("existing"), nil, 0); err != nil {
		t.Fatal(err)
	}

	w, err := s.Watch(ctx, prefix, kstorage.ListOptions{
		Recursive:       true,
		ResourceVersion: "0",
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Stop()

	expectEvent(t, w, watch.Added, "existing")

	if err := s.Create(ctx, key("new"), newFetch("new"), nil, 0); err != nil {
		t.Fatal(err)
	}
	expectEvent(t, w, watch.Added, "new")

	err = s.GuaranteedUpdate(ctx, key("new"), &fetchv1alpha1.Fetch{}, false, nil,
		func(input runtime.Object, res kstorage.ResponseMeta) (runtime.Object, *uint64, error) {
			f := input.(*fetchv1alpha1.Fetch).DeepCopy()
			f.Status.Phase = fetchv1alpha1.FetchPhaseResolved
			return f, nil, nil
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	expectEvent(t, w, watch.Modified, "new")

	if err := s.Delete(ctx, key("new"), nil, nil, nil, nil, kstorage.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	expectEvent(t, w, watch.Deleted, "new")
}

func TestWatchReplaysFromResourceVersion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := New()
	defer s.Close()

	if err := s.Create(ctx, key("a"), newFetch("a"), nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, key("b"), newFetch("b"), nil, 0); err != nil {
		t.Fatal(err)
	}

	var list fetchv1alpha1.FetchList
	if err := s.GetList(ctx, prefix, kstorage.ListOptions{Recursive: true}, &list); err != nil {
		t.Fatal(err)
	}

	// Change b after the list; a watch from the list's RV must replay it.
	err := s.GuaranteedUpdate(ctx, key("b"), &fetchv1alpha1.Fetch{}, false, nil,
		func(input runtime.Object, res kstorage.ResponseMeta) (runtime.Object, *uint64, error) {
			f := input.(*fetchv1alpha1.Fetch).DeepCopy()
			f.Status.Phase = fetchv1alpha1.FetchPhaseResolved
			return f, nil, nil
		}, nil)
	if err != nil {
		t.Fatal(err)
	}

	w, err := s.Watch(ctx, prefix, kstorage.ListOptions{
		Recursive:       true,
		ResourceVersion: list.ResourceVersion,
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Stop()

	expectEvent(t, w, watch.Modified, "b")
}

func TestClusterAnnotations(t *testing.T) {
	s := New()
	defer s.Close()

	clusterCtx := func(name string) context.Context {
		return genericapirequest.WithCluster(context.Background(), genericapirequest.Cluster{
			Name: logicalcluster.Name(name),
		})
	}

	if err := s.Create(clusterCtx("root:org"), key("a"), newFetch("a"), nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(clusterCtx("root:other"), key("b"), newFetch("b"), nil, 0); err != nil {
		t.Fatal(err)
	}

	var got fetchv1alpha1.Fetch
	if err := s.Get(clusterCtx("root:org"), key("a"), kstorage.GetOptions{}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations[logicalcluster.AnnotationKey] != "root:org" {
		t.Errorf("Get annotation = %q, want root:org", got.Annotations[logicalcluster.AnnotationKey])
	}

	wildcard := genericapirequest.WithCluster(context.Background(), genericapirequest.Cluster{Wildcard: true})
	var list fetchv1alpha1.FetchList
	if err := s.GetList(wildcard, prefix, kstorage.ListOptions{Recursive: true}, &list); err != nil {
		t.Fatal(err)
	}
	clusters := map[string]string{}
	for _, item := range list.Items {
		clusters[item.Name] = item.Annotations[logicalcluster.AnnotationKey]
	}
	if clusters["a"] != "root:org" || clusters["b"] != "root:other" {
		t.Fatalf("wildcard list cluster annotations = %v", clusters)
	}

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	restored := New()
	defer restored.Close()
	if err := restored.Restore(snap, decodeFetch); err != nil {
		t.Fatal(err)
	}
	var restoredFetch fetchv1alpha1.Fetch
	if err := restored.Get(clusterCtx("root:other"), key("b"), kstorage.GetOptions{}, &restoredFetch); err != nil {
		t.Fatal(err)
	}
	if restoredFetch.Annotations[logicalcluster.AnnotationKey] != "root:other" {
		t.Errorf("restored annotation = %q, want root:other", restoredFetch.Annotations[logicalcluster.AnnotationKey])
	}
}

func TestWatchInitialEventsBookmark(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := New()
	defer s.Close()

	if err := s.Create(ctx, key("a"), newFetch("a"), nil, 0); err != nil {
		t.Fatal(err)
	}

	sendInitial := true
	w, err := s.watch(ctx, prefix, kstorage.ListOptions{
		Recursive:         true,
		SendInitialEvents: &sendInitial,
		Predicate:         kstorage.SelectionPredicate{AllowWatchBookmarks: true},
	}, func() runtime.Object { return newFetch("") }, func(ref *storedRef) (runtime.Object, error) { return annotatedCopy(ref.Object, ref.meta), nil })
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer w.Stop()

	expectEvent(t, w, watch.Added, "a")

	select {
	case ev, ok := <-w.ResultChan():
		if !ok {
			t.Fatal("watch closed before bookmark")
		}
		if ev.Type != watch.Bookmark {
			t.Fatalf("event type = %s, want BOOKMARK", ev.Type)
		}
		has, err := kstorage.HasInitialEventsEndBookmarkAnnotation(ev.Object)
		if err != nil {
			t.Fatalf("checking bookmark annotation: %v", err)
		}
		if !has {
			t.Fatal("bookmark is missing the initial-events-end annotation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial-events-end bookmark")
	}
}

func TestWatchTooLargeResourceVersion(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer s.Close()

	if err := s.Create(ctx, key("a"), newFetch("a"), nil, 0); err != nil {
		t.Fatal(err)
	}

	_, err := s.Watch(ctx, prefix, kstorage.ListOptions{
		Recursive:       true,
		ResourceVersion: "99",
	})
	if !kstorage.IsTooLargeResourceVersion(err) {
		t.Fatalf("Watch error = %v, want TooLargeResourceVersion", err)
	}
}

func TestPreconditions(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer s.Close()

	if err := s.Create(ctx, key("a"), newFetch("a"), nil, 0); err != nil {
		t.Fatal(err)
	}

	stale := "999"
	err := s.GuaranteedUpdate(ctx, key("a"), &fetchv1alpha1.Fetch{}, false,
		&kstorage.Preconditions{ResourceVersion: &stale},
		func(input runtime.Object, res kstorage.ResponseMeta) (runtime.Object, *uint64, error) {
			return input.DeepCopyObject(), nil, nil
		}, nil)
	if err == nil {
		t.Fatal("GuaranteedUpdate with stale resourceVersion succeeded")
	}

	wrongUID := types.UID("not-the-uid")
	err = s.Delete(ctx, key("a"), nil, &kstorage.Preconditions{UID: &wrongUID}, nil, nil, kstorage.DeleteOptions{})
	if err == nil {
		t.Fatal("Delete with wrong UID succeeded")
	}
}

func TestSnapshotRestore(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer s.Close()

	for _, name := range []string{"a", "b"} {
		if err := s.Create(ctx, key(name), newFetch(name), nil, 0); err != nil {
			t.Fatal(err)
		}
	}

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	restored := New()
	defer restored.Close()
	if err := restored.Restore(snap, decodeFetch); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	var list fetchv1alpha1.FetchList
	if err := restored.GetList(ctx, prefix, kstorage.ListOptions{Recursive: true}, &list); err != nil {
		t.Fatalf("GetList after restore: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("restored items = %d, want 2", len(list.Items))
	}
	if list.ResourceVersion != "3" {
		t.Fatalf("restored list RV = %q, want 3", list.ResourceVersion)
	}

	// New writes continue from the restored resource version.
	var out fetchv1alpha1.Fetch
	if err := restored.Create(ctx, key("c"), newFetch("c"), &out, 0); err != nil {
		t.Fatal(err)
	}
	if out.ResourceVersion != "4" {
		t.Fatalf("post-restore RV = %q, want 4", out.ResourceVersion)
	}
}

func TestSnapshotHandler(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer s.Close()

	ch := make(chan []byte, 1)
	s.SetSnapshotHandler(func(b []byte) { ch <- b })

	if err := s.Create(ctx, key("a"), newFetch("a"), nil, 0); err != nil {
		t.Fatal(err)
	}

	select {
	case raw := <-ch:
		var snap Snapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			t.Fatalf("snapshot JSON: %v", err)
		}
		if snap.ResourceVersion != 2 || len(snap.Items) != 1 {
			t.Fatalf("snapshot = rv %d, %d items", snap.ResourceVersion, len(snap.Items))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot handler was not called")
	}
}

func TestOnSnapshotCoalesces(t *testing.T) {
	s := Shared("on-snapshot-test")
	snapshots := make(chan []byte, 8)
	stop := OnSnapshot(func(b []byte) {
		select {
		case snapshots <- b:
		default:
		}
	})
	defer stop()

	// The coalescer watches mutations; an idle store stays quiet.
	select {
	case <-snapshots:
		t.Fatal("snapshot without a mutation")
	case <-time.After(time.Second):
	}

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.Create(ctx, key(fmt.Sprintf("coalesce-%d", i)), newFetch(fmt.Sprintf("coalesce-%d", i)), nil, 0); err != nil {
			t.Fatal(err)
		}
	}

	var raw []byte
	select {
	case raw = <-snapshots:
	case <-time.After(3 * time.Second):
		t.Fatal("no snapshot after mutations")
	}

	var all All
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatalf("snapshot envelope: %v", err)
	}
	var snap Snapshot
	if err := json.Unmarshal(all.Stores["on-snapshot-test"], &snap); err != nil {
		t.Fatalf("store snapshot: %v", err)
	}
	if len(snap.Items) < 3 {
		t.Fatalf("snapshot has %d items, want at least 3", len(snap.Items))
	}
}

// TestNoopUpdateDetection checks which updates bump the resource version: a
// semantically identical result must not (nil and empty maps are equal), and
// an update func that modifies its input in place and returns it must (the
// registry's graceful deletion does exactly that).
func TestNoopUpdateDetection(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer s.Close()

	if err := s.Create(ctx, key("a"), newFetch("a"), nil, 0); err != nil {
		t.Fatal(err)
	}
	update := func(fn func(*fetchv1alpha1.Fetch) runtime.Object) string {
		t.Helper()
		var out fetchv1alpha1.Fetch
		if err := s.GuaranteedUpdate(ctx, key("a"), &out, false, nil,
			func(input runtime.Object, _ kstorage.ResponseMeta) (runtime.Object, *uint64, error) {
				return fn(input.(*fetchv1alpha1.Fetch)), nil, nil
			}, nil); err != nil {
			t.Fatal(err)
		}
		return out.ResourceVersion
	}

	if rv := update(func(f *fetchv1alpha1.Fetch) runtime.Object {
		f = f.DeepCopy()
		f.Labels = map[string]string{} // nil before: semantically equal
		return f
	}); rv != "2" {
		t.Fatalf("empty-labels update RV = %s, want 2 (no-op)", rv)
	}
	if rv := update(func(f *fetchv1alpha1.Fetch) runtime.Object {
		now := metav1.Now()
		f.DeletionTimestamp = &now // in place
		return f
	}); rv != "3" {
		t.Fatalf("in-place update RV = %s, want 3", rv)
	}
	var got fetchv1alpha1.Fetch
	if err := s.Get(ctx, key("a"), kstorage.GetOptions{}, &got); err != nil {
		t.Fatal(err)
	}
	if got.DeletionTimestamp == nil {
		t.Fatal("in-place update was not stored")
	}
}

// TestReturnedObjectsAreCopies guards the DeepCopy fast path of copyObject:
// callers own what they get back and mutating it must not reach the store.
func TestReturnedObjectsAreCopies(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer s.Close()

	in := newFetch("a")
	in.Labels = map[string]string{"k": "v"}
	var created fetchv1alpha1.Fetch
	if err := s.Create(ctx, key("a"), in, &created, 0); err != nil {
		t.Fatal(err)
	}
	in.Labels["k"] = "mutated-input"
	created.Labels["k"] = "mutated-create-out"

	var got fetchv1alpha1.Fetch
	if err := s.Get(ctx, key("a"), kstorage.GetOptions{}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Labels["k"] != "v" {
		t.Fatalf("label after mutating create input/output = %q, want v", got.Labels["k"])
	}
	got.Labels["k"] = "mutated-get"

	var list fetchv1alpha1.FetchList
	if err := s.GetList(ctx, prefix, kstorage.ListOptions{Recursive: true}, &list); err != nil {
		t.Fatal(err)
	}
	if list.Items[0].Labels["k"] != "v" {
		t.Fatalf("list label = %q, want v", list.Items[0].Labels["k"])
	}
	list.Items[0].Labels["k"] = "mutated-list"

	var updated fetchv1alpha1.Fetch
	if err := s.GuaranteedUpdate(ctx, key("a"), &updated, false, nil,
		func(input runtime.Object, _ kstorage.ResponseMeta) (runtime.Object, *uint64, error) {
			f := input.(*fetchv1alpha1.Fetch).DeepCopy()
			f.Labels["other"] = "x"
			return f, nil, nil
		}, nil); err != nil {
		t.Fatal(err)
	}
	updated.Labels["k"] = "mutated-update-out"

	var again fetchv1alpha1.Fetch
	if err := s.Get(ctx, key("a"), kstorage.GetOptions{}, &again); err != nil {
		t.Fatal(err)
	}
	if again.Labels["k"] != "v" || again.Labels["other"] != "x" {
		t.Fatalf("labels after mutating results = %v, want k=v other=x", again.Labels)
	}

	// A destination of a different type falls back to JSON.
	var u unstructured.Unstructured
	if err := s.Get(ctx, key("a"), kstorage.GetOptions{}, &u); err != nil {
		t.Fatal(err)
	}
	if u.GetName() != "a" || u.GetLabels()["k"] != "v" {
		t.Fatalf("unstructured Get = %v", u.Object)
	}
}

// applyDelta folds a delta store part into a full one, as the JavaScript
// hosts do (scripts/browser/state_store.js applySnapshot).
func applyDelta(full, delta *Snapshot) {
	full.ResourceVersion = delta.ResourceVersion
	for k, raw := range delta.Items {
		full.Items[k] = raw
		delete(full.Clusters, k)
		delete(full.Shards, k)
		if c, ok := delta.Clusters[k]; ok {
			full.Clusters[k] = c
		}
		if sh, ok := delta.Shards[k]; ok {
			full.Shards[k] = sh
		}
	}
	for _, k := range delta.Deletes {
		delete(full.Items, k)
		delete(full.Clusters, k)
		delete(full.Shards, k)
	}
}

func decodeMessage(t *testing.T, raw []byte, store string) (message, *Snapshot) {
	t.Helper()
	var msg message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("decoding persistence message: %v", err)
	}
	return msg, msg.Stores[store]
}

// TestDeltaSnapshotRoundTrip checks that the first message is full, later
// ones carry only changed keys, skipped keys never appear, and full plus
// deltas restores to the live state.
func TestDeltaSnapshotRoundTrip(t *testing.T) {
	const name = "delta-round-trip"
	s := Shared(name)
	ctx := genericapirequest.WithCluster(context.Background(), genericapirequest.Cluster{Name: "root:org"})
	for _, n := range []string{"a", "b", "c"} {
		if err := s.Create(ctx, key(n), newFetch(n), nil, 0); err != nil {
			t.Fatal(err)
		}
	}

	raw, isFull, err := persistMessage(false)
	if err != nil || !isFull {
		t.Fatalf("first message: full=%t err=%v, want a full message", isFull, err)
	}
	msg, full := decodeMessage(t, raw, name)
	if msg.Delta || full == nil || len(full.Items) != 3 || full.Clusters[key("a")] != "root:org" {
		t.Fatalf("first message = delta %t, store %+v", msg.Delta, full)
	}

	if raw, _, err := persistMessage(false); err != nil || raw != nil {
		t.Fatalf("message without writes = %s, %v; want none", raw, err)
	}
	// Leases are bootstrap state and never persisted.
	if err := s.Create(ctx, "/registry/coordination.k8s.io/leases/root/l", newFetch("l"), nil, 0); err != nil {
		t.Fatal(err)
	}
	if raw, _, err := persistMessage(false); err != nil || raw != nil {
		t.Fatalf("message after a lease write = %s, %v; want none", raw, err)
	}

	var out fetchv1alpha1.Fetch
	if err := s.GuaranteedUpdate(ctx, key("a"), &out, false, nil, func(in runtime.Object, _ kstorage.ResponseMeta) (runtime.Object, *uint64, error) {
		f := in.(*fetchv1alpha1.Fetch).DeepCopy()
		f.Status.Phase = fetchv1alpha1.FetchPhaseFetching
		return f, nil, nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, key("b"), nil, nil, nil, nil, kstorage.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, key("d"), newFetch("d"), nil, 0); err != nil {
		t.Fatal(err)
	}
	raw, isFull, err = persistMessage(false)
	if err != nil || isFull {
		t.Fatalf("second message: full=%t err=%v, want a delta", isFull, err)
	}
	msg, delta := decodeMessage(t, raw, name)
	if !msg.Delta || len(delta.Items) != 2 || delta.Items[key("a")] == nil || delta.Items[key("d")] == nil ||
		len(delta.Deletes) != 1 || delta.Deletes[0] != key("b") {
		t.Fatalf("delta = %+v", delta)
	}

	applyDelta(full, delta)
	merged, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	restored := New()
	defer restored.Close()
	if err := restored.Restore(merged, decodeFetch); err != nil {
		t.Fatal(err)
	}
	var list fetchv1alpha1.FetchList
	if err := restored.GetList(ctx, prefix, kstorage.ListOptions{Recursive: true}, &list); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range list.Items {
		got = append(got, f.Name+"="+string(f.Status.Phase)+"@"+f.Annotations[logicalcluster.AnnotationKey])
	}
	want := fmt.Sprint([]string{"a=Fetching@root:org", "c=Unresolved@root:org", "d=Unresolved@root:org"})
	if fmt.Sprint(got) != want {
		t.Fatalf("restored from full+delta = %v, want %v", got, want)
	}

	// A restore makes the next message full again.
	if err := s.Restore(merged, decodeFetch); err != nil {
		t.Fatal(err)
	}
	if _, isFull, err := persistMessage(false); err != nil || !isFull {
		t.Fatalf("message after restore: full=%t err=%v, want full", isFull, err)
	}
}
