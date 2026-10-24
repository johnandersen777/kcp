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

//go:build !windows

// js/wasm replacement for pkg/util/umask.go. golang.org/x/sys/unix.Umask is
// not available in WebAssembly.
package util

// Umask is a wrapper for `unix.Umask()` on non-Windows platforms. There is no
// process umask in a WASM runtime, so it is a no-op.
func Umask(mask int) (old int, err error) {
	return 0, nil
}
