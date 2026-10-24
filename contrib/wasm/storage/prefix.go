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
}

var _ kstorage.Interface = (*prefixedStore)(nil)

// Prefixed wraps a shared store so that every key is namespaced under
// <prefix>/<resourcePrefix>, exactly like k8s.io/apiserver/pkg/storage/etcd3.
// All resources backed by the same Store share resource versions and watches.
// newFunc is used to build the object carried by the initial-events-end
// bookmark of watchlist requests.
func Prefixed(store *Store, prefix, resourcePrefix string, newFunc func() runtime.Object) (kstorage.Interface, error) {
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
	return &prefixedStore{
		store:          store,
		pathPrefix:     pathPrefix,
		resourcePrefix: resourcePrefix,
		newFunc:        newFunc,
	}, nil
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
	return p.store.Create(ctx, full, obj, out, ttl)
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
	return p.store.Delete(ctx, full, out, preconditions, validateDeletion, cachedExistingObject, opts)
}

func (p *prefixedStore) Watch(ctx context.Context, key string, opts kstorage.ListOptions) (watch.Interface, error) {
	full, err := p.key(key, opts.Recursive)
	if err != nil {
		return nil, err
	}
	return p.store.watch(ctx, full, opts, p.newFunc)
}

func (p *prefixedStore) Get(ctx context.Context, key string, opts kstorage.GetOptions, objPtr runtime.Object) error {
	full, err := p.key(key, false)
	if err != nil {
		return err
	}
	return p.store.Get(ctx, full, opts, objPtr)
}

func (p *prefixedStore) GetList(ctx context.Context, key string, opts kstorage.ListOptions, listObj runtime.Object) error {
	full, err := p.key(key, opts.Recursive)
	if err != nil {
		return err
	}
	return p.store.GetList(ctx, full, opts, listObj)
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
	return p.store.GuaranteedUpdate(ctx, full, destination, ignoreNotFound, preconditions, tryUpdate, cachedExistingObject)
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
