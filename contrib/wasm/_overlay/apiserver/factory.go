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

// Package factory is a build-time overlay replacement for
// k8s.io/apiserver/pkg/storage/storagebackend/factory. It adds a "memory"
// backend selected by a ServerList entry of the form memory://<name>.
//
// The backend is the in-memory storage.Interface implementation in
// contrib/wasm/storage. All resources of one apiserver share the store named
// by the URL, so resource versions and watches form a single sequence.
package factory

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/etcd3/metrics"
	"k8s.io/apiserver/pkg/storage/storagebackend"

	memstore "github.com/kcp-dev/kcp/contrib/wasm/storage"
)

// DestroyFunc is to destroy any resources used by the storage returned in Create() together.
type DestroyFunc func()

// Create creates a storage backend based on given config.
func Create(c storagebackend.ConfigForResource, newFunc, newListFunc func() runtime.Object, resourcePrefix string) (storage.Interface, DestroyFunc, error) {
	switch c.Type {
	case storagebackend.StorageTypeETCD2:
		return nil, nil, fmt.Errorf("%s is no longer a supported storage backend", c.Type)
	case storagebackend.StorageTypeUnset, storagebackend.StorageTypeETCD3:
		if name, ok := memoryStoreName(c.Config); ok {
			store, err := memstore.Prefixed(memstore.Shared(name), c.Prefix, resourcePrefix, newFunc, c.Codec)
			if err != nil {
				return nil, nil, err
			}
			return store, func() {}, nil
		}
		return newETCD3Storage(c, newFunc, newListFunc, resourcePrefix)
	default:
		return nil, nil, fmt.Errorf("unknown storage type: %s", c.Type)
	}
}

// CreateHealthCheck creates a healthcheck function based on given config.
func CreateHealthCheck(c storagebackend.Config, stopCh <-chan struct{}) (func() error, error) {
	switch c.Type {
	case storagebackend.StorageTypeETCD2:
		return nil, fmt.Errorf("%s is no longer a supported storage backend", c.Type)
	case storagebackend.StorageTypeUnset, storagebackend.StorageTypeETCD3:
		if _, ok := memoryStoreName(c); ok {
			return func() error { return nil }, nil
		}
		return newETCD3HealthCheck(c, stopCh)
	default:
		return nil, fmt.Errorf("unknown storage type: %s", c.Type)
	}
}

// CreateReadyCheck creates a readiness check based on given config.
func CreateReadyCheck(c storagebackend.Config, stopCh <-chan struct{}) (func() error, error) {
	switch c.Type {
	case storagebackend.StorageTypeETCD2:
		return nil, fmt.Errorf("%s is no longer a supported storage backend", c.Type)
	case storagebackend.StorageTypeUnset, storagebackend.StorageTypeETCD3:
		if _, ok := memoryStoreName(c); ok {
			return func() error { return nil }, nil
		}
		return newETCD3ReadyCheck(c, stopCh)
	default:
		return nil, fmt.Errorf("unknown storage type: %s", c.Type)
	}
}

// CreateProber creates a prober based on given config.
func CreateProber(c storagebackend.Config) (Prober, error) {
	switch c.Type {
	case storagebackend.StorageTypeETCD2:
		return nil, fmt.Errorf("%s is no longer a supported storage backend", c.Type)
	case storagebackend.StorageTypeUnset, storagebackend.StorageTypeETCD3:
		if _, ok := memoryStoreName(c); ok {
			return memoryProber{}, nil
		}
		return newETCD3ProberMonitor(c)
	default:
		return nil, fmt.Errorf("unknown storage type: %s", c.Type)
	}
}

// CreateMonitor creates a metrics monitor based on given config.
func CreateMonitor(c storagebackend.Config) (metrics.Monitor, error) {
	switch c.Type {
	case storagebackend.StorageTypeETCD2:
		return nil, fmt.Errorf("%s is no longer a supported storage backend", c.Type)
	case storagebackend.StorageTypeUnset, storagebackend.StorageTypeETCD3:
		if _, ok := memoryStoreName(c); ok {
			return memoryMonitor{}, nil
		}
		return newETCD3ProberMonitor(c)
	default:
		return nil, fmt.Errorf("unknown storage type: %s", c.Type)
	}
}

// Prober is an interface that defines the Probe function for doing etcd readiness/liveness checks.
type Prober interface {
	Probe(ctx context.Context) error
	Close() error
}

type memoryProber struct{}

func (memoryProber) Probe(context.Context) error { return nil }
func (memoryProber) Close() error                { return nil }

type memoryMonitor struct{}

func (memoryMonitor) Monitor(context.Context) (metrics.StorageMetrics, error) {
	return metrics.StorageMetrics{}, nil
}
func (memoryMonitor) Close() error { return nil }

// memoryStoreName reports whether the config selects the memory backend, and
// returns the shared store name. memory:// alone maps to "default".
func memoryStoreName(c storagebackend.Config) (string, bool) {
	if len(c.Transport.ServerList) != 1 {
		return "", false
	}
	server := c.Transport.ServerList[0]
	if !strings.HasPrefix(server, "memory://") {
		return "", false
	}
	name := strings.TrimPrefix(server, "memory://")
	if name == "" {
		name = "default"
	}
	return name, true
}
