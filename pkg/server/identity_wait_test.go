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
	"errors"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/kcp-dev/logicalcluster/v3"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	"github.com/kcp-dev/sdk/apis/core"
)

// failUntil returns a resolve func that fails until ready is closed.
func failUntil(ready <-chan struct{}, calls *atomic.Int32) func(context.Context) error {
	return func(context.Context) error {
		calls.Add(1)
		select {
		case <-ready:
			return nil
		default:
			return errors.New("APIExport tenancy.kcp.io is not ready")
		}
	}
}

func TestResolveUntilDoneRetriesOnChange(t *testing.T) {
	t.Parallel()
	ready := make(chan struct{})
	changed := make(chan struct{}, 1)
	var calls atomic.Int32

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- resolveUntilDone(context.Background(), changed, time.Hour, failUntil(ready, &calls))
	}()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	close(ready)
	changed <- struct{}{}

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("resolve did not retry on the change signal")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %v, want the change signal, not the fallback", elapsed)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("resolve calls = %d, want 2", got)
	}
}

func TestResolveUntilDoneFallsBackToPolling(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	resolve := func(context.Context) error {
		if calls.Add(1) < 3 {
			return errors.New("not ready")
		}
		return nil
	}
	if err := resolveUntilDone(context.Background(), nil, 5*time.Millisecond, resolve); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("resolve calls = %d, want 3", got)
	}
}

func TestResolveUntilDoneStopsOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	never := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := resolveUntilDone(ctx, nil, time.Hour, failUntil(never, &calls))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

type fakeRegistrar struct {
	handler cache.ResourceEventHandler
	removed bool
}

func (f *fakeRegistrar) AddEventHandler(h cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	f.handler = h
	return nil, nil
}

func (f *fakeRegistrar) RemoveEventHandler(cache.ResourceEventHandlerRegistration) error {
	f.removed = true
	return nil
}

func apiExport(cluster logicalcluster.Name) *apisv1alpha2.APIExport {
	return &apisv1alpha2.APIExport{ObjectMeta: metav1.ObjectMeta{
		Name:        "tenancy.kcp.io",
		Annotations: map[string]string{logicalcluster.AnnotationKey: cluster.String()},
	}}
}

func TestNotifyOnRootAPIExportChange(t *testing.T) {
	t.Parallel()
	reg := &fakeRegistrar{}
	changed, stop, err := notifyOnRootAPIExportChange(reg)
	if err != nil {
		t.Fatal(err)
	}

	pending := func() bool {
		select {
		case <-changed:
			return true
		default:
			return false
		}
	}

	reg.handler.OnAdd(apiExport("someworkspace"), false)
	if pending() {
		t.Error("an APIExport outside root signalled")
	}
	// Events coalesce into one pending signal.
	reg.handler.OnAdd(apiExport(core.RootCluster), false)
	reg.handler.OnUpdate(apiExport(core.RootCluster), apiExport(core.RootCluster))
	if !pending() {
		t.Error("a root APIExport did not signal")
	}
	if pending() {
		t.Error("signals did not coalesce")
	}
	reg.handler.OnUpdate(apiExport(core.RootCluster), apiExport(core.RootCluster))
	if !pending() {
		t.Error("a root APIExport update did not signal")
	}

	stop()
	if !reg.removed {
		t.Error("stop did not remove the handler")
	}
}
