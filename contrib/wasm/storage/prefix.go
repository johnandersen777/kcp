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

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	kstorage "k8s.io/apiserver/pkg/storage"
	"k8s.io/klog/v2"
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

func (p *prefixedStore) Create(ctx context.Context, key string, obj, out runtime.Object, ttl uint64) error {
	full, err := p.key(key, false)
	if err != nil {
		return err
	}
	if err := p.store.Create(ctx, full, obj, out, ttl); err != nil {
		return err
	}
	if out != nil {
		decoded, err := p.applyDefaults(out)
		if err != nil {
			return err
		}
		if decoded != out {
			return copyObject(decoded, out)
		}
	}
	return nil
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
	if err := p.store.Delete(ctx, full, out, preconditions, validateDeletion, cachedExistingObject, opts); err != nil {
		return err
	}
	if out != nil {
		if _, err := p.applyDefaults(out); err != nil {
			return err
		}
	}
	return nil
}

func (p *prefixedStore) Watch(ctx context.Context, key string, opts kstorage.ListOptions) (watch.Interface, error) {
	full, err := p.key(key, opts.Recursive)
	if err != nil {
		return nil, err
	}
	w, err := p.store.watch(ctx, full, opts, p.newFunc)
	if err != nil {
		return nil, err
	}
	return watch.Filter(w, func(in watch.Event) (watch.Event, bool) {
		// Bookmarks and errors are not resource objects (bookmarks are built
		// from newFunc() and have no kind), so there is nothing to default.
		if in.Type == watch.Bookmark || in.Type == watch.Error || in.Object == nil {
			return in, true
		}
		decoded, err := p.applyDefaults(in.Object)
		if err != nil {
			// The object came out of the codec ourselves, so a failure here
			// means the codec rejects its own encoding; surfacing the event
			// undecoded is better than silently dropping a watch update.
			klog.FromContext(ctx).Error(err, "failed to apply defaults to watch event", "type", in.Type)
			return in, true
		}
		in.Object = decoded
		return in, true
	}), nil
}

func (p *prefixedStore) Get(ctx context.Context, key string, opts kstorage.GetOptions, objPtr runtime.Object) error {
	full, err := p.key(key, false)
	if err != nil {
		return err
	}
	if err := p.store.Get(ctx, full, opts, objPtr); err != nil {
		return err
	}
	decoded, err := p.applyDefaults(objPtr)
	if err != nil {
		return err
	}
	if decoded != objPtr {
		return copyObject(decoded, objPtr)
	}
	return nil
}

func (p *prefixedStore) GetList(ctx context.Context, key string, opts kstorage.ListOptions, listObj runtime.Object) error {
	full, err := p.key(key, opts.Recursive)
	if err != nil {
		return err
	}
	if err := p.store.GetList(ctx, full, opts, listObj); err != nil {
		return err
	}
	items, err := meta.ExtractList(listObj)
	if err != nil {
		return err
	}
	for i, item := range items {
		decoded, err := p.applyDefaults(item)
		if err != nil {
			return err
		}
		items[i] = decoded
	}
	return meta.SetList(listObj, items)
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
	// Re-apply defaults to the object handed to the update function, exactly
	// as the etcd3 store does when it decodes the stored value.
	prepare := func(stored runtime.Object) (runtime.Object, error) {
		return p.applyDefaults(stored.DeepCopyObject())
	}
	if err := p.store.guaranteedUpdate(ctx, full, destination, ignoreNotFound, preconditions, tryUpdate, prepare); err != nil {
		return err
	}
	if destination != nil {
		if _, err := p.applyDefaults(destination); err != nil {
			return err
		}
	}
	return nil
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
