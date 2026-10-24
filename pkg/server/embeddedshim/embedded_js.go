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

package embeddedetcd

import (
	"context"

	etcdoptions "github.com/kcp-dev/embeddedetcd/options"
)

// Config is an inert stand-in for embeddedetcd.Config. In a WASM runtime
// there is no filesystem or socket stack to run an embedded etcd server, and
// the memory:// storage backend does not need one.
type Config struct {
	LogLevel string
}

// CompletedConfig mirrors the real package's nil/non-nil semantics: the
// embedded *Config is nil when no embedded etcd is configured.
type completedConfig struct {
	*Config
}

// CompletedConfig reports whether an embedded etcd server was configured.
type CompletedConfig struct {
	*completedConfig
}

// Complete implements the real package's API.
func (c *Config) Complete() CompletedConfig {
	return CompletedConfig{&completedConfig{Config: c}}
}

// NewConfig returns no embedded etcd configuration for WASM.
func NewConfig(o etcdoptions.CompletedOptions, enableWatchCache bool) (*Config, error) {
	return nil, nil
}

// Server is an inert stand-in for embeddedetcd.Server.
type Server struct{}

// NewServer returns an inert server.
func NewServer(c CompletedConfig) *Server { return &Server{} }

// Run does nothing; the memory storage backend needs no etcd server.
func (s *Server) Run(ctx context.Context) error { return nil }
