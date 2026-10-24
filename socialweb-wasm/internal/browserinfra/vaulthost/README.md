# browserinfra/vaulthost

Browser substitute for the OpenBaoServer/Vault controllers and for the vault
controller's bus provider. It runs no OpenBao process and dials nothing:
`vault.read` and `vault.write` resolve to one in-memory
[`browserinfra/vault.Store`](../vault/) per workspace.

The real wiring this mirrors lives in the read-only tree at
`internal/controllers/vault/{controller,provider}.go` and
`internal/controllers/openbaoserver/controller.go`.

## Wiring

```go
import (
    "github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/vaulthost"
    bvault "github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/vault"
)

ctl, err := vaulthost.Register(mgr, handlers, vaulthost.Options{Scope: scope.Contains})
if err != nil { ... }

// Share the same stores with the identity and credential surfaces.
store := ctl.Provider().Store(workspace)
secrets := bvault.NewSecrets(store)
credentials := bvault.NewCredentials(store)
```

`Register` registers two controllers (`vaulthost-server` for `OpenBaoServer`,
`vaulthost` for `Vault`) on the manager in its workspaces, using the
`hostprocesses` APIExport like the real controllers. `Options.Scope` is
optional; nil keeps the manager's workspace filter.

Persistence (JSON round-trippable, no IndexedDB access here):

```go
raw, _ := json.Marshal(ctl.Snapshot())   // or ctl.Provider().Snapshot()

var snapshot vaulthost.Snapshot
json.Unmarshal(raw, &snapshot)
ctl.Restore(snapshot)                    // or ctl.Provider().Restore(snapshot)
```

Snapshot shape: `{"workspaces":{"<workspace>":{"entries":[{"mount","path","version","data"}]}}}`.
`Restore` replaces store contents in place, so `*bvault.Store` pointers already
handed to `NewSecrets`/`NewCredentials` stay valid. A workspace missing from the
snapshot is cleared.

## Exact status written

`OpenBaoServer` (every reconcile, `metadata.generation` from the object):

| Field | Value |
| --- | --- |
| `status.observedGeneration` | `metadata.generation` |
| `status.ready` | `true` |
| `status.phase` | `Ready` (`OpenBaoServerPhaseReady`) |
| `status.initialized` | `true` |
| `status.sealed` | `false` |
| `status.address` | `memory://browser` |
| `status.version` | `browser-memory` |
| `status.refs.kvMount` | `secret` (matches `openbaoctl.KVMount`) |
| `status.conditions[Ready]` | `True`, `reason=Ready`, `observedGeneration=metadata.generation` |

This satisfies the real Vault controller's readiness test
(`server.status.ready && server.status.observedGeneration == server.metadata.generation`)
and the address/refs checks in `internal/app/compose`.

`Vault` (copies the backend, exactly like `internal/controllers/vault`):

| Field | Value |
| --- | --- |
| `status.observedGeneration` | `metadata.generation` of the Vault |
| `status.ready` | backend `ready && observedGeneration == generation` |
| `status.address`, `initialized`, `sealed`, `caBundle`, `version` | copied from the bound `OpenBaoServer` status |
| `status.conditions[BackendBound]` | `True reason=Bound`, or `False reason=BackendUnbound` |
| `status.conditions[Ready]` | `True reason=BackendReady`, or `False reason=BackendNotReady` |

Only `spec.backendRef` kinds `OpenBaoServer` in the same workspace are bound;
`RemoteOpenBao` is not. A Vault whose backend is missing or not Ready requeues
every 2 seconds (`RequeuePending`). A finalizer
(`socialweb.computer/vaulthost`) unbinds the provider before the Vault is
deleted. There is no backend-to-Vault watch: the substitute backend turns Ready
on its first reconcile, so the requeue only covers apply ordering.

## Bus capabilities

`Provider.Register(bus)` uses the same GroupKind
(`socialweb.computer/Vault`), capability names, and
`internal/contract/vault.KV` type as the real provider:

| Capability | Resolves to |
| --- | --- |
| `vault.read` | `contractvault.KV` whose `Read` reads the store; `Write`/`Delete` return `ErrReadOnly` |
| `vault.write` | `contractvault.KV` reading and writing the store |

Resolution is per Vault object ref `(workspace, name)`. The store is the
workspace's shared `*vault.Store`; the default mount comes from the bound
OpenBaoServer's `status.refs.kvMount` (fallback `secret`). An empty `mount` in a
KV call uses that default. Scope is enforced like `openbao.Handle`: entries are
`mount`/`mount/path` prefixes or `*`, and the target is `mount + "/" + path`
after stripping `mount/data/` or `mount/metadata/`. Unbound refs return
`ErrUnbound`; out-of-scope paths return `ErrOutOfScope`.

Deliberately not provided (no OpenBao process, no leases, no PKI):
`vault.lease`, `vault.pki.issue`, `vault.write`-as-`Keys`, and Trust. The
`vault.KV` contract has no `List`; use `Store.List` directly if needed.

## Tests and build

```
cd socialweb-wasm
go test ./internal/browserinfra/vaulthost/...
GOOS=js GOARCH=wasm go build ./internal/browserinfra/vaulthost/
```

The tests cover status construction (`ServerStatus`, `VaultStatus`, `MountOf`)
and capability lookup end to end through `bus.New`/`bus.Resolve` (read-only
enforcement, default mount, scope, unbind, snapshot/restore). Reconcilers are
not driven directly because `ctlruntime.Request` has an unexported client
scope; their logic is the pure status functions plus provider
`Bind`/`Unbind`.
