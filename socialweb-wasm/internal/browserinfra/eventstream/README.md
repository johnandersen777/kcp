# browserinfra/eventstream

Browser substitute for the `EventStream`, `EventSource`, `PdsEventSource`,
`RelayEventSource`, and `JetstreamEventSource` controllers, plus the market
event poller that replaces a watched firehose with a direct repo listing. It
opens no relay, Jetstream, or PDS connection.

The read-only tree this mirrors:

- `internal/controllers/eventstream/controller.go`
- `internal/controllers/eventsource/{controller,backend}.go`
- `internal/controllers/{pdseventsource,relayeventsource,jetstreameventsource}/controller.go`
- `api/events/v1alpha1/types_{eventstream,eventsource}.go`
- `internal/market/requester/inbox.go` (mailbox shape and names)

## Wiring

```go
import "github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/eventstream"

ctl, err := eventstream.Register(mgr, eventstream.Options{Scope: scope.Contains})
```

`Register` puts five controllers on the manager, each in the manager's
workspaces (or `Options.Scope`), all serving the `events` APIExport:

| Controller name | Kind | Watches |
| --- | --- | --- |
| `browser-eventstream` | `EventStream` | `EventSource` (requeues dependants) |
| `browser-eventsource` | `EventSource` | `PdsEventSource`, `RelayEventSource`, `JetstreamEventSource` |
| `browser-pdseventsource` | `PdsEventSource` | none |
| `browser-relayeventsource` | `RelayEventSource` | none |
| `browser-jetstreameventsource` | `JetstreamEventSource` | none |

Deletion only drops the in-memory index entry; there is no finalizer because
the substitute owns nothing outside the process.

## Driving the poller

The poller is not a controller and has no watch. The orchestrator calls it on
its own timer for each active contract, once the receipt URI is known:

```go
poll, err := eventstream.NewPoller(eventstream.PollerOptions{
    Lister:  repoStore, // *market.RepoStore or any market.RecordLister
    Creator: intents,   // anything with CreateStateSync(ctx, StateSync) error
})
if err != nil { ... }

for {
    created, err := poll.Poll(ctx, bidderDID, receiptURI)
    // log/backoff, then wait (for example 15s) or stop when the contract ends
}
```

`Lister` is exactly `internal/atproto/market.RepoStore.ListRecords`:

```go
ListRecords(ctx context.Context, repo, collection string, limit int, cursor string) (market.ListPage, error)
```

so a `market.NewRepoStore(market.RepoStoreConfig{DID: bidderDID, Open: ...})`
works as is. `Creator` is the `CreateStateSync` subset of
`internal/contract/market.StateSyncIntents`; the browser StateSync store can
implement it directly. `contractmarket.ErrExists` is treated as success.

One `Poller` should live for the orchestrator's lifetime: its dedupe set is
in-memory and keyed by the generated StateSync name. Calling `Poll` again is
cheap and safe; already-created mailboxes are skipped.

## Poller behaviour

- Lists `com.publicdomainrelay.temp.market.event` in the bidder's repo, 100 per
  page, at most 20 pages per `Poll` (the same bound as the real requester's
  `listAll`).
- Keeps an event when `value.receipt.uri == receiptURI` and the payload
  collection (`market.NSIDFromURI(value.payload.uri)`) is one of
  `...compute.events.vm.onNetwork`, `.hostKeys`, or `.terminated`. That is
  exactly the guest-event set in `internal/market/requester/events.go`.
- It reads the envelope's strong refs only. It resolves no record, verifies no
  signature, and creates no `atprotoRecordWatch`; the requester validates when
  it consumes the inbox.

Each accepted event becomes one `StateSync`, byte-for-byte the shape
`internal/market/requester`'s `mailbox()` writes:

| Field | Value |
| --- | --- |
| `metadata.name` | `inbox-` + hex(first 16 bytes of `sha256(receiptURI + "\x00" + eventURI + "#" + cid)`) |
| `metadata.labels` | `socialweb.computer/inbox: "true"` |
| `spec.sink.adapter` | `requester.inbox` |
| `spec.direction` | `Outbound` |
| `spec.idempotencyKey` | `inbox:<eventURI>#<cid>` |
| `spec.conflictPolicy` | `SourceWins` |
| `spec.deletionPolicy` | `Retain` |
| `spec.payload` | `{"event":{"uri":...,"cid":...,"receiptUri":...,"receivedAt":...}}` |

`receivedAt` uses `market.FormatTime(now)`. The name derivation matches
`requester.inboxName` so a mailbox created here is recognised there.

## Exact status written

`EventStream`:

| Field | Value |
| --- | --- |
| `status.observedGeneration` | `metadata.generation` |
| `status.phase` | `Running` when the source is Ready; otherwise `Pending`, `Suspended`, or `Failed` |
| `status.cursor` | preserved from the previous status (never advanced) |
| `status.lag` | `0` |
| `status.lastEventAt` | preserved from the previous status |
| `conditions[Ready]` | `True reason=StreamOpen` at `metadata.generation` when the EventSource has `BackendBound=True` and `Ready=True` at the EventSource's generation; otherwise `False` with reason `InvalidSpec`, `Suspended`, `SourceMissing`, or `SourceUnbound` |
| `conditions[Lagging]` | `False reason=UpToDate` |
| `conditions[Failed]` | `False reason=NoError`, or `True reason=InvalidSpec` for a nameless sourceRef/sink or an unknown sink kind |

Valid sinks are `atprotoRecordWatch` and `handler` with a non-empty name. A
sourceRef in another workspace leaves the stream `Pending/SourceUnbound`.

`EventSource`:

| Field | Value |
| --- | --- |
| `status.observedGeneration` | `metadata.generation` |
| `status.cursor`, `lastEventAt`, `lag`, `address` | copied from the backend status when bound |
| `conditions[BackendBound]` | `True reason=BackendBound` when the same-workspace backend object exists; `False` with reason `NotAContractKind`, `CrossWorkspace`, or `BackendMissing` |
| `conditions[Ready]` | the backend's `Ready` at the backend's generation (`reason` and `message` copied); otherwise `False` with `BackendNotReady` |

`PdsEventSource`:

| Field | Value |
| --- | --- |
| `status.observedGeneration` | `metadata.generation` |
| `status.address` | `workspace:AtprotoRepo.socialweb.computer/<name>` |
| `conditions[Ready]` | `True reason=Configured` "the source subscribes to <address>", or `False reason=InvalidSpec` |

`RelayEventSource`:

| Field | Value |
| --- | --- |
| `status.address` | `spec.url`, or `http://<kind>.<name>` for `spec.relayRef` (kind defaults to `relay`, lowercased) |
| `conditions[Ready]` | `True reason=Configured`, or `False reason=InvalidSpec` for both/neither of URL and relayRef, or a nameless relayRef |

`JetstreamEventSource`:

| Field | Value |
| --- | --- |
| `status.address` | `spec.url` |
| `conditions[Ready]` | `True reason=Configured`, or `False reason=InvalidSpec` for an empty URL |

Every condition carries `observedGeneration` equal to the object's
`metadata.generation`. The Ready fields are the ones
`internal/eventstream.StreamConditions`, the compose root watches, and
`ReadyAtGeneration` consume.

## Deliberately not implemented

- **Real firehose/relay/Jetstream WebSocket.** No source is dialed. A backend
  becomes Ready as soon as its spec parses, which is what unblocks compose.
- **Cursor persistence.** `status.cursor` is never advanced and
  `spec.cursor` is ignored; every `Poll` lists the bidder's event records from
  the beginning. Add a persisted cursor (and pass it as the first listing
  cursor) only when the browser repo grows enough for the full scan to hurt.
- **Cross-workspace source binding.** A sourceRef/backendRef in another
  workspace is reported unbound; the real controllers would resolve it through
  the bus.
- **Finalizers.** Nothing is allocated outside memory, so deletion only
  unindexes.
- **`atprotoRecordWatch` delivery and handler dispatch.** The poller writes
  `requester.inbox` directly instead of running a `delivery.Loop`.
- **Dedupe pruning.** The in-memory set lives as long as the `Poller`; it is
  never pruned. One poller per requester session avoids unbounded growth across
  unrelated contracts.
- **Record resolution and signature verification.** The poller trusts the
  lister's envelope refs; the requester re-fetches and verifies on consume.

## Tests and build

```
cd socialweb-wasm
go test ./internal/browserinfra/eventstream/...
GOOS=js GOARCH=wasm go build ./internal/browserinfra/eventstream/
```

The tests cover every status builder (Ready/phase/observedGeneration, stale
backend generations, invalid specs), `BackendRef` normalisation, poller
filtering and pagination with fake records, deterministic sha256 names, dedupe,
`ErrExists`, malformed records, and the no-event case.
