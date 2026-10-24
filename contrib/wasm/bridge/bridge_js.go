//go:build js && wasm

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

// Package bridge wires the WASM kcp control plane to the Deno host. It is
// used by the js/wasm build of ./cmd/kcp (cmd/kcp/main_wasm.go).
package bridge

import (
	"fmt"
	"os"
	"syscall/js"

	"github.com/kcp-dev/kcp/contrib/wasm/server"
)

// Run starts the WASM kcp shard, registers the JS entrypoints the Deno host
// calls, and blocks forever.
//
// The Deno host calls kcpHandleRequest(requestJSON, callbackID) for every
// HTTP request. The Go side handles the request in a goroutine and calls
// __kcpResolve(callbackID, responseJSON) when done. The callback split is
// required because the Fetch controller's outbound HTTP request is
// asynchronous on js/wasm; blocking the JS callback would prevent the Go
// runtime from delivering the fetch result.
func Run() {
	s := server.New()

	// Restore the snapshot Deno loaded from its state file before the
	// controller starts or any request is served.
	if initial := js.Global().Get("__kcpInitialState"); initial.Type() == js.TypeString {
		if raw := initial.String(); raw != "" {
			if err := s.RestoreSnapshot([]byte(raw)); err != nil {
				fmt.Fprintln(os.Stderr, "kcp: could not restore snapshot:", err)
			} else {
				fmt.Fprintln(os.Stderr, "kcp: restored snapshot from Deno")
			}
		}
	}

	// Hand every mutation's snapshot to the Deno host, which owns
	// persistence (atomic file write in scripts/kcp_wasm.ts).
	s.SetSnapshotHandler(func(snapshot []byte) {
		persist := js.Global().Get("__kcpPersist")
		if persist.Type() == js.TypeFunction {
			persist.Invoke(string(snapshot))
		}
	})

	s.Start()

	handle := js.FuncOf(func(this js.Value, args []js.Value) any {
		if len(args) < 2 {
			return nil
		}
		reqJSON := args[0].String()
		id := args[1].Int()

		go func() {
			out := s.HandleJSON(reqJSON)
			js.Global().Call("__kcpResolve", id, out)
		}()
		return nil
	})

	js.Global().Set("kcpHandleRequest", handle)
	js.Global().Set("__kcpWasmReady", true)
	fmt.Fprintln(os.Stderr, "kcp: WASM Fetch CRD API shard registered")
	select {}
}
