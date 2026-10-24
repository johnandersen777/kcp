# browserstate

Snapshot contract shared by the browser demo and the js/wasm build.

The browser stores JSON strings under three keys (`kcp`, `socialweb`, `fs`)
through `scripts/browser/state_store.js`, which uses IndexedDB with an
in-memory fallback. Go wraps those strings in an envelope:

```json
{"version":1,"kcp":{...},"socialweb":{...},"fs":{...}}
```

`Envelope.Validate` rejects unknown versions, empty (or `null`) snapshots, and
invalid JSON. `ParseEnvelope` also rejects malformed envelopes.

`Backend` is the storage seam: `Load`, `Save`, `Delete`. `MemoryBackend` is the
test/fallback implementation and copies values on the way in and out.

## Integration

M5 will wire `Backend` to JS host functions that call `state_store.js`, so this
package never touches IndexedDB directly and the Go code stays portable to
js/wasm. Host functions should pass values through the same envelope validation
before handing bytes to the store.

Tests: `go test ./internal/browserstate/...` from `socialweb-wasm`.
