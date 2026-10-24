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

// js/wasm replacement for util/ssrf. Go's js/wasm HTTP transport only uses the
// Fetch API when Transport has no custom dialer; SSRF dialing is meaningless in
// a browser, where the host enforces network policy.
package ssrf

import (
	"net"
	"net/http"
	"time"
)

// PublicOnlyDialer returns a plain dialer; the browser's fetch handles egress.
func PublicOnlyDialer() *net.Dialer {
	return &net.Dialer{Timeout: 30 * time.Second}
}

// PublicOnlyTransport returns a transport without a custom DialContext so Go's
// js/wasm net/http uses fetch().
func PublicOnlyTransport() *http.Transport {
	return &http.Transport{}
}
