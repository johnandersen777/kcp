# browserinfra/exposure

Browser substitute for the SocialWeb `Service`/`Exposure` controllers and the
exposure publisher. It opens no socket, runs no TLS listener, and dials no
relay: an `Exposure` becomes `Ready` with the configured public origin in
`status.address`, and page-origin HTTP requests are dispatched in-process to
the handler componentcore registered on the bus.

The real wiring this mirrors lives in the read-only tree at
`internal/controllers/exposure/controller.go`,
`internal/controllers/service/controller.go`, `internal/exposure/exposure.go`,
`internal/exposure/registrar.go` (the xrpc-subscription ingress), and
`internal/app/compose/{kinds,status,identity,exposures}.go`.

## Wiring

```go
import (
    "github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
    "github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/exposure"
)

ctl, err := exposure.Register(mgr, exposure.Options{
    Origin:   "https://<fedproxy-host>", // required; the page origin
    Handlers: handlers,                  // *bus.Bus from componentcore
    Scope:    scope.Contains,            // optional
    Log:      log,
})
if err != nil { ... }

// The wasm bridge dispatches a same-origin fetch/ingress request:
h, ok := ctl.HandlerFor(host, path)   // or ctl.Lookup / ctl.Registry().Lookup
if !ok { /* 404: no exposure serves this host+path */ }
h.ServeHTTP(w, r)

// Or hand the controller/registry straight to net/http as an http.Handler:
mux.Handle("/", ctl)                   // dispatches by r.Host and r.URL.Path
```

`Register` adds `exposurev1alpha1` to the manager scheme, registers one
controller (`exposure`) on the `exposures` APIExport in its workspaces, and
watches `Service` objects so an Exposure that appears before its Service (or
whose Service changes) is reconciled again. `New(Options)` builds the same
controller without a manager (status/registry only) and returns an error when
the origin is invalid.

`Options.Handlers` is the `*bus.Bus` where componentcore registers component
HTTP handlers (`Bus.RegisterHTTP`, keyed by `{Workspace, Kind, Name}`). When it
is nil the controller still writes status but registers no routes.

## Origin

`ParseOrigin` accepts an absolute origin or a bare host (treated as https):

| Input | Result |
| --- | --- |
| `https://abc.fedproxy.com` | `https://abc.fedproxy.com`, host `abc.fedproxy.com` |
| `abc.fedproxy.com` | `https://abc.fedproxy.com` |
| `HTTPS://ABC.FedProxy.COM/` | `https://abc.fedproxy.com` |
| `https://abc.fedproxy.com:443` | `https://abc.fedproxy.com` (default port dropped) |
| `http://localhost:8090` | `http://localhost:8090` |
| `http://127.0.0.1:8080`, `http://[::1]:8080` | allowed (loopback) |
| `http://abc.fedproxy.com` | rejected: http is only allowed for localhost/loopback |
| credentials, path, query, fragment, non-http(s) scheme | rejected |

An empty origin is an error: the browser cannot infer the fedproxy host from an
Exposure, which on the real host is a DID-labeled subdomain.

## Exact status written

Every reconcile writes the object's `status` (typed
`exposurev1alpha1.ExposureStatus`; `metadata.generation` is the object's):

| Field | Value |
| --- | --- |
| `status.phase` | `Ready` (`ExposurePhaseReady`) |
| `status.ready` | `true` |
| `status.address` | `<origin>` for path `""`/`"/"`, else `<origin><path>` |
| `status.endpoint` | same as `status.address` (the ingress publisher sets Endpoint = Address) |
| `status.conditions[Ready]` | `True`, `reason=Ready`, `observedGeneration=metadata.generation`, message names the URL |

The Exposure API type has **no** `status.observedGeneration` and **no**
`status.url` field. The generation is therefore carried on each condition's
`observedGeneration`, and the URL is `status.address`: `internal/app/compose`
maps `address` to the endpoint `url` it reads (`exposureViewOf` →
`status.endpoints[{"name":"url","url":address}]`), and
`ApplyOAuthIdentityEndpoints` uses `status.address` as the OAuth client origin
(`oauthClientID`, `oauthRedirectURI`). `phase` and `ready` are copied verbatim.

While not ready the substitute writes the same shape the real controller does:
`phase=Pending` (or `Failed` for an invalid mode/target), `ready=false`,
`address`/`endpoint` cleared, `conditions[Ready]=False` with the reason
(`Suspended`, `InvalidMode`, `ServiceMissing`, `ServiceSuspended`,
`InvalidTarget`, `HandlerMissing`) and the generation. A missing Service or bus
handler retries every `RequeuePending` (2s); there is no bus watch.

## In-process handler registry

`Registry` maps `(host, path prefix)` to an `http.Handler`:

```go
registry := exposure.NewRegistry()
err := registry.Register(owner, host, path, handler) // owner = workspace/name
h, ok := registry.Lookup(host, path)                // alias: HandlerFor
registry.Remove(owner)                              // cleanup, idempotent
```

* One owner (an Exposure) owns at most one route; a re-register replaces it.
* Host matching is case-insensitive, ignores the scheme and default ports
  (`:80`, `:443`); non-default ports are compared.
* Path matching is longest-prefix on segment boundaries (`/api` does not match
  `/apix`), and `/` matches everything for that host.
* Lookups take an `RWMutex`; `Registry` and `Controller` are safe for concurrent
  use and implement `http.Handler` (`ServeHTTP` dispatches by `r.Host` and
  `r.URL.Path`, 404 on a miss).
* Duplicate host+path routes resolve deterministically (lowest owner name)
  rather than reporting ambiguity, because the browser plan serves one origin.

The controller resolves each Exposure's route on reconcile: `serviceRef` →
`Service` in the same workspace → `spec.targetRef` → `Bus.Handler(target,
service.spec.handler)` for `http`/`ws` services. The handler is wrapped in
`bus.WithCaller(target)` exactly like the real local publisher, so
bus-authorized calls made by the requester handler are attributed to the
component. Routes are removed on deletion, suspension, a missing Service, a
missing handler, or a protocol that is no longer `http`/`ws`.

## Real behavior → substitute

| Real Exposure behavior | Substitute |
| --- | --- |
| `local` mode binds a port through the port registry and serves TLS/plain HTTP on a listener | No listener. The route is keyed by the configured public origin and forwarded in-process. |
| `ingress` mode registers an xrpc-subscription (`com.fedproxy.temp.xrpc.subscribe`) or open-tunnel subscriber with Caddy/the ingress proxy and returns a DID-labeled `https://<did>.<proxy-host>` URL | No relay connection, no Caddy, no DID subdomain. The per-exposure URL is the single configured `Origin` (`https://<fedproxy-host>`). |
| `status.address` is the bound listener address or the ingress URL; `status.endpoint` is the address | `status.address` = `status.endpoint` = origin (+ path). compose/`ApplyOAuthIdentityEndpoints` get the public origin they need. |
| `Service` status carries target endpoints from the component registry | Service status is not written. Nothing in the app consumes it for URL resolution; the Exposure status is enough. |
| Handler presence is checked through the bus and rechecked on component/link events (`ComponentChanged`, `ComponentLink` watches) | Checked through the same `Bus.Handler`; retried with `RequeueAfter` instead of bus/link watches. `ComponentLink` is not watched; `Bus.Handler` does not authorize, only `Dial*` does. |
| TCP/SSH exposures get a stream listener and an `ssh://`/`tcp://` address | No stream listener. Status is still written with the public origin, but no route is registered; these protocols are unusable in the browser. |
| TLS (`spec.tls`), client CA bundles, auth (`spec.auth`) | Ignored. The page is already served over HTTPS by the hosting origin; the in-process handler sees the request directly. |
| Stable-host mismatch, `IngressNotReady`, port leases, address-in-use | Not applicable: no port, no stable host, no proxy readiness. |

## Limitations

* No public listener, no TLS server, no Caddy/ingress subscriber, no relay
  WebSocket. The only network path is the hosting page origin.
* One public origin per controller. Real ingress exposures get distinct
  DID-labeled subdomains; the substitute cannot, so multiple exposures that
  resolve to `"/"` on the same origin collide (deterministically). Configure
  distinct `spec.path` prefixes if more than one service must share the origin.
* No path rewriting: the Exposure path is a routing prefix, and the request
  path is passed to the handler unchanged (the real publisher also forwards
  the path; the ingress subscriber re-adds params/body).
* No Service status, no component/link event stream; readiness depends on the
  Service object and the bus handler at reconcile time, hence the 2s requeue.
* `spec.hostname`, `spec.publicHost`, `spec.ingressAddress`, `spec.ingressDial`,
  and `spec.ingressRef` are not used; `Origin` is the single source of truth.

## Tests and build

```
cd socialweb-wasm
go test ./internal/browserinfra/exposure/...
GOOS=js GOARCH=wasm go build ./internal/browserinfra/exposure/
```

The tests cover the status builders, origin validation, registry
register/lookup/remove (including replacement and concurrent access),
host normalization, longest-prefix and segment-boundary path routing, and
`ServeHTTP` dispatch. Reconcilers are not driven directly because
`ctlruntime.Request` has an unexported client scope; their status and routing
logic is the pure `Status`/`NotReady`/`Register` surface.
