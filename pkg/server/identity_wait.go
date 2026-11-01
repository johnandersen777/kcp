/*
Copyright 2026 The kcp Authors.

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
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/kcp-dev/logicalcluster/v3"
	"github.com/kcp-dev/sdk/apis/core"
)

// identityResolveFallback bounds the wait between two identity resolves when
// no APIExport event arrives (the old fixed poll interval).
const identityResolveFallback = 500 * time.Millisecond

// eventHandlerRegistrar is the part of a shared informer that
// notifyOnRootAPIExportChange needs.
type eventHandlerRegistrar interface {
	AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error)
	RemoveEventHandler(handle cache.ResourceEventHandlerRegistration) error
}

// notifyOnRootAPIExportChange returns a channel that receives (coalesced) when
// an APIExport in the root workspace is added or updated, e.g. when the
// identity controller writes its status.identityHash. stop removes the
// handler.
func notifyOnRootAPIExportChange(informer eventHandlerRegistrar) (changed <-chan struct{}, stop func(), err error) {
	ch := make(chan struct{}, 1)
	notify := func(obj any) {
		o, ok := obj.(metav1.Object)
		if !ok || logicalcluster.From(o) != core.RootCluster {
			return
		}
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	reg, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    notify,
		UpdateFunc: func(_, obj any) { notify(obj) },
	})
	if err != nil {
		return nil, nil, err
	}
	return ch, func() { _ = informer.RemoveEventHandler(reg) }, nil
}

// resolveUntilDone calls resolve until it succeeds or ctx ends. After a failed
// attempt it retries as soon as changed receives, and at the latest after
// fallback, so a resolve that ran before the APIExports had identities retries
// on the status update instead of a poll interval later.
func resolveUntilDone(ctx context.Context, changed <-chan struct{}, fallback time.Duration, resolve func(context.Context) error) error {
	logger := klog.FromContext(ctx)
	timer := time.NewTimer(fallback)
	defer timer.Stop()
	for {
		err := resolve(ctx)
		if err == nil {
			return nil
		}
		logger.V(3).Info("failed to resolve identities, keeping trying", "err", err)

		timer.Reset(fallback)
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %w)", ctx.Err(), err)
		case <-changed:
		case <-timer.C:
		}
	}
}
