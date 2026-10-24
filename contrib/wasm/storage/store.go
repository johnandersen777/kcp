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

// Package storage implements k8s.io/apiserver/pkg/storage.Interface on top of
// an in-memory map. It is the single-node replacement for etcd used by the
// WASM kcp shard: no mmap, no WAL, no sockets.
//
// The implementation provides resource versions, read-your-writes
// consistency, precondition checks, watches, list with label/field selection,
// and JSON snapshots for host-side persistence.
package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	kstorage "k8s.io/apiserver/pkg/storage"
	"k8s.io/klog/v2"

	"github.com/kcp-dev/logicalcluster/v3"
)

// Snapshot is the serialized form of the store. Items are keyed by storage
// key and hold each runtime.Object as JSON. Clusters and Shards carry the
// kcp logical-cluster identity of each key (kcp does not persist it in the
// object; the etcd3 store re-annotates objects on read).
type Snapshot struct {
	ResourceVersion uint64                     `json:"resourceVersion"`
	Items           map[string]json.RawMessage `json:"items"`
	Clusters        map[string]string          `json:"clusters,omitempty"`
	Shards          map[string]string          `json:"shards,omitempty"`
	// Deletes is set only in a delta (see persistMessage): keys removed since
	// the previous message. Items then holds only the keys written since.
	Deletes []string `json:"deletes,omitempty"`
}

// objectMeta is the kcp identity of an object, derived from the request
// context at write time.
type objectMeta struct {
	cluster logicalcluster.Name
	shard   genericapirequest.Shard
}

// Store is an in-memory storage.Interface implementation.
type Store struct {
	mu         sync.RWMutex
	objects    map[string]runtime.Object
	objectMeta map[string]objectMeta
	rv         uint64
	versioner  kstorage.Versioner
	watchers   map[*bufferWatcher]*watchRegistration

	// events is a bounded history used to replay events when a watch starts
	// at a resource version returned by a previous list. compactedRV is the
	// newest revision that has been dropped from the history.
	events      []storedEvent
	compactedRV uint64

	// derived memoizes per-reader views of stored objects (prefixedStore's
	// codec-defaulted decode), keyed by storage key and then by reader. An
	// entry is valid only for the stored object it was computed from; writes
	// drop the key's entries. Lock order: mu, then derivedMu.
	derivedMu sync.Mutex
	derived   map[string]map[any]*derivedEntry

	// Persistence bookkeeping, guarded by encMu (taken after mu). encoded
	// caches the JSON of objects[key], filled lazily by snapshots so a key
	// written many times between snapshots is encoded once; dirty holds the
	// keys written since the previous persistence message; needFull asks for
	// a full message (new store, or restored contents).
	encMu    sync.Mutex
	encoded  map[string]json.RawMessage
	dirty    map[string]struct{}
	needFull bool

	onChange  func([]byte)
	persistCh chan []byte
	stopCh    chan struct{}
	closeOnce sync.Once
	keysFunc  kstorage.KeysFunc
}

// maxWatchHistory bounds the replayed event history. When a watch starts
// older than the history, it receives the current state as ADDED events.
const maxWatchHistory = 4096

// mutationSeq counts writes across all stores. Coalescing snapshotters (see
// OnSnapshot) poll it instead of receiving a serialized snapshot after every
// mutation: encoding the whole store on each write is O(state) per write and
// dominated boot time for the real kcp shard (a 6 MB store meant roughly a
// second of JSON per write in wasm).
var mutationSeq atomic.Uint64

// lastMutationNanos is the wall-clock time of the newest mutation, used to
// persist once writes settle rather than while controllers stream them.
var lastMutationNanos atomic.Int64

func noteMutation() {
	mutationSeq.Add(1)
	lastMutationNanos.Store(time.Now().UnixNano())
}

type storedEvent struct {
	rv   uint64
	typ  watch.EventType
	key  string
	obj  runtime.Object
	meta objectMeta
}

type watchRegistration struct {
	key       string
	recursive bool
	predicate kstorage.SelectionPredicate
}

var _ kstorage.Interface = (*Store)(nil)

// New returns an empty in-memory store.
func New() *Store {
	s := &Store{
		objects:    map[string]runtime.Object{},
		objectMeta: map[string]objectMeta{},
		rv:         1, // like etcd, the first write is revision 2; lists never report 0
		versioner:  kstorage.APIObjectVersioner{},
		watchers:   map[*bufferWatcher]*watchRegistration{},
		derived:    map[string]map[any]*derivedEntry{},
		encoded:    map[string]json.RawMessage{},
		dirty:      map[string]struct{}{},
		needFull:   true,
		persistCh:  make(chan []byte, 1),
		stopCh:     make(chan struct{}),
	}
	go s.persistLoop()
	return s
}

// Close stops the snapshot goroutine.
func (s *Store) Close() {
	s.closeOnce.Do(func() { close(s.stopCh) })
}

// SetSnapshotHandler registers a callback invoked (asynchronously, with the
// latest state only) after every mutation. The callback receives a JSON
// Snapshot.
func (s *Store) SetSnapshotHandler(fn func([]byte)) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// Snapshot serializes the current state.
func (s *Store) Snapshot() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked()
}

// Restore replaces the store contents with a snapshot. decode turns the raw
// JSON of one item back into its runtime.Object (e.g. a *Fetch).
func (s *Store) Restore(data []byte, decode func([]byte) (runtime.Object, error)) error {
	return s.RestoreWith(data, func(_ string, raw []byte) (runtime.Object, error) { return decode(raw) })
}

// RestoreWith is Restore with the storage key passed to the decoder. The
// key lets a heterogeneous apiserver pick the resource's typed decoder.
func (s *Store) RestoreWith(data []byte, decode func(key string, raw []byte) (runtime.Object, error)) error {
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("decoding snapshot: %w", err)
	}

	objects := make(map[string]runtime.Object, len(snap.Items))
	metas := make(map[string]objectMeta, len(snap.Items))
	maxRV := snap.ResourceVersion
	if maxRV == 0 {
		maxRV = 1
	}
	for key, raw := range snap.Items {
		obj, err := decode(key, raw)
		if err != nil {
			return fmt.Errorf("decoding item %q: %w", key, err)
		}
		rv, err := s.versioner.ObjectResourceVersion(obj)
		if err != nil {
			return fmt.Errorf("reading resource version of %q: %w", key, err)
		}
		if rv > maxRV {
			maxRV = rv
		}
		objects[key] = obj
	}

	for key, cluster := range snap.Clusters {
		m := metas[key]
		m.cluster = logicalcluster.Name(cluster)
		metas[key] = m
	}
	for key, shard := range snap.Shards {
		m := metas[key]
		m.shard = genericapirequest.Shard(shard)
		metas[key] = m
	}

	s.mu.Lock()
	s.objects = objects
	s.objectMeta = metas
	s.rv = maxRV
	s.events = nil
	s.derivedMu.Lock()
	s.derived = map[string]map[any]*derivedEntry{}
	s.derivedMu.Unlock()
	s.encMu.Lock()
	s.encoded = make(map[string]json.RawMessage, len(snap.Items))
	for key, raw := range snap.Items {
		s.encoded[key] = raw // restorable as-is: it was just restored from
	}
	s.dirty = map[string]struct{}{}
	s.needFull = true
	s.encMu.Unlock()
	// Restored objects are the current state; any watch older than the
	// snapshot gets a fresh ADDED view rather than replaying nothing.
	s.compactedRV = maxRV
	// Watchers that connected before the restore have a stale view: their
	// initial list saw the old (usually empty) store and the restore emits no
	// events. The apiserver's watch cache is one of them, so leaving it open
	// means restored objects never reach informers (a restored kcp shard then
	// hangs waiting for its system CRDs). Close them so clients relist.
	for w := range s.watchers {
		delete(s.watchers, w)
		w.close()
	}
	s.mu.Unlock()
	return nil
}

// Versioner implements storage.Interface.
func (s *Store) Versioner() kstorage.Versioner { return s.versioner }

// Create implements storage.Interface.
func (s *Store) Create(ctx context.Context, key string, obj, out runtime.Object, ttl uint64) error {
	stored, m, err := s.create(ctx, key, obj, ttl)
	if err != nil || out == nil {
		return err
	}
	return intoAnnotated(stored, m, out)
}

// create stores a copy of obj and returns the stored object, which callers
// must not modify.
func (s *Store) create(ctx context.Context, key string, obj runtime.Object, ttl uint64) (runtime.Object, objectMeta, error) {
	if ttl != 0 {
		return nil, objectMeta{}, fmt.Errorf("in-memory store does not support TTL")
	}
	if obj == nil {
		return nil, objectMeta{}, fmt.Errorf("cannot create a nil object")
	}

	stored := obj.DeepCopyObject()
	m := metaFromContext(ctx)

	s.mu.Lock()
	if _, exists := s.objects[key]; exists {
		rv := s.rv
		s.mu.Unlock()
		return nil, objectMeta{}, kstorage.NewKeyExistsError(key, int64(rv))
	}
	s.rv++
	if err := s.versioner.UpdateObject(stored, s.rv); err != nil {
		s.rv--
		s.mu.Unlock()
		return nil, objectMeta{}, err
	}
	s.objects[key] = stored
	s.objectMeta[key] = m
	s.changedLocked(key)
	s.recordEventLocked(key, watch.Added, stored)
	s.broadcastLocked(key, watch.Added, stored)
	noteMutation()
	snap := s.snapshotForHandlerLocked()
	s.mu.Unlock()

	s.persist(snap)
	return stored, m, nil
}

// Get implements storage.Interface.
func (s *Store) Get(ctx context.Context, key string, opts kstorage.GetOptions, objPtr runtime.Object) error {
	obj, m, err := s.get(key, opts)
	if err != nil || obj == nil {
		return err
	}
	return intoAnnotated(obj, m, objPtr)
}

// get returns the stored object (nil if absent and opts.IgnoreNotFound).
// Stored objects are never mutated in place (writes replace them), so callers
// copy them outside the lock.
func (s *Store) get(key string, opts kstorage.GetOptions) (runtime.Object, objectMeta, error) {
	s.mu.RLock()
	obj, exists := s.objects[key]
	m := s.objectMeta[key]
	rv := s.rv
	s.mu.RUnlock()

	if !exists {
		if opts.IgnoreNotFound {
			return nil, objectMeta{}, nil
		}
		return nil, objectMeta{}, kstorage.NewKeyNotFoundError(key, int64(rv))
	}
	return obj, m, nil
}

// GetList implements storage.Interface.
func (s *Store) GetList(ctx context.Context, key string, opts kstorage.ListOptions, listObj runtime.Object) error {
	return s.getList(key, opts, listObj, func(e storedRef) (runtime.Object, error) {
		return annotatedCopy(e.Object, e.meta), nil
	})
}

// getList lists the stored objects under key and fills listObj with view(e)
// of each, in key order.
func (s *Store) getList(key string, opts kstorage.ListOptions, listObj runtime.Object, view func(storedRef) (runtime.Object, error)) error {
	s.mu.RLock()
	entries := make([]storedRef, 0, len(s.objects))
	for k, obj := range s.objects {
		if !matchesKey(k, key, opts.Recursive) {
			continue
		}
		if !predicateMatches(opts.Predicate, obj) {
			continue
		}
		entries = append(entries, storedRef{Object: obj, key: k, meta: s.objectMeta[k]})
	}
	rv := s.rv
	s.mu.RUnlock()

	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })

	// Continue is the last key of the previous page (opaque to clients).
	if cont := opts.Predicate.Continue; cont != "" {
		filtered := entries[:0]
		for _, e := range entries {
			if e.key > cont {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	var continueValue string
	var remaining *int64
	if limit := opts.Predicate.Limit; limit > 0 && int64(len(entries)) > limit {
		remain := int64(len(entries)) - limit
		remaining = &remain
		continueValue = entries[limit-1].key
		entries = entries[:limit]
	}

	items := make([]runtime.Object, 0, len(entries))
	for _, e := range entries {
		item, err := view(e)
		if err != nil {
			return err
		}
		items = append(items, item)
	}

	if err := meta.SetList(listObj, items); err != nil {
		return err
	}
	return s.versioner.UpdateList(listObj, rv, continueValue, remaining)
}

// GuaranteedUpdate implements storage.Interface.
func (s *Store) GuaranteedUpdate(
	ctx context.Context,
	key string,
	destination runtime.Object,
	ignoreNotFound bool,
	preconditions *kstorage.Preconditions,
	tryUpdate kstorage.UpdateFunc,
	cachedExistingObject runtime.Object,
) error {
	stored, m, err := s.guaranteedUpdate(ctx, key, destination, ignoreNotFound, preconditions, tryUpdate, nil)
	if err != nil {
		return err
	}
	return intoAnnotated(stored, m, destination)
}

// guaranteedUpdate is GuaranteedUpdate with an optional prepare hook that
// turns the stored object into the caller's view of it (prefixedStore
// re-applies codec defaults there). prepare runs under the store lock and
// must not modify its argument. It returns the stored object after the
// update (the existing one for a no-op), which callers must not modify.
func (s *Store) guaranteedUpdate(
	ctx context.Context,
	key string,
	destination runtime.Object,
	ignoreNotFound bool,
	preconditions *kstorage.Preconditions,
	tryUpdate kstorage.UpdateFunc,
	prepare func(stored runtime.Object) (runtime.Object, error),
) (runtime.Object, objectMeta, error) {
	if tryUpdate == nil {
		return nil, objectMeta{}, fmt.Errorf("tryUpdate is required")
	}

	s.mu.Lock()
	existing, exists := s.objects[key]
	m := s.objectMeta[key]

	// orig is the unmodified view a no-op is detected against. tryUpdate gets
	// a private copy because it may modify its input in place and return it
	// (the registry's graceful deletion does); comparing against that same
	// object would turn every such update into a no-op.
	var orig, input runtime.Object
	var responseMeta kstorage.ResponseMeta
	if exists {
		rv, err := s.versioner.ObjectResourceVersion(existing)
		if err != nil {
			s.mu.Unlock()
			return nil, objectMeta{}, err
		}
		responseMeta.ResourceVersion = rv
		if preconditions != nil {
			if err := preconditions.Check(key, existing); err != nil {
				s.mu.Unlock()
				return nil, objectMeta{}, err
			}
		}
		orig = existing
		if prepare != nil {
			if orig, err = prepare(existing); err != nil {
				s.mu.Unlock()
				return nil, objectMeta{}, err
			}
		}
		input = orig.DeepCopyObject()
	} else {
		if !ignoreNotFound {
			rv := s.rv
			s.mu.Unlock()
			return nil, objectMeta{}, kstorage.NewKeyNotFoundError(key, int64(rv))
		}
		// Create-on-update semantics: hand the caller a zero object.
		input = destination.DeepCopyObject()
	}

	output, _, err := tryUpdate(input, responseMeta)
	if err != nil {
		s.mu.Unlock()
		return nil, objectMeta{}, err
	}
	if output == nil {
		output = input
	}

	if exists && objectsEqual(orig, output) {
		s.mu.Unlock()
		return existing, m, nil
	}

	s.rv++
	if err := s.versioner.UpdateObject(output, s.rv); err != nil {
		s.rv--
		s.mu.Unlock()
		return nil, objectMeta{}, err
	}
	stored := output.DeepCopyObject()
	m = metaFromContext(ctx)
	s.objects[key] = stored
	s.objectMeta[key] = m
	s.changedLocked(key)

	eventType := watch.Added
	if exists {
		eventType = watch.Modified
	}
	s.recordEventLocked(key, eventType, stored)
	s.broadcastLocked(key, eventType, stored)
	noteMutation()
	snap := s.snapshotForHandlerLocked()
	s.mu.Unlock()

	s.persist(snap)
	return stored, m, nil
}

// Delete implements storage.Interface.
func (s *Store) Delete(
	ctx context.Context,
	key string,
	out runtime.Object,
	preconditions *kstorage.Preconditions,
	validateDeletion kstorage.ValidateObjectFunc,
	cachedExistingObject runtime.Object,
	opts kstorage.DeleteOptions,
) error {
	obj, m, err := s.delete(ctx, key, preconditions, validateDeletion)
	if err != nil || out == nil {
		return err
	}
	return intoAnnotated(obj, m, out)
}

// delete removes key and returns the object it held.
func (s *Store) delete(ctx context.Context, key string, preconditions *kstorage.Preconditions, validateDeletion kstorage.ValidateObjectFunc) (runtime.Object, objectMeta, error) {
	s.mu.Lock()
	obj, exists := s.objects[key]
	if !exists {
		rv := s.rv
		s.mu.Unlock()
		return nil, objectMeta{}, kstorage.NewKeyNotFoundError(key, int64(rv))
	}
	if preconditions != nil {
		if err := preconditions.Check(key, obj); err != nil {
			s.mu.Unlock()
			return nil, objectMeta{}, err
		}
	}
	if validateDeletion != nil {
		if err := validateDeletion(ctx, obj.DeepCopyObject()); err != nil {
			s.mu.Unlock()
			return nil, objectMeta{}, err
		}
	}

	s.rv++
	delete(s.objects, key)
	s.changedLocked(key)
	s.recordEventLocked(key, watch.Deleted, obj)
	s.broadcastLocked(key, watch.Deleted, obj)
	m := s.objectMeta[key]
	delete(s.objectMeta, key)
	noteMutation()
	snap := s.snapshotForHandlerLocked()
	s.mu.Unlock()

	s.persist(snap)
	return obj, m, nil
}

// Watch implements storage.Interface.
func (s *Store) Watch(ctx context.Context, key string, opts kstorage.ListOptions) (watch.Interface, error) {
	return s.watch(ctx, key, opts, nil, func(ref *storedRef) (runtime.Object, error) {
		return annotatedCopy(ref.Object, ref.meta), nil
	})
}

// watch is Watch with an optional prototype constructor used to emit the
// initial-events-end BOOKMARK required by the WatchList protocol (the raw
// etcd3 store does the same; without it, informers using sendInitialEvents
// never finish initializing), and with view turning each stored object into
// the caller-owned object the event carries. view runs on the watcher's
// goroutine, outside the store lock.
func (s *Store) watch(ctx context.Context, key string, opts kstorage.ListOptions, bookmark func() runtime.Object, view func(*storedRef) (runtime.Object, error)) (watch.Interface, error) {
	w := newBufferWatcher()
	w.onStop = func() {
		s.mu.Lock()
		delete(s.watchers, w)
		s.mu.Unlock()
	}

	var requestedRV uint64
	var fromRV bool
	if opts.ResourceVersion != "" && opts.ResourceVersion != "0" {
		rv, err := s.versioner.ParseResourceVersion(opts.ResourceVersion)
		if err != nil {
			return nil, err
		}
		requestedRV = rv
		fromRV = true
	}
	sendInitial := opts.ResourceVersion == "0" ||
		(opts.SendInitialEvents != nil && *opts.SendInitialEvents)

	s.mu.Lock()
	if fromRV && requestedRV > s.rv {
		rv := s.rv
		s.mu.Unlock()
		return nil, kstorage.NewTooLargeResourceVersionError(requestedRV, rv, 1)
	}
	// Like a compacted etcd: a watch older than the history must relist.
	// Replaying the current state as ADDED events would never deliver the
	// deletions in the gap, leaving informers with stale objects.
	if fromRV && !sendInitial && requestedRV < s.compactedRV {
		compacted := s.compactedRV
		s.mu.Unlock()
		return nil, apierrors.NewResourceExpired(fmt.Sprintf("too old resource version: %d (%d)", requestedRV, compacted))
	}

	reg := &watchRegistration{key: key, recursive: opts.Recursive, predicate: opts.Predicate}
	s.watchers[w] = reg

	switch {
	case sendInitial:
		// Send the current state as ADDED events.
		keys := make([]string, 0, len(s.objects))
		for k := range s.objects {
			if matchesKey(k, key, opts.Recursive) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			obj := s.objects[k]
			if predicateMatches(opts.Predicate, obj) {
				w.send(watch.Event{Type: watch.Added, Object: &storedRef{Object: obj, key: k, meta: s.objectMeta[k]}})
			}
		}
		if bookmark != nil && opts.SendInitialEvents != nil && *opts.SendInitialEvents && opts.Predicate.AllowWatchBookmarks {
			obj := bookmark()
			if err := s.versioner.UpdateObject(obj, s.rv); err == nil {
				if err := kstorage.AnnotateInitialEventsEndBookmark(obj); err == nil {
					w.send(watch.Event{Type: watch.Bookmark, Object: obj})
				}
			}
		}
	case fromRV:
		for _, ev := range s.events {
			if ev.rv <= requestedRV {
				continue
			}
			if !matchesKey(ev.key, key, opts.Recursive) {
				continue
			}
			if !predicateMatches(opts.Predicate, ev.obj) {
				continue
			}
			w.send(watch.Event{Type: ev.typ, Object: &storedRef{Object: ev.obj, key: ev.key, meta: ev.meta}})
		}
	}
	s.mu.Unlock()

	go func() {
		select {
		case <-ctx.Done():
			w.Stop()
		case <-w.done:
		}
	}()

	return watch.Filter(w, func(in watch.Event) (watch.Event, bool) {
		ref, ok := in.Object.(*storedRef)
		if !ok {
			return in, true // bookmarks
		}
		obj, err := view(ref)
		if err != nil {
			// Surfacing the event undecoded is better than silently dropping a
			// watch update.
			klog.FromContext(ctx).Error(err, "failed to prepare watch event", "type", in.Type, "key", ref.key)
			obj = annotatedCopy(ref.Object, ref.meta)
		}
		in.Object = obj
		return in, true
	}), nil
}

// Stats implements storage.Interface.
func (s *Store) Stats(ctx context.Context) (kstorage.Stats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return kstorage.Stats{ObjectCount: int64(len(s.objects))}, nil
}

// ReadinessCheck implements storage.Interface.
func (s *Store) ReadinessCheck() error { return nil }

// RequestWatchProgress implements storage.Interface.
func (s *Store) RequestWatchProgress(ctx context.Context) error { return nil }

// GetCurrentResourceVersion implements storage.Interface.
func (s *Store) GetCurrentResourceVersion(ctx context.Context) (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rv, nil
}

// EnableResourceSizeEstimation implements storage.Interface.
func (s *Store) EnableResourceSizeEstimation(keysFunc kstorage.KeysFunc) error {
	s.mu.Lock()
	s.keysFunc = keysFunc
	s.mu.Unlock()
	return nil
}

// CompactRevision implements storage.Interface. There is no compaction.
func (s *Store) CompactRevision() int64 { return 0 }

func (s *Store) recordEventLocked(key string, typ watch.EventType, obj runtime.Object) {
	s.events = append(s.events, storedEvent{
		rv:   s.rv,
		typ:  typ,
		key:  key,
		obj:  obj, // stored objects are immutable
		meta: s.objectMeta[key],
	})
	// Trim in batches: copying the whole history on every write once it is
	// full dominated update cost (~500 KB copied per write).
	if len(s.events) > maxWatchHistory+maxWatchHistory/4 {
		drop := len(s.events) - maxWatchHistory
		s.compactedRV = s.events[drop-1].rv
		s.events = append([]storedEvent(nil), s.events[drop:]...)
	}
}

func (s *Store) broadcastLocked(key string, typ watch.EventType, obj runtime.Object) {
	for w, reg := range s.watchers {
		if !matchesKey(key, reg.key, reg.recursive) {
			continue
		}
		if !predicateMatches(reg.predicate, obj) {
			continue
		}
		if !w.send(watch.Event{Type: typ, Object: &storedRef{Object: obj, key: key, meta: s.objectMeta[key]}}) {
			// Slow consumer: drop it rather than blocking storage. The
			// callback is deliberately not invoked here because we already
			// hold the store lock.
			delete(s.watchers, w)
			w.close()
		}
	}
}

func (s *Store) snapshotLocked() ([]byte, error) {
	snap := Snapshot{
		ResourceVersion: s.rv,
		Items:           make(map[string]json.RawMessage, len(s.objects)),
		Clusters:        make(map[string]string, len(s.objectMeta)),
		Shards:          make(map[string]string, len(s.objectMeta)),
	}
	s.encMu.Lock()
	for key, obj := range s.objects {
		raw, err := s.encodedLocked(key, obj)
		if err != nil {
			s.encMu.Unlock()
			return nil, err
		}
		snap.Items[key] = raw
	}
	s.encMu.Unlock()
	for key, m := range s.objectMeta {
		if !m.cluster.Empty() {
			snap.Clusters[key] = m.cluster.String()
		}
		if !m.shard.Empty() {
			snap.Shards[key] = m.shard.String()
		}
	}
	return json.Marshal(snap)
}

// snapshotBytesLocked returns nil if the snapshot cannot be encoded; the
// mutation itself still succeeded.
func (s *Store) snapshotBytesLocked() []byte {
	b, err := s.snapshotLocked()
	if err != nil {
		return nil
	}
	return b
}

// snapshotForHandlerLocked serializes the store only when a snapshot handler
// actually consumes the bytes. Handlers that only need to know *that*
// something changed (OnSnapshot) use mutationSeq instead.
func (s *Store) snapshotForHandlerLocked() []byte {
	if s.onChange == nil {
		return nil
	}
	return s.snapshotBytesLocked()
}

func (s *Store) persist(snap []byte) {
	if snap == nil {
		return
	}
	s.mu.RLock()
	enabled := s.onChange != nil
	s.mu.RUnlock()
	if !enabled {
		return
	}

	select {
	case <-s.stopCh:
		return
	default:
	}

	select {
	case s.persistCh <- snap:
	default:
		// Keep only the newest snapshot.
		select {
		case <-s.persistCh:
		default:
		}
		select {
		case s.persistCh <- snap:
		default:
		}
	}
}

func (s *Store) persistLoop() {
	for {
		select {
		case <-s.stopCh:
			return
		case snap := <-s.persistCh:
			s.mu.RLock()
			fn := s.onChange
			s.mu.RUnlock()
			if fn != nil {
				fn(snap)
			}
		}
	}
}

func matchesKey(key, prefix string, recursive bool) bool {
	if recursive {
		return strings.HasPrefix(key, prefix)
	}
	return key == prefix
}

func predicateMatches(p kstorage.SelectionPredicate, obj runtime.Object) bool {
	labelEmpty := p.Label == nil || p.Label.Empty()
	fieldEmpty := p.Field == nil || p.Field.Empty()
	if labelEmpty && fieldEmpty {
		return true
	}
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return false
	}
	if !labelEmpty && !p.Label.Matches(labels.Set(accessor.GetLabels())) {
		return false
	}
	if !fieldEmpty && !p.Field.Matches(fields.Set{
		"metadata.name":      accessor.GetName(),
		"metadata.namespace": accessor.GetNamespace(),
	}) {
		return false
	}
	return true
}

// objectsEqual decides whether an update is a no-op, which must not bump the
// resource version. Semantic equality treats nil and empty collections, and
// equal quantities and times, alike, as their encodings would.
func objectsEqual(a, b runtime.Object) bool {
	return apiequality.Semantic.DeepEqual(a, b)
}

// copyObject deep-copies src into dst. When both have the same concrete type
// (the common case) the copy is a DeepCopyObject assigned through the
// destination pointer, as the watch cache does for its `out runtime.Object`
// arguments. Only mismatched types, such as Unstructured into a typed object,
// pay for a JSON round-trip.
func copyObject(src, dst runtime.Object) error {
	sv, dv := reflect.ValueOf(src), reflect.ValueOf(dst)
	if sv.Type() == dv.Type() && dv.Kind() == reflect.Pointer && !dv.IsNil() && !sv.IsNil() {
		dv.Elem().Set(reflect.ValueOf(src.DeepCopyObject()).Elem())
		return nil
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return fmt.Errorf("encoding object: %w", err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("decoding object into %T: %w", dst, err)
	}
	return nil
}

// metaFromContext captures the kcp logical cluster and shard of a write
// request. kcp's etcd3 store does not persist these in the object; it
// re-annotates objects on read, and clients (notably the in-process cache
// server) rely on the annotations to build URLs.
func metaFromContext(ctx context.Context) objectMeta {
	var m objectMeta
	if name, err := genericapirequest.ClusterNameFrom(ctx); err == nil {
		m.cluster = name
	}
	m.shard = genericapirequest.ShardFrom(ctx)
	return m
}

func annotateObject(obj runtime.Object, m objectMeta) {
	if obj == nil {
		return
	}
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return
	}
	if m.cluster.Empty() && m.shard.Empty() {
		return
	}
	annotations := accessor.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if !m.cluster.Empty() {
		annotations[logicalcluster.AnnotationKey] = m.cluster.String()
	}
	if !m.shard.Empty() {
		annotations[genericapirequest.ShardAnnotationKey] = m.shard.String()
	}
	accessor.SetAnnotations(annotations)
}

// intoAnnotated copies src into dst and adds the kcp identity annotations.
func intoAnnotated(src runtime.Object, m objectMeta, dst runtime.Object) error {
	if err := copyObject(src, dst); err != nil {
		return err
	}
	annotateObject(dst, m)
	return nil
}

// storedRef is a stored object (shared, never to be modified) with its key
// and kcp identity. Watch events carry it until the watcher's view turns it
// into a caller-owned object.
type storedRef struct {
	runtime.Object
	key  string
	meta objectMeta
}

type derivedEntry struct {
	src  runtime.Object
	once sync.Once
	obj  runtime.Object
	err  error
}

// changedLocked records a write of key: it drops the key's derived views and
// cached encoding and marks it for the next delta. The caller holds mu for
// writing.
func (s *Store) changedLocked(key string) {
	s.derivedMu.Lock()
	delete(s.derived, key)
	s.derivedMu.Unlock()
	s.encMu.Lock()
	delete(s.encoded, key)
	s.dirty[key] = struct{}{}
	s.encMu.Unlock()
}

// encodedLocked returns the cached JSON of obj, the current object at key.
// The caller holds mu (either way) and encMu.
func (s *Store) encodedLocked(key string, obj runtime.Object) (json.RawMessage, error) {
	if raw, ok := s.encoded[key]; ok {
		return raw, nil
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("encoding %q: %w", key, err)
	}
	s.encoded[key] = raw
	return raw, nil
}

// persistPart returns this store's part of the next persistence message: all
// kept keys when full, else the kept keys written since the previous call.
// skip drops keys that are not persisted. changed reports whether the part
// carries any item or delete.
func (s *Store) persistPart(full bool, skip func(string) bool) (part *Snapshot, changed bool, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.encMu.Lock()
	defer s.encMu.Unlock()

	part = &Snapshot{ResourceVersion: s.rv, Items: map[string]json.RawMessage{}, Clusters: map[string]string{}, Shards: map[string]string{}}
	add := func(key string, obj runtime.Object) error {
		raw, err := s.encodedLocked(key, obj)
		if err != nil {
			return err
		}
		part.Items[key] = raw
		if m := s.objectMeta[key]; !m.cluster.Empty() {
			part.Clusters[key] = m.cluster.String()
		}
		if m := s.objectMeta[key]; !m.shard.Empty() {
			part.Shards[key] = m.shard.String()
		}
		return nil
	}
	if full {
		for key, obj := range s.objects {
			if !skip(key) {
				if err := add(key, obj); err != nil {
					return nil, false, err
				}
			}
		}
	} else {
		for key := range s.dirty {
			if skip(key) {
				continue
			}
			if obj, ok := s.objects[key]; ok {
				if err := add(key, obj); err != nil {
					return nil, false, err
				}
			} else {
				part.Deletes = append(part.Deletes, key)
			}
		}
		sort.Strings(part.Deletes)
	}
	s.dirty = map[string]struct{}{}
	s.needFull = false
	return part, full || len(part.Items) > 0 || len(part.Deletes) > 0, nil
}

// derive returns fn(src) for the stored object src at key, computed once per
// owner while src is the current object at key. The result is shared and
// must not be modified. Objects that are no longer current (deleted, or
// replayed from the watch history) are computed but not memoized.
func (s *Store) derive(owner any, key string, src runtime.Object, fn func(runtime.Object) (runtime.Object, error)) (runtime.Object, error) {
	s.mu.RLock()
	e := s.derivedEntryLocked(owner, key, src)
	s.mu.RUnlock()
	return e.get(fn)
}

// derivedEntryLocked is derive's lookup; the caller holds mu (either way).
func (s *Store) derivedEntryLocked(owner any, key string, src runtime.Object) *derivedEntry {
	if s.objects[key] != src {
		return &derivedEntry{src: src}
	}
	s.derivedMu.Lock()
	defer s.derivedMu.Unlock()
	byOwner := s.derived[key]
	if byOwner == nil {
		byOwner = map[any]*derivedEntry{}
		s.derived[key] = byOwner
	}
	e := byOwner[owner]
	if e == nil || e.src != src {
		e = &derivedEntry{src: src}
		byOwner[owner] = e
	}
	return e
}

func (e *derivedEntry) get(fn func(runtime.Object) (runtime.Object, error)) (runtime.Object, error) {
	e.once.Do(func() { e.obj, e.err = fn(e.src) })
	return e.obj, e.err
}

func annotatedCopy(obj runtime.Object, m objectMeta) runtime.Object {
	cp := obj.DeepCopyObject()
	annotateObject(cp, m)
	return cp
}

// bufferWatcher is a drop-on-overflow watch sink.
type bufferWatcher struct {
	mu      sync.Mutex
	ch      chan watch.Event
	done    chan struct{}
	stopped bool
	onStop  func()
}

func newBufferWatcher() *bufferWatcher {
	return &bufferWatcher{
		ch:   make(chan watch.Event, 100),
		done: make(chan struct{}),
	}
}

func (w *bufferWatcher) ResultChan() <-chan watch.Event { return w.ch }

func (w *bufferWatcher) Stop() {
	if w.close() && w.onStop != nil {
		w.onStop()
	}
}

// close closes the watcher channels once. It returns true if this call
// performed the close.
func (w *bufferWatcher) close() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return false
	}
	w.stopped = true
	close(w.ch)
	close(w.done)
	return true
}

func (w *bufferWatcher) send(ev watch.Event) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return false
	}
	select {
	case w.ch <- ev:
		return true
	default:
		return false
	}
}
