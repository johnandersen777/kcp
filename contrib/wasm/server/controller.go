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
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kstorage "k8s.io/apiserver/pkg/storage"

	fetchv1alpha1 "github.com/kcp-dev/kcp/contrib/wasm/apis/fetch/v1alpha1"
)

const (
	defaultFetchTimeout = 30 * time.Second
	maxBodyBytes        = 1 << 20 // 1MiB
)

func (s *Server) enqueue(name string) {
	s.mu.Lock()
	if _, ok := s.queued[name]; ok {
		s.mu.Unlock()
		return
	}
	s.queued[name] = struct{}{}
	s.mu.Unlock()

	select {
	case s.queue <- name:
	case <-s.stopCh:
	}
}

func (s *Server) controlLoop() {
	for {
		select {
		case <-s.stopCh:
			return
		case name := <-s.queue:
			s.mu.Lock()
			delete(s.queued, name)
			s.mu.Unlock()
			s.reconcile(name)
		}
	}
}

// reconcile is the Fetch controller: it turns a non-empty spec.url into a
// fetched status.body. The initial phase of every object is Unresolved; while
// the request is in flight the phase is Fetching, and it terminates in
// Resolved or Error.
func (s *Server) reconcile(name string) {
	ctx := context.Background()

	var current fetchv1alpha1.Fetch
	if err := s.store.Get(ctx, fetchKey(name), kstorage.GetOptions{}, &current); err != nil {
		// Deleted, or the store is unavailable: nothing to do.
		return
	}
	if current.Status.ObservedGeneration == current.Generation && current.Status.Phase != fetchv1alpha1.FetchPhaseUnresolved {
		// Already handled this generation.
		return
	}
	if strings.TrimSpace(current.Spec.URL) == "" {
		// Nothing to fetch: stay Unresolved until a URL is set.
		return
	}

	gen := current.Generation
	method := current.Spec.Method
	rawURL := current.Spec.URL

	// Mark the object as being fetched.
	var inFlight fetchv1alpha1.Fetch
	err := s.store.GuaranteedUpdate(ctx, fetchKey(name), &inFlight, false, nil,
		func(input runtime.Object, _ kstorage.ResponseMeta) (runtime.Object, *uint64, error) {
			f := input.(*fetchv1alpha1.Fetch)
			if f.Generation != gen {
				return f, nil, nil
			}
			next := f.DeepCopy()
			next.Status.Phase = fetchv1alpha1.FetchPhaseFetching
			next.Status.ObservedGeneration = gen
			return next, nil, nil
		}, nil)
	if err != nil {
		return
	}

	result := s.doFetch(method, rawURL)

	stale := false
	var updated fetchv1alpha1.Fetch
	err = s.store.GuaranteedUpdate(ctx, fetchKey(name), &updated, false, nil,
		func(input runtime.Object, _ kstorage.ResponseMeta) (runtime.Object, *uint64, error) {
			f := input.(*fetchv1alpha1.Fetch)
			if f.Generation != gen {
				// The spec changed while we were fetching.
				stale = true
				return f, nil, nil
			}
			next := f.DeepCopy()
			next.Status.Phase = result.phase
			next.Status.StatusCode = result.statusCode
			next.Status.Body = result.body
			next.Status.ContentType = result.contentType
			next.Status.Message = result.message
			next.Status.ObservedGeneration = gen
			now := metav1.Now()
			next.Status.FetchedAt = &now
			return next, nil, nil
		}, nil)
	if err != nil {
		return
	}
	if stale {
		s.enqueue(name)
	}
}

type fetchResult struct {
	phase       fetchv1alpha1.FetchPhase
	statusCode  int
	body        string
	contentType string
	message     string
}

// doFetch uses the standard library http.Client with its default transport.
// On js/wasm that transport is implemented on top of the JS Fetch API, so
// outbound requests are performed by the Deno host without any per-call-site
// transport plumbing.
func (s *Server) doFetch(method, rawURL string) fetchResult {
	if strings.TrimSpace(method) == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequest(strings.ToUpper(method), rawURL, nil)
	if err != nil {
		return fetchResult{phase: fetchv1alpha1.FetchPhaseError, message: "invalid request: " + err.Error()}
	}
	req.Header.Set("User-Agent", "kcp-wasm-fetch-controller")

	resp, err := s.client.Do(req)
	if err != nil {
		return fetchResult{phase: fetchv1alpha1.FetchPhaseError, message: "fetch failed: " + err.Error()}
	}
	defer resp.Body.Close()

	b, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))

	result := fetchResult{
		phase:       fetchv1alpha1.FetchPhaseResolved,
		statusCode:  resp.StatusCode,
		contentType: resp.Header.Get("Content-Type"),
		body:        string(b),
	}
	if len(b) > maxBodyBytes {
		result.body = string(b[:maxBodyBytes])
		result.message = fmt.Sprintf("response body truncated to %d bytes", maxBodyBytes)
	}
	if readErr != nil {
		result.phase = fetchv1alpha1.FetchPhaseError
		result.message = "reading response body: " + readErr.Error()
		return result
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		result.phase = fetchv1alpha1.FetchPhaseError
		result.message = fmt.Sprintf("unexpected HTTP status %s", resp.Status)
	}
	return result
}
