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

// js/wasm cannot perform DNS lookups (Go's wasm net is a fake in-process
// network). When DNS and HTTP well-known resolution fail, ask the public
// AppView's com.atproto.identity.resolveHandle endpoint instead.
package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

const resolveHandleAppView = "https://public.api.bsky.app"

func resolveHandleHTTPFallback(
	ctx context.Context,
	handle syntax.Handle,
) (syntax.DID, error, bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	endpoint := fmt.Sprintf(
		"%s/xrpc/com.atproto.identity.resolveHandle?handle=%s",
		resolveHandleAppView,
		url.QueryEscape(handle.String()),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err, true
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: resolveHandle: %w", ErrHandleResolutionFailed, err), true
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf(
			"%w: resolveHandle returned HTTP %d",
			ErrHandleResolutionFailed,
			resp.StatusCode,
		), true
	}

	var payload struct {
		DID string `json:"did"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("%w: %w", ErrHandleResolutionFailed, err), true
	}
	did, err := syntax.ParseDID(payload.DID)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrHandleResolutionFailed, err), true
	}
	return did, nil, true
}
