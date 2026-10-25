//go:build js && wasm

package main

import (
	"context"
	"encoding/json"
	"errors"
	"syscall/js"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
)

// installTokenRefresher hands OAuth token refreshes to the page's refresh
// worker (scripts/browser/oauth_refresh_worker.js) when the kcp worker
// provides __kcpRefreshTokens(requestJSON) -> Promise<responseJSON>. The
// refresh then runs on its own thread, so a busy Go runtime cannot delay the
// token response and lose a rotated (single use) refresh token.
func installTokenRefresher() {
	fn := js.Global().Get("__kcpRefreshTokens")
	if fn.Type() != js.TypeFunction {
		return
	}
	oauthclient.SetTokenRefresher(oauthclient.TokenRefresherFunc(
		func(ctx context.Context, req oauthclient.TokenRefreshRequest) (oauthclient.TokenRefreshResponse, error) {
			body, err := json.Marshal(req)
			if err != nil {
				return oauthclient.TokenRefreshResponse{}, err
			}
			type result struct {
				text string
				err  error
			}
			done := make(chan result, 1)
			var then, catch js.Func
			then = js.FuncOf(func(_ js.Value, args []js.Value) any {
				done <- result{text: args[0].String()}
				return nil
			})
			catch = js.FuncOf(func(_ js.Value, args []js.Value) any {
				done <- result{err: errors.New("oauth refresh worker: " + js.Global().Get("String").Invoke(args[0]).String())}
				return nil
			})
			defer then.Release()
			defer catch.Release()
			fn.Invoke(string(body)).Call("then", then, catch)
			var r result
			select {
			case r = <-done:
			case <-ctx.Done():
				// The worker still finishes and stores the rotated set; Go
				// re-reads its store before the next refresh.
				return oauthclient.TokenRefreshResponse{}, ctx.Err()
			}
			if r.err != nil {
				return oauthclient.TokenRefreshResponse{}, r.err
			}
			// {"error": code, ...}: TokenRefreshError maps invalid_grant and
			// pass_through to ErrSessionExpired and ErrRefreshPassThrough.
			var failed oauthclient.TokenRefreshError
			if json.Unmarshal([]byte(r.text), &failed) == nil && failed.Code != "" {
				return oauthclient.TokenRefreshResponse{}, &failed
			}
			var out oauthclient.TokenRefreshResponse
			if err := json.Unmarshal([]byte(r.text), &out); err != nil {
				return oauthclient.TokenRefreshResponse{}, err
			}
			return out, nil
		}))
}
