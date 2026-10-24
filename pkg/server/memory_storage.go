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

package server

import (
	"strings"

	"k8s.io/apiserver/pkg/storage/storagebackend"
)

// isMemoryStorageConfig reports whether the apiserver is backed by the
// in-memory storage backend (memory://<name>) instead of etcd. Components that
// require raw etcd access are disabled in that mode.
func isMemoryStorageConfig(t storagebackend.TransportConfig) bool {
	return len(t.ServerList) == 1 && strings.HasPrefix(t.ServerList[0], "memory://")
}
