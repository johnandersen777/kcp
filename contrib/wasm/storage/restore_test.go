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
	"errors"
	"strings"
	"testing"
	"time"

	kstorage "k8s.io/apiserver/pkg/storage"
)

func TestCheckSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		ok   bool
	}{
		{"empty", "", true},
		{"current", `{"version":1,"stores":{}}`, true},
		{"no stores", `{"version":1}`, true},
		{"older shape", `{"version":0,"items":{}}`, false},
		{"newer", `{"version":2,"stores":{}}`, false},
		{"null", `null`, false},
		{"array", `[1]`, false},
		{"truncated", `{"version":1,"stores":{"main":`, false},
		{"not json", `kcp`, false},
	} {
		err := CheckSnapshot([]byte(tc.data))
		if (err == nil) != tc.ok {
			t.Errorf("%s: CheckSnapshot = %v, want ok=%t", tc.name, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrUnreadableSnapshot) {
			t.Errorf("%s: %v is not ErrUnreadableSnapshot", tc.name, err)
		}
	}
}

// An envelope this build cannot read fails the restore (rc128 ignored it and
// booted fresh, and the first full message then cleared the saved records).
func TestRestoreAllRefusesUnreadableEnvelope(t *testing.T) {
	name := "restore-unreadable-test"
	s := Shared(name)
	ctx := context.Background()
	if err := s.Create(ctx, key("kept"), newFetch("kept"), nil, 0); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{
		`{"version":2,"stores":{"` + name + `":{"items":{}}}}`,
		`{"version":1,"stores":{"` + name + `":`,
	} {
		err := RestoreAll([]byte(data))
		if !errors.Is(err, ErrUnreadableSnapshot) {
			t.Fatalf("RestoreAll(%.40q) = %v, want ErrUnreadableSnapshot", data, err)
		}
	}
	out := newFetch("")
	if err := s.Get(ctx, key("kept"), kstorage.GetOptions{}, out); err != nil {
		t.Fatalf("the store changed after a refused restore: %v", err)
	}
}

func TestFlushSnapshotWithoutLoop(t *testing.T) {
	start := time.Now()
	if FlushSnapshot(50 * time.Millisecond) {
		t.Fatal("FlushSnapshot without an OnSnapshot loop reported a flush")
	}
	if time.Since(start) > time.Second {
		t.Fatal("FlushSnapshot did not honor its timeout")
	}
}

// FlushSnapshot persists pending writes at once, before the quiet period.
func TestFlushSnapshotPersistsPendingWrites(t *testing.T) {
	name := "flush-snapshot-test"
	s := Shared(name)
	got := make(chan []byte, 8)
	stop := OnSnapshot(func(b []byte) { got <- b })
	defer stop()
	if err := s.Create(context.Background(), key("flushed"), newFetch("flushed"), nil, 0); err != nil {
		t.Fatal(err)
	}
	if !FlushSnapshot(2 * time.Second) {
		t.Fatal("FlushSnapshot timed out")
	}
	select {
	case raw := <-got:
		var all All
		if err := json.Unmarshal(raw, &all); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(all.Stores[name]), key("flushed")) {
			t.Fatalf("flushed message lacks the write: %.200s", raw)
		}
	default:
		t.Fatal("FlushSnapshot returned before fn got the pending write")
	}
	// Nothing new: a second flush hands fn nothing.
	if !FlushSnapshot(2 * time.Second) {
		t.Fatal("second FlushSnapshot timed out")
	}
	select {
	case raw := <-got:
		t.Fatalf("flush without writes persisted %.200s", raw)
	default:
	}
}

// MergeAll never replaces a store: a fragment brings back its own keys only,
// and an unreadable envelope changes nothing.
func TestMergeAllKeepsTheLiveStore(t *testing.T) {
	name := "merge-all-test"
	s := Shared(name)
	ctx := context.Background()
	RegisterDecoder(prefix, decodeFetch)
	if err := s.Create(ctx, key("live"), newFetch("live"), nil, 0); err != nil {
		t.Fatal(err)
	}
	saved := New()
	defer saved.Close()
	if err := saved.Create(ctx, key("saved"), newFetch("saved"), nil, 0); err != nil {
		t.Fatal(err)
	}
	snap, err := saved.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(All{Version: allVersion, Stores: map[string]json.RawMessage{name: snap}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MergeAll([]byte(`{"version":2,"stores":{}}`)); !errors.Is(err, ErrUnreadableSnapshot) {
		t.Fatalf("MergeAll(newer envelope) = %v, want ErrUnreadableSnapshot", err)
	}
	added, err := MergeAll(envelope)
	if err != nil || added != 1 {
		t.Fatalf("MergeAll = %d, %v; want 1", added, err)
	}
	for _, n := range []string{"live", "saved"} {
		if err := s.Get(ctx, key(n), kstorage.GetOptions{}, newFetch("")); err != nil {
			t.Fatalf("%s missing after MergeAll: %v", n, err)
		}
	}
}
