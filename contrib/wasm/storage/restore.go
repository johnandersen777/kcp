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
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// decoder decodes one resource object from a snapshot item.
type decoder func(raw []byte) (runtime.Object, error)

var (
	decodersMu sync.RWMutex
	decoders   = map[string]decoder{}
)

// RegisterDecoder records how to decode storage keys under prefix. The real
// apiserver registers one per resource when its prefixed store is created.
func RegisterDecoder(prefix string, decode decoder) {
	if prefix == "" || decode == nil {
		return
	}
	prefix = strings.TrimSuffix(prefix, "/") + "/"
	decodersMu.Lock()
	decoders[prefix] = decode
	decodersMu.Unlock()
}

func decoderFor(key string) (decoder, bool) {
	decodersMu.RLock()
	defer decodersMu.RUnlock()
	var best string
	for prefix := range decoders {
		if strings.HasPrefix(key, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return nil, false
	}
	return decoders[best], true
}

// All is the on-disk form of every shared store.
type All struct {
	Version int                        `json:"version"`
	Stores  map[string]json.RawMessage `json:"stores"`
}

const allVersion = 1

// debugTimings turns on a timing line for every persistence message. They
// are produced on the coalescing ticker, so the lines are noisy and
// opt-in; RestoreAll logs its summary once per boot unconditionally.
var debugTimings = debugTimingsEnabled()

// keyClass groups storage keys by store, group and resource so logs stay
// readable. Keys look like /registry/<group>/<resource>/<cluster>/<name>.
func keyClass(key string) string {
	parts := strings.Split(strings.TrimPrefix(key, "/"), "/")
	if len(parts) >= 3 && (parts[0] == "registry" || parts[0] == "cache") {
		return parts[0] + "/" + parts[1] + "/" + parts[2]
	}
	if len(parts) > 0 {
		return parts[0]
	}
	return key
}

type classStats struct {
	items int
	bytes int
	dur   time.Duration
}

func addClassStat(stats map[string]*classStats, key string, bytes int, dur time.Duration) {
	class := keyClass(key)
	stat := stats[class]
	if stat == nil {
		stat = &classStats{}
		stats[class] = stat
	}
	stat.items++
	stat.bytes += bytes
	stat.dur += dur
}

// formatClassStats renders the largest classes first.
func formatClassStats(stats map[string]*classStats) string {
	classes := make([]string, 0, len(stats))
	for class := range stats {
		classes = append(classes, class)
	}
	sort.Slice(classes, func(i, j int) bool {
		if stats[classes[i]].bytes != stats[classes[j]].bytes {
			return stats[classes[i]].bytes > stats[classes[j]].bytes
		}
		return classes[i] < classes[j]
	})
	var b strings.Builder
	for _, class := range classes {
		stat := stats[class]
		fmt.Fprintf(&b, " %s: %d items, %d bytes, %s;", class, stat.items, stat.bytes, stat.dur.Round(time.Millisecond))
	}
	return b.String()
}

// message is one persistence message. A full message has the All shape that
// RestoreAll reads; a delta (Delta set) carries per store only the keys
// written since the previous message plus Snapshot.Deletes. Consumers fold
// deltas into the full form (scripts/browser/state_store.js). Another
// top-level section added here must be carried verbatim by those consumers,
// which they do for any field besides version, delta and stores.
type message struct {
	Version int                  `json:"version"`
	Delta   bool                 `json:"delta,omitempty"`
	Stores  map[string]*Snapshot `json:"stores"`
}

// persistMessage builds the next persistence message across all shared
// stores. It is full when full is set or any store asks for it (new store,
// restore), and a delta otherwise; nil means nothing persisted changed (for
// example only skipped keys such as leases were written). Each call resets
// the stores' dirty sets, so messages must be delivered in order.
func persistMessage(full bool) (data []byte, isFull bool, err error) {
	start := time.Now()
	stores := SharedStores()
	for _, store := range stores {
		store.encMu.Lock()
		full = full || store.needFull
		store.encMu.Unlock()
	}
	out := message{Version: allVersion, Delta: !full, Stores: map[string]*Snapshot{}}
	changed := false
	for name, store := range stores {
		part, partChanged, err := store.persistPart(full, skipSnapshotKey)
		if err != nil {
			return nil, false, fmt.Errorf("snapshotting store %q: %w", name, err)
		}
		if partChanged {
			out.Stores[name] = part
			changed = true
		}
	}
	if !changed {
		return nil, full, nil
	}
	data, err = json.Marshal(out)
	if err != nil {
		return nil, false, err
	}
	if debugTimings {
		fmt.Fprintf(os.Stderr, "snapshot: delta=%t, %d store(s), %d bytes, %s\n", out.Delta, len(out.Stores), len(data), time.Since(start).Round(time.Millisecond))
	}
	return data, full, nil
}

// SnapshotAll captures every shared store in one full JSON envelope, as
// RestoreAll reads it. Keys that RestoreAll would drop are left out.
func SnapshotAll() ([]byte, error) {
	data, _, err := persistMessage(true)
	if data == nil && err == nil {
		data, err = json.Marshal(message{Version: allVersion, Stores: map[string]*Snapshot{}})
	}
	return data, err
}

// skipSnapshotKey drops kcp's own bootstrap state: the cache store rebuilds
// from the registry, and leases/configmaps/RBAC in the system workspaces are
// recreated at boot. Restoring them makes the apiserver fight its own
// bootstrap (lease timeouts, configmap sync failures) and delays readiness.
func skipSnapshotKey(key string) bool {
	if strings.HasPrefix(key, "/cache/") {
		return true
	}
	if strings.Contains(key, "/coordination.k8s.io/leases/") ||
		strings.HasPrefix(key, "/registry/rbac.authorization.k8s.io/") {
		return true
	}
	if strings.HasSuffix(key, "/namespaces/kube-system") {
		// The namespace itself is bootstrap state; its contents are not.
		return false
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "kube-system" || segment == "system:admin" {
			return true
		}
	}
	return false
}

func filterSnapshot(raw []byte) ([]byte, error) {
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, err
	}
	for key := range snap.Items {
		if skipSnapshotKey(key) {
			delete(snap.Items, key)
		}
	}
	for key := range snap.Clusters {
		if skipSnapshotKey(key) {
			delete(snap.Clusters, key)
		}
	}
	for key := range snap.Shards {
		if skipSnapshotKey(key) {
			delete(snap.Shards, key)
		}
	}
	return json.Marshal(snap)
}

// RestoreAll restores one SnapshotAll envelope. Decoders must already be
// registered, i.e. the apiserver has installed its APIs.
func RestoreAll(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	start := time.Now()
	var all All
	if err := json.Unmarshal(data, &all); err != nil {
		return fmt.Errorf("decoding combined snapshot: %w", err)
	}
	if all.Version != allVersion {
		// An older mini-shard snapshot has a different shape. Start fresh
		// rather than failing the apiserver boot.
		fmt.Fprintf(os.Stderr, "restore: ignoring incompatible snapshot version %d\n", all.Version)
		return nil
	}
	fmt.Fprintf(os.Stderr, "restore: %d store(s), %d bytes, envelope decoded in %s\n", len(all.Stores), len(data), time.Since(start).Round(time.Millisecond))
	for name, storeRaw := range all.Stores {
		storeStart := time.Now()
		store := Shared(name)
		filterStart := time.Now()
		raw, err := filterSnapshot(storeRaw)
		if err != nil {
			return fmt.Errorf("filtering store %q: %w", name, err)
		}
		filterDur := time.Since(filterStart)
		restoreStart := time.Now()
		items, fallbacks := 0, 0
		stats := map[string]*classStats{}
		if err := store.RestoreWith(raw, func(key string, itemRaw []byte) (runtime.Object, error) {
			itemStart := time.Now()
			var (
				obj runtime.Object
				err error
			)
			if decode, ok := decoderFor(key); ok {
				obj, err = decode(itemRaw)
			} else {
				fallbacks++
				// Custom resources are decoded by the CRD handler, which registers
				// its storage when the restored CRDs establish. Restore them as
				// unstructured data; the raw API server object carries its GVK.
				var u unstructured.Unstructured
				if err := u.UnmarshalJSON(itemRaw); err != nil {
					return nil, fmt.Errorf("decoding unregistered item %q: %w", key, err)
				}
				if u.GetAPIVersion() == "" || u.GetKind() == "" {
					return nil, fmt.Errorf("unregistered item %q has no apiVersion/kind", key)
				}
				obj, err = &u, nil
			}
			items++
			addClassStat(stats, key, len(itemRaw), time.Since(itemStart))
			return obj, err
		}); err != nil {
			return fmt.Errorf("restoring store %q: %w", name, err)
		}
		fmt.Fprintf(os.Stderr, "restore: store %q: %d items (%d unstructured fallback), %d bytes after filter, filter %s, decode+restore %s, total %s; classes:%s\n",
			name, items, fallbacks, len(raw), filterDur.Round(time.Millisecond), time.Since(restoreStart).Round(time.Millisecond), time.Since(storeStart).Round(time.Millisecond), formatClassStats(stats))
	}
	fmt.Fprintf(os.Stderr, "restore: done in %s\n", time.Since(start).Round(time.Millisecond))
	return nil
}

// OnSnapshot attaches fn to every shared store, including stores created
// later. It returns a function that removes the handler.
func OnSnapshot(fn func([]byte)) func() {
	if fn == nil {
		return func() {}
	}
	// The real apiserver mutates many keys during bootstrap. Serializing the
	// whole store is O(state): on the wasm shard a 6 MB store costs ~1.4s per
	// snapshot, so any fixed cadence fast enough to look "live" keeps the
	// single Go thread busy for most of boot. Watch the process-wide mutation
	// counter instead and persist once writes have been quiet for
	// snapshotQuiet, with snapshotMaxAge as the worst-case staleness while
	// controllers stream changes.
	//
	// fn receives a full snapshot first (and after a restore), then deltas of
	// the keys written since (see persistMessage), so it must apply every
	// message in order. A full snapshot every snapshotCompact lets a consumer
	// that lost a delta converge again.
	const (
		snapshotQuiet  = 750 * time.Millisecond
		snapshotMaxAge = 10 * time.Second
	)
	// KCP_SNAPSHOT_COMPACT_SECONDS overrides the periodic full snapshot; 0
	// turns it off (full snapshots then happen only first and after a failed
	// persist). Each full one encodes the whole store at once, and on a phone
	// that peak is wasm memory the page never gets back.
	snapshotCompact := 10 * time.Minute
	if v, err := strconv.Atoi(os.Getenv("KCP_SNAPSHOT_COMPACT_SECONDS")); err == nil && v >= 0 {
		snapshotCompact = time.Duration(v) * time.Second
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		last := mutationSeq.Load()
		lastSnapshot := time.Now()
		lastFull := time.Time{}
		for {
			select {
			case <-stop:
				return
			case now := <-ticker.C:
				current := mutationSeq.Load()
				if current == last {
					continue
				}
				quiet := now.Sub(time.Unix(0, lastMutationNanos.Load())) >= snapshotQuiet
				if !quiet && now.Sub(lastSnapshot) < snapshotMaxAge {
					continue
				}
				last = current
				full := lastFull.IsZero() || (snapshotCompact > 0 && now.Sub(lastFull) >= snapshotCompact)
				raw, wasFull, err := persistMessage(full)
				if err != nil {
					lastFull = time.Time{} // dirty keys were consumed; resync
				} else if raw != nil {
					fn(raw)
					if wasFull {
						lastFull = time.Now()
					}
				}
				lastSnapshot = time.Now()
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}
