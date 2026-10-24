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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	kstorage "k8s.io/apiserver/pkg/storage"

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
	if ttl != 0 {
		return fmt.Errorf("in-memory store does not support TTL")
	}
	if obj == nil {
		return fmt.Errorf("cannot create a nil object")
	}

	stored := obj.DeepCopyObject()

	s.mu.Lock()
	if _, exists := s.objects[key]; exists {
		rv := s.rv
		s.mu.Unlock()
		return kstorage.NewKeyExistsError(key, int64(rv))
	}
	s.rv++
	if err := s.versioner.UpdateObject(stored, s.rv); err != nil {
		s.rv--
		s.mu.Unlock()
		return err
	}
	s.objects[key] = stored
	s.objectMeta[key] = metaFromContext(ctx)
	s.recordEventLocked(key, watch.Added, stored)
	s.broadcastLocked(key, watch.Added, stored)
	noteMutation()
	snap := s.snapshotForHandlerLocked()
	s.mu.Unlock()

	s.persist(snap)
	if out != nil {
		if err := copyObject(stored, out); err != nil {
			return err
		}
		annotateObject(out, s.metaFor(key))
	}
	return nil
}

func (s *Store) metaFor(key string) objectMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.objectMeta[key]
}

// Get implements storage.Interface.
func (s *Store) Get(ctx context.Context, key string, opts kstorage.GetOptions, objPtr runtime.Object) error {
	s.mu.RLock()
	obj, exists := s.objects[key]
	rv := s.rv
	if exists {
		obj = obj.DeepCopyObject()
	}
	s.mu.RUnlock()

	if !exists {
		if opts.IgnoreNotFound {
			return nil
		}
		return kstorage.NewKeyNotFoundError(key, int64(rv))
	}
	if err := copyObject(obj, objPtr); err != nil {
		return err
	}
	s.mu.RLock()
	m := s.objectMeta[key]
	s.mu.RUnlock()
	annotateObject(objPtr, m)
	return nil
}

// GetList implements storage.Interface.
func (s *Store) GetList(ctx context.Context, key string, opts kstorage.ListOptions, listObj runtime.Object) error {
	type entry struct {
		key  string
		obj  runtime.Object
		meta objectMeta
	}

	s.mu.RLock()
	entries := make([]entry, 0, len(s.objects))
	for k, obj := range s.objects {
		if !matchesKey(k, key, opts.Recursive) {
			continue
		}
		if !predicateMatches(opts.Predicate, obj) {
			continue
		}
		entries = append(entries, entry{key: k, obj: obj.DeepCopyObject(), meta: s.objectMeta[k]})
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
		annotateObject(e.obj, e.meta)
		items = append(items, e.obj)
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
	if tryUpdate == nil {
		return fmt.Errorf("tryUpdate is required")
	}

	s.mu.Lock()
	existing, exists := s.objects[key]

	var input runtime.Object
	var responseMeta kstorage.ResponseMeta
	if exists {
		input = existing.DeepCopyObject()
		rv, err := s.versioner.ObjectResourceVersion(input)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		responseMeta.ResourceVersion = rv
		if preconditions != nil {
			if err := preconditions.Check(key, input); err != nil {
				s.mu.Unlock()
				return err
			}
		}
	} else {
		if !ignoreNotFound {
			rv := s.rv
			s.mu.Unlock()
			return kstorage.NewKeyNotFoundError(key, int64(rv))
		}
		// Create-on-update semantics: hand the caller a zero object.
		input = destination.DeepCopyObject()
	}

	output, _, err := tryUpdate(input, responseMeta)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if output == nil {
		output = input
	}

	if exists && objectsEqual(input, output) {
		s.mu.Unlock()
		return copyObject(output, destination)
	}

	s.rv++
	if err := s.versioner.UpdateObject(output, s.rv); err != nil {
		s.rv--
		s.mu.Unlock()
		return err
	}
	stored := output.DeepCopyObject()
	s.objects[key] = stored
	s.objectMeta[key] = metaFromContext(ctx)

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
	if err := copyObject(stored, destination); err != nil {
		return err
	}
	annotateObject(destination, s.metaFor(key))
	return nil
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
	s.mu.Lock()
	obj, exists := s.objects[key]
	if !exists {
		rv := s.rv
		s.mu.Unlock()
		return kstorage.NewKeyNotFoundError(key, int64(rv))
	}
	if preconditions != nil {
		if err := preconditions.Check(key, obj); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	if validateDeletion != nil {
		if err := validateDeletion(ctx, obj.DeepCopyObject()); err != nil {
			s.mu.Unlock()
			return err
		}
	}

	s.rv++
	delete(s.objects, key)
	s.recordEventLocked(key, watch.Deleted, obj)
	s.broadcastLocked(key, watch.Deleted, obj)
	m := s.objectMeta[key]
	delete(s.objectMeta, key)
	noteMutation()
	snap := s.snapshotForHandlerLocked()
	s.mu.Unlock()

	s.persist(snap)
	if out != nil {
		if err := copyObject(obj, out); err != nil {
			return err
		}
		annotateObject(out, m)
	}
	return nil
}

// Watch implements storage.Interface.
func (s *Store) Watch(ctx context.Context, key string, opts kstorage.ListOptions) (watch.Interface, error) {
	return s.watch(ctx, key, opts, nil)
}

// watch is Watch with an optional prototype constructor used to emit the
// initial-events-end BOOKMARK required by the WatchList protocol. The raw
// etcd3 store does the same; without it, informers using sendInitialEvents
// never finish initializing.
func (s *Store) watch(ctx context.Context, key string, opts kstorage.ListOptions, bookmark func() runtime.Object) (watch.Interface, error) {
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

	reg := &watchRegistration{key: key, recursive: opts.Recursive, predicate: opts.Predicate}
	s.watchers[w] = reg

	switch {
	case sendInitial || (fromRV && requestedRV < s.compactedRV):
		// No usable history: send the current state as ADDED events.
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
				w.send(watch.Event{Type: watch.Added, Object: annotatedCopy(obj, s.objectMeta[k])})
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
			w.send(watch.Event{Type: ev.typ, Object: annotatedCopy(ev.obj, ev.meta)})
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

	return w, nil
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
		obj:  obj.DeepCopyObject(),
		meta: s.objectMeta[key],
	})
	if len(s.events) > maxWatchHistory {
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
		if !w.send(watch.Event{Type: typ, Object: annotatedCopy(obj, s.objectMeta[key])}) {
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
	for key, obj := range s.objects {
		raw, err := json.Marshal(obj)
		if err != nil {
			return nil, fmt.Errorf("encoding %q: %w", key, err)
		}
		snap.Items[key] = raw
	}
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

func objectsEqual(a, b runtime.Object) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

func copyObject(src, dst runtime.Object) error {
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
