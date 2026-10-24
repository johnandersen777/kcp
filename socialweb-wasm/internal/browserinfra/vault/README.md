# browserinfra/vault

In-memory, browser-side substitute for the OpenBao-backed vault contract.
It implements three read-only-tree interfaces over one concurrency-safe store:

| Interface | Type | Source |
| --- | --- | --- |
| `vault.KV` (`Read`/`Write`/`Delete`) | `*Store` | `internal/contract/vault` |
| `atproto.SecretStore` (`Get`/`Put`/`Delete`) | `*Secrets` | `internal/marketidentity/atproto` |
| `component.CredentialResolver` + `CredentialWriter` + `CredentialDeleter` | `*Credentials` | `internal/component` |

No JavaScript, no network, no OpenBao client calls. The only use of the
read-only `internal/vault/openbao` package is its `ErrNotFound` sentinel, so
existing `errors.Is(err, openbao.ErrNotFound)` checks keep working.

## Intended integration

- One `*Store` per browser workspace (or tab profile). Share it between
  `NewSecrets(store)` and `NewCredentials(store)` so identity OAuth state,
  PDS sessions, and component credentials all see the same memory.
- The eventual OpenBaoServer substitute controller should serve
  `vault.CapabilityRead` / `vault.CapabilityWrite` through the bus from this
  store, mirroring the `bus.Provide` wiring in
  `internal/controllers/vault/provider.go`, but resolving to the local store
  instead of dialing OpenBao. Do not route through `openbao.ServerOf`: it
  requires TLS material that does not exist in the browser.
- Persistence: `store.Snapshot()` is JSON-round-trippable
  (`{"entries":[{"mount","path","version","data"}]}`). Write it to
  IndexedDB/localStorage and call `store.Restore(snapshot)` on load. Restore
  replaces the whole store (no merge). Snapshot data is JSON-shaped; `[]byte`
  values are base64 through JSON.
- `Secrets`/`Credentials` expose `Snapshot`/`Restore` delegating to the same
  store.

## Semantics worth knowing

- Keys are the pair `(mount, path)`; both are required. Paths nest with `/`
  and `mount/data/...` or `mount/metadata/...` prefixes are accepted, matching
  `openbao.Handle`.
- `Store.List(ctx, mount, path)` returns immediate children, sorted, with a
  trailing `/` on directories. It is not part of `vault.KV`; it exists for the
  substitute controller's listing surface.
- `Delete` is idempotent (a missing key is not an error), matching OpenBao KV
  metadata deletes.
- Only the current version is retained. `Read`/`Resolve` with `version > 0`
  succeeds only when it equals the current version; otherwise `ErrNotFound`.
- `Secrets.Get` maps a missing or empty secret to `atproto.ErrSecretNotFound`.
  `SecretRef.Vault`, `SecretRef.Key`, and `CredentialRef.Server` are ignored
  (the browser has one store).
- Leases, PKI, Trust, and key rotation are not implemented; those capabilities
  stay unavailable in the browser.

## OpenBaoServer CRD status for Ready

The Vault controller sets its own Ready from
`server.status.ready && server.status.observedGeneration == server.metadata.generation`
(`internal/controllers/vault/controller.go`). A substitute controller must
write, on the `OpenBaoServer` status:

- `observedGeneration`: `metadata.generation` (required, or Vault never binds)
- `ready: true`
- `phase: Ready` (`OpenBaoServerPhaseReady`)
- `initialized: true`
- `sealed: false`
- `address`: non-empty, e.g. `memory://browser`
- `refs.kvMount`: the default KV mount used when a `vault.KV` call passes no
  mount (the provider's `defaultMount`)
- `conditions`: a `Ready=True` condition whose `observedGeneration` equals the
  object generation (convention; the OpenBaoServer path only reads `ready` and
  `observedGeneration`, but `RemoteOpenBao` and other consumers check
  conditions)
- `version`: optional, e.g. `browser-memory`

## Tests

```
cd socialweb-wasm && go test ./internal/browserinfra/vault/...
```
