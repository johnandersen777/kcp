//go:build js && wasm && kcprealserver

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

	k8soptions "k8s.io/apiserver/pkg/server/options"
)

// SetLoopbackDial makes every loopback client dial the in-process pipe.
// The k8s.io/apiserver options package is overlaid for js/wasm builds and
// exposes LoopbackDial for the client it creates inside BuildGenericConfig.
func SetLoopbackDial(dial func(ctx context.Context, network, address string) (net.Conn, error)) {
	loopbackDial = dial
	k8soptions.LoopbackDial = dial
}
