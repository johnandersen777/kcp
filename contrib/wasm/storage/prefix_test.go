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
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/versioning"
	"k8s.io/apimachinery/pkg/watch"
	kstorage "k8s.io/apiserver/pkg/storage"
)

// statusDefaulter mimics the structural-schema default on a Workspace's
// .status.phase, which Kubernetes drops on create (the status subresource
// clears status) and re-applies whenever the object is decoded.
type statusDefaulter struct{}

func (statusDefaulter) Default(obj runtime.Object) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}
	if _, found, _ := unstructured.NestedString(u.Object, "status", "phase"); !found {
		_ = unstructured.SetNestedField(u.Object, "Scheduling", "status", "phase")
	}
}

// unstructuredTyper, creator and convertor are the minimal pieces the
// versioning codec needs for a single unstructured kind.
type unstructuredTyper struct{}

func (unstructuredTyper) ObjectKinds(obj runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Empty() {
		return nil, false, fmt.Errorf("object %T has no kind", obj)
	}
	return []schema.GroupVersionKind{gvk}, false, nil
}

func (unstructuredTyper) Recognizes(schema.GroupVersionKind) bool { return true }

type unstructuredCreator struct{}

func (unstructuredCreator) New(gvk schema.GroupVersionKind) (runtime.Object, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u, nil
}

type unstructuredConvertor struct{}

func (unstructuredConvertor) Convert(in, out, _ interface{}) error {
	src, ok := in.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("cannot convert %T", in)
	}
	dst, ok := out.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("cannot convert into %T", out)
	}
	dst.Object = runtime.DeepCopyJSON(src.Object)
	return nil
}

func (unstructuredConvertor) ConvertToVersion(in runtime.Object, _ runtime.GroupVersioner) (runtime.Object, error) {
	return in, nil
}

func (unstructuredConvertor) ConvertFieldLabel(_ schema.GroupVersionKind, label, value string) (string, string, error) {
	return label, value, nil
}

func testCodec() runtime.Codec {
	gv := schema.GroupVersion{Group: "tenancy.kcp.io", Version: "v1alpha1"}
	return versioning.NewCodec(
		unstructured.UnstructuredJSONScheme,
		unstructured.UnstructuredJSONScheme,
		unstructuredConvertor{},
		unstructuredCreator{},
		unstructuredTyper{},
		statusDefaulter{},
		gv,
		gv,
		"test",
	)
}

func newWorkspace(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "tenancy.kcp.io/v1alpha1",
		"kind":       "Workspace",
		"metadata": map[string]interface{}{
			"name": name,
		},
	}}
}

func newStore(t *testing.T, codec runtime.Codec) kstorage.Interface {
	t.Helper()
	store, err := Prefixed(New(), "/registry", "/workspaces", func() runtime.Object {
		return &unstructured.Unstructured{}
	}, codec)
	if err != nil {
		t.Fatalf("creating prefixed store: %v", err)
	}
	return store
}

func phaseOf(t *testing.T, obj *unstructured.Unstructured) string {
	t.Helper()
	phase, _, err := unstructured.NestedString(obj.Object, "status", "phase")
	if err != nil {
		t.Fatalf("reading status.phase: %v", err)
	}
	return phase
}

// TestDefaultsAppliedOnRead is the regression test for creating a Workspace
// without pre-setting the tenancy.kcp.io/phase label: the status subresource
// strips .status.phase on create, and the controller relies on the codec
// defaulting it back on every read.
func TestDefaultsAppliedOnRead(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, testCodec())

	created := &unstructured.Unstructured{}
	if err := store.Create(ctx, "/workspaces/ws", newWorkspace("ws"), created, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := phaseOf(t, created); got != "Scheduling" {
		t.Errorf("create response status.phase = %q, want %q", got, "Scheduling")
	}

	got := &unstructured.Unstructured{}
	if err := store.Get(ctx, "/workspaces/ws", kstorage.GetOptions{}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if phase := phaseOf(t, got); phase != "Scheduling" {
		t.Errorf("get status.phase = %q, want %q", phase, "Scheduling")
	}

	list := &unstructured.UnstructuredList{}
	if err := store.GetList(ctx, "/workspaces", kstorage.ListOptions{Recursive: true}, list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("list returned %d items, want 1", len(list.Items))
	}
	if phase := phaseOf(t, &list.Items[0]); phase != "Scheduling" {
		t.Errorf("list item status.phase = %q, want %q", phase, "Scheduling")
	}
}

func TestDefaultsAppliedOnUpdate(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, testCodec())

	if err := store.Create(ctx, "/workspaces/ws", newWorkspace("ws"), nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}

	updated := &unstructured.Unstructured{}
	err := store.GuaranteedUpdate(ctx, "/workspaces/ws", updated, false, nil, func(existing runtime.Object, _ kstorage.ResponseMeta) (runtime.Object, *uint64, error) {
		u := existing.DeepCopyObject().(*unstructured.Unstructured)
		if phase := phaseOf(t, u); phase != "Scheduling" {
			t.Errorf("update input status.phase = %q, want %q", phase, "Scheduling")
		}
		labels := u.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels["updated"] = "true"
		u.SetLabels(labels)
		return u, nil, nil
	}, nil)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if phase := phaseOf(t, updated); phase != "Scheduling" {
		t.Errorf("update result status.phase = %q, want %q", phase, "Scheduling")
	}
}

func TestDefaultsAppliedToWatchEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := newStore(t, testCodec())
	if err := store.Create(ctx, "/workspaces/ws", newWorkspace("ws"), nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}

	w, err := store.Watch(ctx, "/workspaces", kstorage.ListOptions{Recursive: true, ResourceVersion: "2"})
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer w.Stop()

	err = store.GuaranteedUpdate(ctx, "/workspaces/ws", &unstructured.Unstructured{}, false, nil, func(existing runtime.Object, _ kstorage.ResponseMeta) (runtime.Object, *uint64, error) {
		u := existing.DeepCopyObject().(*unstructured.Unstructured)
		labels := u.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels["updated"] = "true"
		u.SetLabels(labels)
		return u, nil, nil
	}, nil)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	select {
	case event := <-w.ResultChan():
		if event.Type != watch.Modified {
			t.Fatalf("watch event type = %q, want %q", event.Type, watch.Modified)
		}
		u, ok := event.Object.(*unstructured.Unstructured)
		if !ok {
			t.Fatalf("watch event object is %T, want *unstructured.Unstructured", event.Object)
		}
		if phase := phaseOf(t, u); phase != "Scheduling" {
			t.Errorf("watch event status.phase = %q, want %q", phase, "Scheduling")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for watch event")
	}
}

// TestNoCodecKeepsStoredObjects ensures the simplified Fetch-only wasm server
// (which does not configure a codec) keeps working unchanged.
func TestNoCodecKeepsStoredObjects(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, nil)

	if err := store.Create(ctx, "/workspaces/ws", newWorkspace("ws"), nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := &unstructured.Unstructured{}
	if err := store.Get(ctx, "/workspaces/ws", kstorage.GetOptions{}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, found, _ := unstructured.NestedString(got.Object, "status", "phase"); found {
		t.Error("status.phase was defaulted even though no codec is configured")
	}
}

// countingCodec counts decodes, i.e. applyDefaults round-trips.
type countingCodec struct {
	runtime.Codec
	decodes int
}

func (c *countingCodec) Decode(data []byte, gvk *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	c.decodes++
	return c.Codec.Decode(data, gvk, into)
}

// TestDefaultsMemoizedPerStoredObject checks that reads reuse one decode per
// stored object, that the shared view cannot be modified through a result,
// and that a write invalidates it.
func TestDefaultsMemoizedPerStoredObject(t *testing.T) {
	ctx := context.Background()
	codec := &countingCodec{Codec: testCodec()}
	store := newStore(t, codec)
	if err := store.Create(ctx, "/workspaces/ws", newWorkspace("ws"), nil, 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		got := &unstructured.Unstructured{}
		if err := store.Get(ctx, "/workspaces/ws", kstorage.GetOptions{}, got); err != nil {
			t.Fatal(err)
		}
		if phase := phaseOf(t, got); phase != "Scheduling" {
			t.Fatalf("get %d status.phase = %q", i, phase)
		}
		_ = unstructured.SetNestedField(got.Object, "Mutated", "status", "phase")
	}
	list := &unstructured.UnstructuredList{}
	if err := store.GetList(ctx, "/workspaces", kstorage.ListOptions{Recursive: true}, list); err != nil {
		t.Fatal(err)
	}
	if phase := phaseOf(t, &list.Items[0]); phase != "Scheduling" {
		t.Fatalf("list status.phase = %q after mutating get results", phase)
	}
	if codec.decodes != 1 {
		t.Fatalf("decodes after 3 gets and a list = %d, want 1", codec.decodes)
	}

	if err := store.GuaranteedUpdate(ctx, "/workspaces/ws", &unstructured.Unstructured{}, false, nil, func(existing runtime.Object, _ kstorage.ResponseMeta) (runtime.Object, *uint64, error) {
		u := existing.(*unstructured.Unstructured)
		u.SetLabels(map[string]string{"updated": "true"})
		return u, nil, nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	got := &unstructured.Unstructured{}
	if err := store.Get(ctx, "/workspaces/ws", kstorage.GetOptions{}, got); err != nil {
		t.Fatal(err)
	}
	if got.GetLabels()["updated"] != "true" || phaseOf(t, got) != "Scheduling" {
		t.Fatalf("get after update = %v", got.Object)
	}
	// The update's destination decode is the one the next Get reuses.
	if codec.decodes != 2 {
		t.Fatalf("decodes after update and get = %d, want 2", codec.decodes)
	}
}
