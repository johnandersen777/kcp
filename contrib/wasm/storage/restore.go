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

// debugTimings turns on per-class timing lines for every SnapshotAll call.
// SnapshotAll runs on the coalescing ticker, so the lines are noisy and
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

func classStatsOfSnapshot(raw []byte) map[string]*classStats {
	var snap Snapshot
	stats := map[string]*classStats{}
	if err := json.Unmarshal(raw, &snap); err != nil {
		return stats
	}
	for key, item := range snap.Items {
		addClassStat(stats, key, len(item), 0)
	}
	return stats
}

// SnapshotAll captures every shared store in one JSON envelope.
func SnapshotAll() ([]byte, error) {
	start := time.Now()
	stores := SharedStores()
	names := make([]string, 0, len(stores))
	for name := range stores {
		names = append(names, name)
	}
	sort.Strings(names)
	out := All{Version: allVersion, Stores: map[string]json.RawMessage{}}
	var details strings.Builder
	for _, name := range names {
		storeStart := time.Now()
		raw, err := stores[name].Snapshot()
		if err != nil {
			return nil, fmt.Errorf("snapshotting store %q: %w", name, err)
		}
		stats := map[string]*classStats{}
		if debugTimings {
			stats = classStatsOfSnapshot(raw)
		}
		out.Stores[name] = raw
		if debugTimings {
			fmt.Fprintf(&details, " %q: %d bytes, %s;%s", name, len(raw), time.Since(storeStart).Round(time.Millisecond), formatClassStats(stats))
		}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if debugTimings {
		fmt.Fprintf(os.Stderr, "snapshot: %d store(s), %d bytes, total %s;%s\n", len(out.Stores), len(data), time.Since(start).Round(time.Millisecond), details.String())
	}
	return data, nil
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
	const (
		snapshotQuiet  = 750 * time.Millisecond
		snapshotMaxAge = 10 * time.Second
	)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		last := mutationSeq.Load()
		lastSnapshot := time.Now()
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
				if raw, err := SnapshotAll(); err == nil {
					fn(raw)
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
