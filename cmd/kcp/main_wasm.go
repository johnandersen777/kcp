//go:build js && wasm && !kcprealserver

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

// This is the js/wasm build mode of the kcp command. The native control
// plane (kcp.go) requires a real etcd transport and the ability to listen on
// sockets, neither of which exists in Go's wasm ports. For js/wasm, kcp is
// built as a kcp-compatible shard whose API surface (discovery, CRUD, status
// subresource, patches, Table conversion) and Fetch CRD are served through
// the Deno host in scripts/.
//
// Build with:
//
//	GOOS=js GOARCH=wasm go build -o bin/wasm/kcp.wasm ./cmd/kcp
package main

import "github.com/kcp-dev/kcp/contrib/wasm/bridge"

func main() {
	bridge.Run()
}
