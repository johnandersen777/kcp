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
	"path"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	kstorage "k8s.io/apiserver/pkg/storage"
)

// prefixedStore mirrors the etcd3 store's key layout: the registry passes
// keys relative to a resource, and the backend prepends
// <global prefix>/<resource prefix>/ before touching the shared store.
type prefixedStore struct {
	store          *Store
	pathPrefix     string
	resourcePrefix string
	newFunc        func() runtime.Object
	codec          runtime.Codec
}

var _ kstorage.Interface = (*prefixedStore)(nil)

// Prefixed wraps a shared store so that every key is namespaced under
// <prefix>/<resourcePrefix>, exactly like k8s.io/apiserver/pkg/storage/etcd3.
// All resources backed by the same Store share resource versions and watches.
// newFunc is used to build the object carried by the initial-events-end
// bookmark of watchlist requests.
//
// codec is the resource's storage codec. The etcd3 store encodes every object
// with it on write and decodes it again on read, which re-applies
// structural-schema defaults that the API server deliberately does not persist
// (for example the default .status.phase of a Workspace whose status
// subresource strips status on create). The in-memory store keeps
// runtime.Objects instead of encoded bytes, so it performs the same round-trip
// on the read paths to remain behaviourally equivalent.
func Prefixed(store *Store, prefix, resourcePrefix string, newFunc func() runtime.Object, codec runtime.Codec) (kstorage.Interface, error) {
	pathPrefix := path.Join("/", prefix)
	if !strings.HasSuffix(pathPrefix, "/") {
		pathPrefix += "/"
	}
	if resourcePrefix == "" {
		return nil, fmt.Errorf("resourcePrefix cannot be empty")
	}
	if resourcePrefix == "/" {
		return nil, fmt.Errorf("resourcePrefix cannot be /")
	}
	if !strings.HasPrefix(resourcePrefix, "/") {
		return nil, fmt.Errorf("resourcePrefix needs to start from /")
	}
	if newFunc != nil {
		keyPrefix := pathPrefix + strings.TrimPrefix(resourcePrefix, "/") + "/"
		RegisterDecoder(keyPrefix, func(raw []byte) (runtime.Object, error) {
			obj := newFunc()
			if codec == nil {
				if err := json.Unmarshal(raw, obj); err != nil {
					return nil, err
				}
				return obj, nil
			}
			decoded, _, err := codec.Decode(raw, nil, obj)
			return decoded, err
		})
	}
	return &prefixedStore{
		store:          store,
		pathPrefix:     pathPrefix,
		resourcePrefix: resourcePrefix,
		newFunc:        newFunc,
		codec:          codec,
	}, nil
}

// applyDefaults round-trips obj through the resource codec so that the
// structural-schema defaults the etcd3 store re-applies on decode are also
// present for the in-memory backend. It returns obj unchanged when no codec is
// configured (the simplified Fetch-only wasm server).
func (p *prefixedStore) applyDefaults(obj runtime.Object) (out runtime.Object, err error) {
	if p.codec == nil || obj == nil {
		return obj, nil
	}
	// The CRD schema-coercing codec can panic when a dynamic object has no
	// GVK (observed on server-side apply in the browser shard). Defaults are
	// best-effort; never take the apiserver down for them.
	defer func() {
		if value := recover(); value != nil {
			out, err = obj, nil
		}
	}()
	data, err := runtime.Encode(p.codec, obj)
	if err != nil {
		return nil, fmt.Errorf("encoding %T: %w", obj, err)
	}
	// Decode into the type the caller requested. The store keeps one copy of
	// each object (at the storage version), while informers watch a served
	// version such as v1alpha1; decoding into the stored object's type would
	// hand them a v1alpha2 object and the reflector drops the event.
	target := obj
	if p.newFunc != nil {
		target = p.newFunc()
	}
	decoded, _, err := p.codec.Decode(data, nil, target)
	if err != nil {
		return nil, fmt.Errorf("decoding %T: %w", obj, err)
	}
	return decoded, nil
}

func (p *prefixedStore) key(key string, recursive bool) (string, error) {
	prepared, err := kstorage.PrepareKey(p.resourcePrefix, key, recursive)
	if err != nil {
		return "", err
	}
	return p.pathPrefix + strings.TrimPrefix(prepared, "/"), nil
}

func (p *prefixedStore) Versioner() kstorage.Versioner { return p.store.Versioner() }

// decoded is the reader's view of a stored object: the codec round-trip of
// applyDefaults. It depends only on the stored object and this reader's codec
// and target type (readers of different served versions share keys), so it
// is computed once per stored object and reader and then copied, instead of
// running the codec on every read and for every watcher. The result is
// shared and must not be modified.
func (p *prefixedStore) decoded(key string, stored runtime.Object) (runtime.Object, error) {
	if p.codec == nil {
		return stored, nil
	}
	return p.store.derive(p, key, stored, p.decode)
}

func (p *prefixedStore) decode(stored runtime.Object) (runtime.Object, error) {
	// Encoding may touch its input (the versioning codec sets and restores
	// the GVK), so never hand it the shared stored object.
	return p.applyDefaults(stored.DeepCopyObject())
}

// into copies the reader's view of a stored object into dst.
func (p *prefixedStore) into(key string, stored runtime.Object, m objectMeta, dst runtime.Object) error {
	obj, err := p.decoded(key, stored)
	if err != nil {
		return err
	}
	return intoAnnotated(obj, m, dst)
}

// view returns a caller-owned copy of the reader's view of a stored object.
func (p *prefixedStore) view(ref *storedRef) (runtime.Object, error) {
	obj, err := p.decoded(ref.key, ref.Object)
	if err != nil {
		return nil, err
	}
	return annotatedCopy(obj, ref.meta), nil
}

func (p *prefixedStore) Create(ctx context.Context, key string, obj, out runtime.Object, ttl uint64) error {
	full, err := p.key(key, false)
	if err != nil {
		return err
	}
	stored, m, err := p.store.create(ctx, full, obj, ttl)
	if err != nil || out == nil {
		return err
	}
	return p.into(full, stored, m, out)
}

func (p *prefixedStore) Delete(
	ctx context.Context,
	key string,
	out runtime.Object,
	preconditions *kstorage.Preconditions,
	validateDeletion kstorage.ValidateObjectFunc,
	cachedExistingObject runtime.Object,
	opts kstorage.DeleteOptions,
) error {
	full, err := p.key(key, false)
	if err != nil {
		return err
	}
	obj, m, err := p.store.delete(ctx, full, preconditions, validateDeletion)
	if err != nil || out == nil {
		return err
	}
	return p.into(full, obj, m, out)
}

func (p *prefixedStore) Watch(ctx context.Context, key string, opts kstorage.ListOptions) (watch.Interface, error) {
	full, err := p.key(key, opts.Recursive)
	if err != nil {
		return nil, err
	}
	return p.store.watch(ctx, full, opts, p.newFunc, p.view)
}

func (p *prefixedStore) Get(ctx context.Context, key string, opts kstorage.GetOptions, objPtr runtime.Object) error {
	full, err := p.key(key, false)
	if err != nil {
		return err
	}
	obj, m, err := p.store.get(full, opts)
	if err != nil || obj == nil {
		return err
	}
	return p.into(full, obj, m, objPtr)
}

func (p *prefixedStore) GetList(ctx context.Context, key string, opts kstorage.ListOptions, listObj runtime.Object) error {
	full, err := p.key(key, opts.Recursive)
	if err != nil {
		return err
	}
	return p.store.getList(full, opts, listObj, func(ref storedRef) (runtime.Object, error) {
		return p.view(&ref)
	})
}

func (p *prefixedStore) GuaranteedUpdate(
	ctx context.Context,
	key string,
	destination runtime.Object,
	ignoreNotFound bool,
	preconditions *kstorage.Preconditions,
	tryUpdate kstorage.UpdateFunc,
	cachedExistingObject runtime.Object,
) error {
	full, err := p.key(key, false)
	if err != nil {
		return err
	}
	// Hand the update function the decoded, defaulted object, exactly as the
	// etcd3 store does when it decodes the stored value. prepare runs under
	// the store lock, hence the locked lookup.
	prepare := func(stored runtime.Object) (runtime.Object, error) {
		if p.codec == nil {
			return stored, nil
		}
		return p.store.derivedEntryLocked(p, full, stored).get(p.decode)
	}
	stored, m, err := p.store.guaranteedUpdate(ctx, full, destination, ignoreNotFound, preconditions, tryUpdate, prepare)
	if err != nil || destination == nil {
		return err
	}
	return p.into(full, stored, m, destination)
}

func (p *prefixedStore) Stats(ctx context.Context) (kstorage.Stats, error) {
	return p.store.Stats(ctx)
}

func (p *prefixedStore) ReadinessCheck() error { return p.store.ReadinessCheck() }

func (p *prefixedStore) RequestWatchProgress(ctx context.Context) error {
	return p.store.RequestWatchProgress(ctx)
}

func (p *prefixedStore) GetCurrentResourceVersion(ctx context.Context) (uint64, error) {
	return p.store.GetCurrentResourceVersion(ctx)
}

func (p *prefixedStore) EnableResourceSizeEstimation(keysFunc kstorage.KeysFunc) error {
	return p.store.EnableResourceSizeEstimation(keysFunc)
}

func (p *prefixedStore) CompactRevision() int64 { return p.store.CompactRevision() }
