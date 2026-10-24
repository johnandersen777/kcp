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
	"context"
	"net"
	"net/http"

	utilnet "k8s.io/apimachinery/pkg/util/net"
)

// loopbackDial is set by the js/wasm boot path. When non-nil, the loopback
// client dials an in-process pipe instead of a socket.
var loopbackDial func(ctx context.Context, network, address string) (net.Conn, error)

func wrapLoopbackDial(rt http.RoundTripper) http.RoundTripper {
	if loopbackDial == nil {
		return rt
	}
	for {
		switch t := rt.(type) {
		case *http.Transport:
			t.DialContext = loopbackDial
			return rt
		case utilnet.RoundTripperWrapper:
			rt = t.WrappedRoundTripper()
		default:
			return rt
		}
	}
}
