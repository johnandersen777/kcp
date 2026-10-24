//go:build js

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

// js/wasm replacement for serving_unix.go. Socket reuse options do not exist
// in the wasm net stack (the Deno host owns real sockets).
package options

import "syscall"

func permitPortReuse(network, addr string, conn syscall.RawConn) error { return nil }

func permitAddressReuse(network, addr string, conn syscall.RawConn) error { return nil }
