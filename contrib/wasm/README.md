# kcp on WASM: the `Fetch` CRD

This directory contains a small **kcp-compatible API shard** that is compiled
to WebAssembly (`GOOS=js GOARCH=wasm`) and is driven by Deno. It serves a new
`Fetch` CRD written in Go: create a `Fetch` with a `spec.url`, the controller
fetches it, and the response body is stored in `status.body` where it shows up
in `kubectl get fetches`.

```console
$ kubectl get fetches
NAME    URL                                                   PHASE      CODE   BODY                                                            AGE
demo    http://127.0.0.1:18080/echo?msg=hello-from-kcp-wasm   Resolved   200    fetched-by=deno method=GET path=/echo msg=hello-from-kcp-wasm   2026-10-20T21:20:21Z
later   http://127.0.0.1:18080/echo?msg=patched-later         Resolved   200    fetched-by=deno method=GET path=/echo msg=patched-later         2026-10-20T21:20:21Z
```

## Quick start

```console
$ ./scripts/build-kcp-wasm.sh                 # GOOS=js GOARCH=wasm + wasm_exec.js
$ ./scripts/verify-kcp-wasm.sh                # build, run under Deno, verify with kubectl
```

To run it by hand:

```console
$ ./scripts/build-kcp-wasm.sh                 # writes bin/wasm/kcp.wasm and wasm_exec.js
$ deno run --allow-read --allow-net --allow-env --allow-write scripts/kcp_wasm.ts \
    --wasm=bin/wasm/kcp.wasm \
    --wasm-exec=bin/wasm/wasm_exec.js \
    --state=bin/wasm/kcp.state.json \
    --port=18080
```

`--state` defaults to `<wasm path>.state.json`. The file is written
atomically by the Deno host after every mutation and read back at startup.

and point kubectl at it with a plain-HTTP kubeconfig:

```yaml
apiVersion: v1
kind: Config
clusters:
- name: kcp-wasm
  cluster:
    server: http://127.0.0.1:18080
contexts:
- name: kcp-wasm
  context: {cluster: kcp-wasm, user: kcp-wasm}
current-context: kcp-wasm
users:
- name: kcp-wasm
  user: {}
```

```console
$ export KUBECONFIG=$PWD/kubeconfig
$ kubectl get crds
NAME                   AGE
fetches.fetch.kcp.io   <unknown>

$ kubectl apply -f - <<'EOF'
apiVersion: fetch.kcp.io/v1alpha1
kind: Fetch
metadata:
  name: demo
spec:
  url: http://127.0.0.1:18080/echo?msg=hello-from-kcp-wasm
EOF
fetch.fetch.kcp.io/demo created

$ kubectl get fetch demo -o yaml
...
status:
  body: fetched-by=deno method=GET path=/echo msg=hello-from-kcp-wasm
  contentType: text/plain; charset=utf-8
  phase: Resolved
  statusCode: 200
```

A `Fetch` created without a URL stays `Unresolved`; adding a URL later (for
example with `kubectl patch fetch later --type=merge -p '{"spec":{"url":"..."}}'`)
makes the controller fetch it.

## Architecture

```
kubectl / curl ──HTTP──> Deno.serve (scripts/kcp_wasm.ts, main thread)
                              │  postMessage {id, request}
                              ▼
                         Web Worker (scripts/kcp_wasm_worker.ts)
                              │  WebAssembly.instantiate(kcp.wasm)
                              │  calls Go's exported kcpHandleRequest()
                              ▼
                         kcp-wasm (./cmd/kcp, GOOS=js GOARCH=wasm)
                              │
                              │  Fetch controller: http.Client.Do()
                              ▼
                         Go's js/wasm net/http transport
                              │  (net/http/roundtrip_js.go) uses the JS
                              │   Fetch API for every request
                              ▼
                         Deno fetch() ──> http://127.0.0.1:18080/echo
```

### Why outbound HTTP is transparent

The shard never installs a custom `RoundTripper` and the controller uses plain
`http.Client`/`http.Get`. Go's **js/wasm port itself** implements
`(*net/http.Transport).RoundTrip` on top of the JS `fetch` API
(`src/net/http/roundtrip_js.go`). Any code compiled for `js/wasm` that uses a
`*http.Transport` without a custom `DialContext`/`Dial`/`DialTLS` gets that
implementation automatically — including a zero-value `http.Client`, the
default client, and Go's `http.Get`.

Two runtime details matter, both verified against Deno 2.9:

* `fetch` must exist (it does in Deno), and
* Go disables the Fetch path when `globalThis.process.argv0` starts with
  `"node"` (`jsFetchDisabled` in `roundtrip_js.go`). Deno reports
  `process.argv0 == "deno"`, so the Fetch path is active. Running the module
  under a Node-compatibility shim that sets `argv0` to `node` would silently
  fall back to the (fake) socket network instead.

`GOOS=wasip1` cannot do this: its `net` package is a fake in-process network
and there is no host hook into Go's `net/http`.

### The js/wasm build mode of `cmd/kcp`

The WASM artifact is built from the kcp command itself:

```console
$ GOOS=js GOARCH=wasm go build -o bin/wasm/kcp.wasm ./cmd/kcp
```

The command has a build-tagged split:

* `cmd/kcp/kcp.go` and `cmd/kcp/help.go` are guarded by `//go:build !js` —
  they pull in embedded etcd and Kubernetes components that cannot compile or
  run in WebAssembly.
* `cmd/kcp/main_wasm.go` is guarded by `//go:build js && wasm` and starts the
  kcp-compatible shard implemented in `contrib/wasm` through
  `contrib/wasm/bridge`.

The native (non-WASM) `cmd/kcp` is unchanged. The reason the real control
plane cannot run under `js/wasm` is not just compilation; even with the
dependency errors patched, it needs (a) an etcd wire-protocol transport,
(b) `net.Listen` (Go's wasm ports cannot listen on sockets), and (c) a
listener to put TLS/auth in front of. For reference, the compile errors from
the untagged path are:

```console
$ GOOS=js GOARCH=wasm go build ./cmd/kcp   # without the js build tags
# github.com/coreos/go-systemd/v22/journal
undefined: syscall.UnixRights
# go.etcd.io/etcd/client/pkg/v3/fileutil
undefined: syscall.Flock, syscall.LOCK_EX, ...
# go.etcd.io/bbolt/internal/common
undefined: MaxAllocSize
# k8s.io/mount-utils
undefined: syscall.Access, unix.Utsname, unix.Uname
# k8s.io/kubernetes/pkg/probe
undefined: syscall.SetsockoptLinger, syscall.SOL_SOCKET, ...
# github.com/moby/term
undefined: unix.Termios, ...
```

`contrib/wasm` therefore implements the smallest kcp-compatible control
plane that is meaningful in this environment: the same Kubernetes API shapes,
discovery, CRUD, status subresource, patches, server-side `Table` conversion,
and a kubeconfig-usable HTTP endpoint provided by the Deno host. The missing
pieces are called out in [Limitations](#limitations).

## Upstream kubectl on WASM (in a separate worker)

`contrib/wasm/kubectl` compiles upstream kubectl — the
[`kubernetes/kubectl`](https://github.com/kubernetes/kubectl) command tree
(`k8s.io/kubectl/pkg/cmd`) — to `GOOS=js GOARCH=wasm`. It runs in its own
Deno Web Worker, separate from the worker that hosts the kcp WASM shard, and
talks to the shard over ordinary HTTP. Go's js/wasm `net/http` transport
performs every request with that worker's `fetch()`.

```
kubectl-wasm worker (scripts/kubectl_wasm_worker.ts)
  kubectlRun(args)                    kcp host process
    client-go discovery/CRUD                |
      net/http (js/wasm transport)          |
        fetch() ─────HTTP──────> Deno.serve ─┴─> kcp worker (kcp.wasm)
```

```console
$ ./scripts/build-kubectl-wasm.sh            # bin/wasm/kubectl.wasm
$ ./scripts/build-kcp-wasm.sh                # bin/wasm/kcp.wasm
$ ./scripts/verify-kubectl-wasm.sh           # both, end to end
```

Run it by hand against a running kcp-wasm host:

```console
$ deno run --allow-read --allow-net scripts/kubectl_wasm.ts \
    --wasm=bin/wasm/kubectl.wasm \
    --wasm-exec=bin/wasm/wasm_exec.js \
    -- get fetches --server=http://127.0.0.1:18080

$ deno run --allow-read --allow-net scripts/kubectl_wasm.ts \
    --wasm=bin/wasm/kubectl.wasm \
    --wasm-exec=bin/wasm/wasm_exec.js \
    --stdin-file=manifest.yaml \
    -- create -f - --server=http://127.0.0.1:18080
```

`scripts/kubectl_wasm_worker.ts` exposes one RPC:
`kubectlRun(requestJSON, callbackID)` → `__kubectlResolve(callbackID,
resultJSON)`. The request holds the argument list and stdin; the result holds
stdout, stderr and the exit code.

The WASM runtime has no real filesystem, so `scripts/kubectl_wasm_fs.ts`
installs a synchronous `globalThis.fs` for Go's js/wasm `syscall` package
before the Go runtime starts:

* reads (`kubeconfig`, `-f <file>`) are served by Deno's synchronous fs APIs
  (the worker needs `--allow-read`),
* fd 0 (`kubectl create -f -`) returns the stdin sent with the run,
* writes (kubectl's discovery cache) stay in memory.

`scripts/prepare-kubectl-wasm-modules.sh` swaps out the unix-only pieces of
the upstream sources for js/wasm: `k8s.io/kubectl`'s `syscall.SIGHUP`
interrupt handler, `unix.Umask`, the `SIGWINCH` resize watcher, the
`syscall.Exec` plugin path, and `moby/term`'s TTY ioctls.

### Web UI: a just-bash shell with kubectl, goat and yq

`scripts/kubectl_wasm_web.ts` serves a page whose terminal is a
[just-bash](https://github.com/vercel-labs/just-bash) shell. The shell's
custom commands bridge to WebAssembly workers; real shell parsing gives pipes,
redirection, heredocs, `&&`, variables and the built-in unix commands.

```
just-bash (page)
  kubectl ─╮
  goat ────┼──> WASM workers ──fetch()──> PDS / PLC / AppView (CORS)
  yq ──────╯    kcp / kubectl / cli
```

* `kubectl` — the upstream kubectl WASM worker; requests for the virtual API
  origin are relayed to the kcp worker through the page.
* `goat` and `yq` — stock upstream Go binaries compiled to WASM. Each
  invocation gets a **fresh Go runtime instance** in `cli_worker.js`, exactly
  like running the native binary: argv, env, cwd and stdin come from the
  shell, and stdout/stderr/exit code and filesystem changes go back to it.
* `jq` — just-bash's built-in jq implementation (no WASM worker).

```console
$ ./scripts/build-kcp-wasm.sh
$ ./scripts/build-kubectl-wasm.sh
$ ./scripts/build-goat-wasm.sh
$ ./scripts/build-yq-wasm.sh
$ deno run --allow-read --allow-net scripts/kubectl_wasm_web.ts
# open http://127.0.0.1:8090
/home/user $ kubectl get fetches
/home/user $ kubectl get fetches -o json > fetches.json
/home/user $ yq '.items[].metadata.name' fetches.json
/home/user $ echo '{"a":1}' | jq .a
/home/user $ goat resolve bsky.app
/home/user $ goat xrpc query https://public.api.bsky.app app.bsky.actor.getProfile actor==atproto.com
/home/user $ kubectl apply -f - <<'EOF'
apiVersion: fetch.kcp.io/v1alpha1
kind: Fetch
metadata: {name: demo}
spec: {url: http://127.0.0.1:8090/echo?msg=heredoc}
EOF
```

On every page load a boot sequence is typed and auto-entered, one command at
a time: it resolves `socialweb.computer` with goat, derives the PDS
`getRecord` URL for its `app.bsky.actor.profile/self` record, applies that URL
as a Fetch, waits with `kubectl wait --for=jsonpath=...`, tees the Fetch JSON
to `/tmp/socialweb-profile.json` while printing it with `yq -P`, then prints
`.status.body` from that tee'd file through `jq -r ... | yq -P`. The boot
commands are added to shell history, so ↑ recalls them. The same command
substitution / pipe chains work interactively.

Files live in just-bash's in-memory filesystem. Before each goat/yq run the
shell files are seeded into the CLI worker, and its resulting files are merged
back, so `goat lex new` output appears in `ls` and yq edits are visible to
`cat`. The shell filesystem, history and environment (including goat auth
sessions under `$XDG_STATE_HOME`) persist in localStorage.

Upstream sources are not modified. goat and yq are built from their repos'
default branches, and the stock `main()` runs per invocation. The only
platform adaptation is an indigo copy for js/wasm: Go's wasm port has no DNS
and net/http only uses `fetch` without a custom `DialContext`, so the copy
skips DNS, keeps a fetch-capable transport, and falls back to the public
AppView for handle resolution. It is built through a generated `-modfile`;
the goat checkout and module cache stay untouched. goat calls PDS, PLC and
AppView directly from the browser; those services send
`Access-Control-Allow-Origin: *` (and allow Authorization in preflight), so no
proxy is involved.

The layout is responsive: narrow/iPhone viewports stack the diagram under the
terminal, show a touch toolbar for the actions that need Ctrl (Ctrl-C,
Ctrl-D, Enter, Tab, history), shrink the terminal font, and render the
diagram top-to-bottom. The activity log starts collapsed; `show`/`hide`
toggles it and the choice is remembered.

xterm.js, Pele and just-bash load from jsDelivr/esm.sh, so the browser needs
network access. There is no build step for the page.

Limitations: websocket-based commands (`goat firehose`) do not work in the
browser; files shared with goat/yq are UTF-8 text up to 1 MiB; two tabs share
one localStorage snapshot. Goat's network calls require the remote service to
allow CORS (Bluesky PDS, PLC and AppView do).

### VM shell: rent a VM from the browser and open a shell on it

The combined real-kcp + socialweb module (`scripts/build-socialweb-kcp-wasm.sh`)
runs a socialweb MarketRequester in the tab. After signing in with AT Protocol
OAuth, the VM SHELL panel rents a firecracker VM from a market bidder and shows
a link to a wootty web terminal on it:

1. The page generates a 32-character wootty token and writes the secrets bundle
   `[{path: /etc/wootty/wootty.env, value: "WOOTTY_AUTH_TOKEN=<token>"}]` into
   the in-browser vault at `requester/contracts/<name>/secrets`, through a
   page-only Go export (`socialwebVaultPut`); nothing reachable over HTTP can
   call it.
2. It applies a `ComputeContract` with `bidderDids`, `secretsRef` and
   `tunnels: [{tag: wootty, port: 7681, shell: {envFile: /etc/wootty/wootty.env}}]`.
   socialweb's cloud-init module installs opentunnel and wootty in the guest.
3. The requester's ingress Exposure registers a per-Exposure did:key with the
   `xrpc.fedproxy.com` relay (`scripts/browser/ingress_ws.js`), so the tab has
   a public `https://did-key-….xrpc.fedproxy.com` address. Bidders POST bids
   and guest events to it, and the guest's `setup-secrets.service` fetches the
   bundle from it.
4. When the guest reports its tunnel, `status.contract.networks[tag=wootty]`
   holds `https://<route>.<id>.opentunnel.xyz`. The panel links to
   `<address>/#token=<token>`; the token stays in the URL fragment, so it never
   reaches the tunnel or a server log. Release patches `spec.release=true`.

Measured against a production bidder: about 40 s from the click to Held, and the
tunnel address about 70 s later. The OAuth client metadata scope is generated
from socialweb's `RequesterOAuthScope` at build time (`requester-scope.txt`
next to `kcp.wasm`) so it always matches what the requester asks for.

## The `Fetch` CRD (written in Go)

* `apis/fetch/v1alpha1/types.go` — typed `Fetch`, `FetchList`, spec/status and
  deep-copy methods. Phases: `Unresolved` (initial), `Fetching`, `Resolved`,
  `Error`.
* `apis/fetch/v1alpha1/crd.go` — `NewCRD()` builds an
  `apiextensions.k8s.io/v1` `CustomResourceDefinition` in Go, including the
  OpenAPI v3 schema, the `status` subresource, and `additionalPrinterColumns`
  for URL, Phase, Code, **Body** and Age. The shard serves this object
  (`kubectl get crds`) and derives its `Table` output from those columns, so
  the CRD is the single source of truth for what kubectl displays.
* `server/controller.go` — the reconciler. It performs the fetch with the
  standard library client, follows redirects, records the status code,
  content type and body (capped at 1MiB), and requeues if the spec changed
  while the request was in flight.
* `server/server.go` — discovery (`/api`, `/apis`), CRUD, status subresource,
  JSON merge / apply patches, and `meta.k8s.io/v1` `Table` responses, backed
  by the `storage.Interface` implementation below.
* `server/openapi.go` — a minimal OpenAPI v2 document in **protobuf**.
  kubectl proto-decodes `/openapi/v2` unconditionally; serving JSON makes
  `kubectl apply` fail with `cannot parse invalid wire-format data`.
* `server/table.go` — Table conversion using the CRD's printer columns.
* `bridge/bridge_js.go` — js/wasm glue. Restores the snapshot, registers the
  snapshot callback, and exposes `kcpHandleRequest` / `__kcpResolve`.
* `main_js.go` — standalone entrypoint for the shard (`./cmd/kcp` also builds
  it in its js/wasm mode).

## Storage and persistence

`storage/store.go` implements `k8s.io/apiserver/pkg/storage.Interface` — the
same interface etcd3 implements — on top of an in-memory map. This is the
single-node replacement for etcd:

* `Create`, `Get`, `GetList`, `GuaranteedUpdate`, `Delete`, `Watch`, `Stats`,
  `ReadinessCheck`, `RequestWatchProgress`, `GetCurrentResourceVersion`,
  `CompactRevision`;
* monotonically increasing resource versions via `APIObjectVersioner`;
* no-op detection (an update with identical content does not bump the
  resource version), UID/resourceVersion preconditions, label and field
  selectors, limit/continue paging, and non-blocking fan-out watches;
* JSON snapshots (`Snapshot` / `Restore`), with the resource version carried
  across restarts.

Persistence is owned by Deno, not by Go:

```
Go mutation ──> Store.OnChange(snapshotJSON)
             ──> __kcpPersist (syscall/js)
             ──> worker postMessage
             ──> main thread Atomically writes --state file (tmp + rename)
restart: main reads --state ──> worker init ──> __kcpInitialState
             ──> Store.Restore before the controller starts
```

Because the store is a real `storage.Interface`, it can back kcp's generic
apiserver directly. That wiring is in place:

## Integrating with kcp's generic apiserver

The apiserver constructs one store per resource through
`k8s.io/apiserver/pkg/storage/storagebackend/factory`. A patched copy of that
factory (selected by the `-modfile` in the build scripts) adds a backend
selected with `memory://<name>`:

```console
$ KCP_MEMORY_STORAGE=main ./kcp start --root-directory=/tmp/kcp-memory
# or directly:
$ ./kcp start --root-directory=/tmp/kcp-memory --etcd-servers=memory://main
```

Details:

* `storage.Prefixed` reproduces etcd3's key layout
  (`<prefix>/<resourcePrefix>/<key>`), so all resources of one apiserver share
  a single resource-version and watch sequence without colliding.
* The cache server (an in-process second apiserver) gets its own
  `memory://main-cache` store when the backend is memory.
* Embedded etcd is compiled out of the js/wasm build by a build-tagged shim
  (`pkg/server/embeddedshim`); etcd-only components (object-count scanner,
  logical-cluster migration dump) are skipped when the backend is memory.
* kcp does not persist the logical cluster in the object. The etcd3 store
  re-annotates objects on read (`kcp.io/cluster`, shard), and internal cache
  clients rely on those annotations to build URLs. The memory store records
  the request context's cluster/shard per key and applies the same
  annotations on Get/List/Watch (and keeps them in snapshots).
* The watch history (bounded, 4096 events) lets caches start a watch at the
  resourceVersion returned by their list without missing events. For
  `sendInitialEvents` (WatchList) watches, the store emits the
  initial-events-end BOOKMARK after the initial ADDED events, using the
  resource's `newFunc` prototype — raw (non-cached) storages must do this
  themselves, and kcp's cache server runs without a watch cache.

Reproduce the integration natively (real kcp control plane, no etcd):

```console
$ ./scripts/verify-kcp-memory.sh
== starting kcp with memory://main storage on port 6445 ==
== apiserver is serving from memory ==
== applying the Go-defined Fetch CRD ==
== creating and reading Fetch custom resources ==
fetch.fetch.kcp.io/demo created
== deleting through the memory backend ==
fetch.fetch.kcp.io "demo" deleted
PASS: kcp's generic apiserver bootstrapped and served CRDs/CRs from memory://main (no etcd).
```

The script starts the real `kcp start`, waits for bootstrap (which writes RBAC,
namespaces, system CRDs and workspaces through the memory backend), applies
the `Fetch` CRD, and exercises create/get/list/delete of a `Fetch`. The
in-process cache server runs on its own `memory://main-cache` store.

Building the real server for wasm:

```console
$ ./scripts/build-kcp-real-wasm.sh
Building the real kcp server for js/wasm...
Built /home/johnandersen777/src/kcp/bin/wasm/kcp-real.wasm
```

That is a ~240MiB module containing the full control plane and the
`storage.Interface` memory backend, with the unix-only dependencies replaced
as described above. It is currently a compile artifact: the real-server
entrypoint does not install the listener/HTTP bridge, so the native run is
the end-to-end proof of the storage integration.

`scripts/prepare-kcp-patched-modules.sh` copies the unix-only dependencies
(systemd journal, mount-utils, probe, moby/term, kube securitycontext) into a
writable directory, swaps in js/wasm implementations, and writes the
`.go.memory.mod` used by the build. The resulting `kcp-real.wasm` compiles the
full control plane and the memory backend; serving external clients still
needs the listener/HTTP bridge, which the shard entrypoint implements and the
real-server entrypoint does not yet.

## Scripts

* `scripts/build-kcp-wasm.sh` — builds `./cmd/kcp` for `js/wasm` (the kcp
  command's WASM build mode) and stages the matching
  `wasm_exec.js` from `go env GOROOT`.
* `scripts/kcp_wasm.ts` — Deno host. Serves HTTP, forwards requests to the
  worker, provides `/echo` (a deterministic local URL for the controller),
  and owns snapshot persistence (`--state`, atomic file replace). Also serves
  `/readyz` for readiness checks.
* `scripts/kcp_wasm_worker.ts` — Web Worker. Loads `wasm_exec.js`, instantiates
  the module with `WebAssembly.instantiate`, passes the initial snapshot to
  Go, and implements the `kcpHandleRequest` → `__kcpResolve` RPC plus the
  `__kcpPersist` snapshot callback. The callback split is required: Go's
  outbound fetch is asynchronous, and blocking the JS callback would prevent
  the Go runtime from completing it.
* `scripts/verify-kcp-wasm.sh` — end-to-end check with real `kubectl`:
  discovery/CRD, apply, Unresolved→patch URL→Resolved, body in the table and
  in `-o yaml`, then a Deno restart to verify snapshot restore.
* `scripts/prepare-kcp-patched-modules.sh` — copies unix-only dependency
  modules to a writable directory, installs the js/wasm replacements, and
  writes `.go.memory.mod` with the required `replace` directives.
* `scripts/build-kcp-real-wasm.sh` — builds the real `cmd/kcp` server path for
  `js/wasm` with the patched modules (`kcp-real.wasm`).
* `scripts/build-kubectl-wasm.sh` — builds upstream kubectl
  (`contrib/wasm/kubectl`, js/wasm) and stages `wasm_exec.js`.
* `scripts/build-goat-wasm.sh` — builds bluesky-social/goat (stock main) and
  stages the matching `wasm_exec_cli.js`; indigo gets a js/wasm adaptation via
  a generated `-modfile`.
* `scripts/build-yq-wasm.sh` — builds mikefarah/yq (stock) for js/wasm.
* `scripts/prepare-kubectl-wasm-modules.sh` — copies `k8s.io/kubectl` and
  `moby/term` to a writable directory, installs the js/wasm replacements, and
  writes the `-modfile` used by the kubectl build.
* `scripts/kubectl_wasm.ts` — Deno host: runs `kubectl.wasm` in a Web Worker
  and prints its stdout/stderr/exit code. Everything after `--` is passed to
  kubectl; `--stdin-file` feeds `-f -`.
* `scripts/kubectl_wasm_worker.ts` — worker that instantiates `kubectl.wasm`
  and implements the `kubectlRun` → `__kubectlResolve` RPC.
* `scripts/kubectl_wasm_fs.ts` — synchronous `globalThis.fs` shim: Deno-backed
  reads of kubeconfig/`-f` files, run-scoped stdin, in-memory writes.
* `scripts/kubectl_wasm_web.ts` — static server for the browser demo: page,
  worker scripts, all `.wasm` files, `/config`, `/echo` and `/demo.yaml`.
* `scripts/browser/` — the browser side. `app.html` (layout), `app.js`
  (just-bash shell, worker orchestration, polling), `kcp_worker.js` (kcp.wasm),
  `kubectl_worker.js` (kubectl.wasm + fetch bridge), `cli_worker.js` (fresh
  goat/yq instance per command), `fs_shim.js` (in-memory fs, stdout capture,
  cwd), `diagram.js` (Mermaid builder + unit test).
* `scripts/verify-kubectl-wasm-web.sh` — builds both modules, serves the demo,
  checks every asset and route, and runs the diagram tests.
* `scripts/verify-kubectl-wasm.sh` — end-to-end check: builds both modules,
  runs them in separate Deno workers, and exercises discovery, create from
  stdin, the Fetch controller, `-o yaml` and Table output.
* `scripts/verify-kcp-memory.sh` — runs the real native kcp control plane on
  `memory://main`, applies the Fetch CRD, and exercises CR create/get/list/
  delete against the in-memory backend.

## Tests

```console
$ go test -race ./contrib/wasm/...
$ deno check scripts/kcp_wasm.ts scripts/kcp_wasm_worker.ts
$ deno check scripts/kubectl_wasm.ts scripts/kubectl_wasm_worker.ts scripts/kubectl_wasm_fs.ts
$ deno test scripts/browser/diagram_test.js
$ ./scripts/verify-kcp-wasm.sh
$ ./scripts/verify-kubectl-wasm.sh # upstream kubectl wasm -> kcp wasm, no native kubectl
$ ./scripts/build-goat-wasm.sh
$ ./scripts/build-yq-wasm.sh
$ ./scripts/verify-kubectl-wasm-web.sh # browser demo assets and diagram builder
$ ./scripts/verify-kcp-memory.sh   # real kcp control plane on memory://main
$ ./scripts/build-kcp-real-wasm.sh # compile the real server for js/wasm
```

The native tests exercise the storage.Interface implementation (create/get,
GuaranteedUpdate, no-op detection, preconditions, list selectors and paging,
watches, snapshot/restore), the Fetch controller (against `httptest`),
patches, Table conversion, discovery, and the OpenAPI protobuf response. The
end-to-end script additionally covers the js/wasm fetch bridge and persistence
across a Deno restart.

## References

* Deno Web Workers example (used by `scripts/kcp_wasm.ts` /
  `scripts/kcp_wasm_worker.ts`): <https://docs.deno.com/examples/web_workers/>
* Deno WebAssembly example (the worker uses `WebAssembly.instantiate` with
  Go's `wasm_exec.js` import object): <https://docs.deno.com/examples/webassembly/>
* Go's js/wasm HTTP transport: `src/net/http/roundtrip_js.go` in the Go
  source tree (the `jsFetchDisabled` heuristic is what keeps Deno on the
  Fetch path).

## Limitations

* In-memory store with a single-writer snapshot file. Persistence is wired for
  the shard entrypoint; the real kcp server's shared stores are not snapshotted
  yet. Two Deno hosts sharing a `--state` file would clobber each other.
* Watch history is bounded (4096 events). A watch starting older than the
  history or than a restored snapshot receives the current state as ADDED
  events rather than a replay.
* etcd-only components (object-count scanner, logical-cluster migration dump
  and controller) are disabled when the backend is `memory://`.
* Reads round-trip each object through the resource's storage codec, like the
  etcd3 store does, so structural-schema defaults that only exist on decode
  (for example a Workspace's default `.status.phase`) are re-applied. This
  makes creating a Workspace without pre-setting the `tenancy.kcp.io/phase`
  label work; the round-trip costs one encode/decode per read.
* The real-kcp integration is verified natively. The js/wasm real-server build
  (`kcp-real.wasm`) compiles the control plane and the memory backend, but its
  entrypoint does not yet install a listener/HTTP bridge; Go's wasm ports have
  no real sockets and Deno cannot reach the in-process fake network.
* The shard API is served by the Deno host; the WASM module itself cannot
  listen on a socket. This is a property of Go's wasm ports, not of this design.
* Bodies are stored as UTF-8 strings (up to 1MiB), matching what the JS
  `fetch` API returns as text.
