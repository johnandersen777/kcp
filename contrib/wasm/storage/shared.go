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

import "sync"

var (
	sharedMu     sync.Mutex
	sharedStores = map[string]*Store{}
)

// Shared returns the process-wide store with the given name, creating it on
// first use. The apiserver's memory://<name> storage backend maps to this so
// that all resources (and all logical clusters) of one apiserver share one
// resource-version sequence and one watch stream.
func Shared(name string) *Store {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if s, ok := sharedStores[name]; ok {
		return s
	}
	s := New()
	sharedStores[name] = s
	return s
}

// SharedStores returns a snapshot of the named shared stores.
func SharedStores() map[string]*Store {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	out := make(map[string]*Store, len(sharedStores))
	for name, s := range sharedStores {
		out[name] = s
	}
	return out
}
