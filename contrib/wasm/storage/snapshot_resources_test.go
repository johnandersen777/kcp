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
	"testing"

	kstorage "k8s.io/apiserver/pkg/storage"

	fetchv1alpha1 "github.com/kcp-dev/kcp/contrib/wasm/apis/fetch/v1alpha1"
)

func TestRestoreResourcesMerges(t *testing.T) {
	RegisterDecoder(prefix, decodeFetch)
	ctx := context.Background()
	name := "restore-resources-merge-test"
	s := Shared(name)
	kept := key("kept")
	unrelated := "/registry/other.example.com/widgets/unrelated"
	if err := s.Create(ctx, kept, newFetch("kept"), nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, unrelated, newFetch("unrelated"), nil, 0); err != nil {
		t.Fatal(err)
	}
	before, _, err := s.get(kept, kstorage.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	beforeRV := before.(*fetchv1alpha1.Fetch).ResourceVersion

	next, err := json.Marshal(newFetch("next"))
	if err != nil {
		t.Fatal(err)
	}
	item, err := json.Marshal(newFetch("kept"))
	if err != nil {
		t.Fatal(err)
	}
	envelope := message{Version: allVersion, Stores: map[string]*Snapshot{name: {
		ResourceVersion: 10,
		Items:           map[string]json.RawMessage{key("next"): next, kept: item},
	}}}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := RestoreResources(raw); err != nil {
		t.Fatalf("RestoreResources: %v", err)
	}
	for _, key := range []string{kept, key("next"), unrelated} {
		if _, _, err := s.get(key, kstorage.GetOptions{}); err != nil {
			t.Fatalf("%q missing after merge: %v", key, err)
		}
	}
	after, _, err := s.get(kept, kstorage.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rv := after.(*fetchv1alpha1.Fetch).ResourceVersion; rv == beforeRV {
		t.Fatalf("merged object kept its resource version %q", rv)
	}
}

func TestSnapshotResourcesKeepsOnlySelectedKeys(t *testing.T) {
	ctx := context.Background()
	name := "snapshot-resources-test"
	s := Shared(name)
	kept := key("kept")
	dropped := "/registry/other.example.com/widgets/dropped"
	if err := s.Create(ctx, kept, newFetch("kept"), nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, dropped, newFetch("dropped"), nil, 0); err != nil {
		t.Fatal(err)
	}

	raw, err := SnapshotResources([]string{prefix})
	if err != nil {
		t.Fatalf("SnapshotResources: %v", err)
	}
	if err := CheckSnapshot(raw); err != nil {
		t.Fatalf("CheckSnapshot: %v", err)
	}
	var all All
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(all.Stores[name], &snap); err != nil {
		t.Fatalf("store %q envelope: %v", name, err)
	}
	if _, ok := snap.Items[kept]; !ok {
		t.Fatalf("kept key missing, items = %d", len(snap.Items))
	}
	if _, ok := snap.Items[dropped]; ok {
		t.Fatalf("unselected key %q was kept", dropped)
	}
}
