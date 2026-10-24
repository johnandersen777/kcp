# ingressws

Go-facing notes for the fedproxy ingress adapter used by the in-browser
socialweb/local-PDS instance (M5 groundwork).

Browsers own the WebSocket API, so the client half lives in
`scripts/browser/ingress_ws.ts`. It registers with the relay's
`com.fedproxy.temp.xrpc.subscribe` endpoint:

1. POST `com.fedproxy.temp.xrpc.getRegistrationNonce` with
   `{"key": <did>, "signatures": []}` and an AT Protocol service-auth bearer
   token.
2. Sign the returned base64 nonce bytes and build a
   `com.fedproxy.temp.xrpc.registration` object.
3. Open `/xrpc/com.fedproxy.temp.xrpc.subscribe` with `registration`, `did`,
   and `service_auth` query parameters.

The adapter answers `request` frames by calling the injected handler (status,
body, contentType, headers) and writing a `response` frame. It also handles the
relay keepalive path, reconnects with a fresh registration, and exposes the
other subscription frame kinds through `onFrame`.

## Integration

The Go js/wasm build will call this adapter through host functions: JS forwards
inbound requests to Go, Go dispatches them through the existing kcp HTTP stack,
and the response bytes go back to JS as a `response` frame. No Go WebSocket
implementation is needed.

Wire constants match
`socialweb-computer-kcp/internal/atproto/ingress/protocol.go`:
`com.fedproxy.temp.xrpc.subscribe`, `...getRegistrationNonce`, and
`com.fedproxy.temp.xrpc.registration`.

Tests: `deno test scripts/browser/ingress_ws_test.js` from the repository root.
