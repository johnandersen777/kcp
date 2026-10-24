//go:build !js

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

// Package embeddedetcd re-exports the embedded etcd server for native builds.
// The js/wasm build replaces it with an inert stub (see embedded_js.go) so
// that etcd's bbolt/mmap/fsync dependency tree is not compiled into WASM.
package embeddedetcd

import (
	real "github.com/kcp-dev/embeddedetcd"
	etcdoptions "github.com/kcp-dev/embeddedetcd/options"
)

type Config = real.Config
type CompletedConfig = real.CompletedConfig
type Server = real.Server

func NewConfig(o etcdoptions.CompletedOptions, enableWatchCache bool) (*Config, error) {
	return real.NewConfig(o, enableWatchCache)
}

func NewServer(c CompletedConfig) *Server {
	return real.NewServer(c)
}
