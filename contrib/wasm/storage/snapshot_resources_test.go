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
)

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
