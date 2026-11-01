# Fenced policy sandbox in the page: opaque-origin iframe + per-run Worker, wired like the WebRTC ingress (rev 2, plan only)

Status: proposed, waiting for owner approval. No code is written until it is approved.

Rev 2 (2026-10-30, owner, atirc seq 836): follow the established wasm pattern of the WebRTC
ingress (an API value declared in socialweb and only instantiated by the wasm build, a contract
interface with a native and a `js && wasm` implementation, a page half reached through one
`__socialweb<Name>` bridge, state in resource status) instead of rev 1's ad-hoc
`__kcpSandbox*` globals. The fence mechanism (opaque iframe + per-run Worker) is unchanged.
Also records the owner's stable-rkey preference for the bundled tangled-vouch action.

## Context

The owner chose option C of plan 0036 (section 2): evaluate socialweb policy (e.g.
`requester-tangled-vouch`) inside the page's kcp.wasm instead of in Go-only checks. Today every
policy verdict in the page would be Deny, because the page's runner says it is not fenced.

References: socialweb at `origin/main` 90d0435e (`~/src/kcp/socialweb-computer-kcp`, read only);
this repo at `kcp-wasm-memory-storage-1`.

### What "Fenced" means in socialweb

- Interface `internal/contract/sandbox/sandbox.go:8-38`: `Program{Source, Dir, Timeout, Net, Env}`
  -> `Result{ExitCode, TimedOut, Stdout, Stderr}`; `Runner{Run, Available, Fenced, Kind}`.
  `Fenced()` is a self-declared bool. No test anywhere asserts fencing (no `_test.go` under
  `internal/sandbox` or `internal/policyengine/workflow`; nothing greps for `Fenced` in tests).
- Linux runner `internal/sandbox/deno/deno.go`: `Fenced()` is always true (:42). It runs
  `deno run --quiet --no-prompt --no-config`, then `--allow-net` if `Net`, else
  `--deny-net --no-remote --no-lock` (:59-64), `--allow-read/--allow-write=<Dir>` only if `Dir`
  (:65-67), `--deny-run --deny-ffi --ext ts -` (:68), the program on stdin, a closed environment
  when `Env` is set (:74-76), a timeout via `context.WithTimeout` (default 2 min, :18-20, :53-58)
  and stdout/stderr capped at 64 KiB each (:22, :77-80). No memory cap (V8 default heap).
- What the policy engine asks for: every `${{ }}` expression and every `uses:` action is one
  `Run` with `Net: true`, no `Dir`, `Env` = `NO_COLOR`, `DENO_NO_UPDATE_CHECK` + step env
  (`internal/policyengine/workflow/expression.go:285-314`, `action.go:101-129`;
  `CONTRIBUTING.md:1116-1127`). So for policy code the Linux fence is: **no filesystem, no
  ambient env, no subprocess, no FFI, killed at the deadline, output capped; network allowed**
  (and since `--no-remote` is only added without net, remote `import()` is allowed too).
- The gate: `expression.go:289` refuses a runner with `Fenced() == false`; `Engine.Ready()`
  (`internal/policyengine/controller/engine.go:164-166`) needs `Available() && Fenced()`;
  `manager.go:113-131` turns that into the PolicyEngine `Ready` condition (when false the
  message is hard-coded "the embedded or system deno runtime is unavailable", :125-130); the
  run controller (`internal/controllers/policyworkflowrun/controller.go:159-173`) only runs on a
  ready engine. A refused expression falls back to the pure-Go property-path resolver
  (`expression.go:108-121`), an action step fails, the verdict is Deny
  (`CONTRIBUTING.md:1121-1127`).
- Inputs Go -> sandbox: the program text only. Expressions are a generated script with the
  contexts inlined as JSON literals (`github`, `steps`, `inputs`, `env`, `secrets`, `vars`,
  `needs`, `matrix`; `expression.go:136-205`); the answer is `console.log` on stdout. An action is
  `actionShim` + the bundled `dist/index.js` + footer (`action.go:116-120, 209-309`): the shim
  inlines the step env, the policy cache (`GITHUB_CACHE`) and the event, overrides `Deno.env`,
  `Deno.writeTextFile(Sync)`/`readTextFile(Sync)` for the virtual `GITHUB_*` files, routes
  `console.*` to stderr, turns `Deno.exit` into a throw, and prints one
  `__PE_RESULT__{code, vfs}` line on stdout that Go parses (`action.go:131-200`: outputs, env,
  cache updates).
- Host APIs the bundled action uses (`tangy/policy-requester-tangled-vouch/dist/index.js`,
  300 KB): `Deno.env.get`, `Deno.exit`, `Deno.readTextFileSync`, `Deno.writeTextFile(Sync)`
  (all through the shim), `Deno.stdout/stderr.writeSync` and `Deno.inspect` (shim), top-level
  `await`, and `fetch` (5 sites: did:web, plc.directory, `/.well-known/atproto-did`,
  `com.atproto.repo.getRecord` :9438-9448, `listRecords` :9451-9470). The trust lookups read
  `cache["trust/<counterparty>"]` first (`operatorOf.json`, `vouchedBy.json`, :9827-9840), which
  Go seeds from `trust.BuildSnapshotFor` only when the run reconciler has a `Host`
  (`internal/policyengine/controller/run.go:277-288`); without it the action fetches itself.
- Who writes the code: the bundled actions are ours (`executor.BundledActions`,
  `engine.go:509`); workflow text can come from a `PolicyWorkflow` record (`RecordRef`), and
  its `${{ }}` expressions are arbitrary JS. On the page (requester only) that is the
  requester's own chosen workflow; counterparty records arrive as data (JSON literals).

### The page today

- `socialweb-wasm/internal/socialweb/orchestrator.go:315-326`: engine factory with
  `Sandbox: worker.New()`, `DisableActions: true`, then only `policyenginectl.Register`. No
  `policyworkflow`/`policyworkflowrun` controllers and no trust `Host` are registered (compare
  socialweb `internal/controllers/policy/register.go:67-93`), so even a fenced runner would never
  see a `PolicyWorkflowRun`.
- socialweb `internal/sandbox/worker/worker.go`: a module Worker from a same-origin `blob:` URL
  (:77-83), program run with `new Function` (:51), `console.*` captured, timeout 2 min (:111-119).
  `Fenced()` false (:27-29) is correct: that Worker has the page's origin, so it can open the
  page's IndexedDB (OAuth sessions and DPoP keys, kcp state), Cache Storage and OPFS, join the
  page's `BroadcastChannel`s, `fetch` same-origin URLs as the page (dev server `/debug/*`),
  `import()`/`importScripts` anything, and spawn Workers. It also has no `Deno` global, so the
  action shim (`Deno.exit.bind`, `Deno.writeTextFile.bind`) would throw at line 1 anyway.
- No CSP, COOP or COEP on either host: `scripts/kubectl_wasm_web.ts` sets none, the Caddy site
  from `scripts/deploy.sh:100-110` sets only `Cache-Control` and the metadata `Content-Type`.

### The established wasm pattern: the WebRTC ingress

How the requester's WebRTC ingress (commit 4585d48e6 "webrtc ingress", plan 0034 for the guest
side) is built, layer by layer:

| Layer | WebRTC ingress | Where |
| --- | --- | --- |
| API value declared in socialweb, instantiated only by the wasm build | `IngressConfig{Provider: "webrtc", WebRTC: &IngressWebRTCConfig{STUNURLs, SessionTTLSeconds, MaxSessions}}` | socialweb `api/v1alpha1/types_application.go:216-236`; the page sets it in `createRoot` (`orchestrator.go:528-532`) and keeps it on restore (`:544-546`) |
| Schema | generated from socialweb, never hand-written: no wasm-only kinds exist | `socialweb-wasm/manifests/README.md:1-6` ("Do not edit by hand"), `marketrequester-apiresourceschema.yaml` (synced in 4585d48e6) |
| Contract interface + injection seam | `tunnel.Service{Offer, Accept, Close}`, `tunnel.ServiceOptions`; requester `Deps.OpenIngress` | socialweb `internal/contract/tunnel/tunnel.go:42-56`, `internal/services/requester/service.go:35, :153-170` |
| Native implementation | pion, build-tag split for js | socialweb `internal/ingress/webrtc/webrtc.go`, `settings_js.go` / `settings_other.go` |
| wasm implementation | `browserwebrtc.NewService` (`//go:build js && wasm`) and a stub that errors (`//go:build !js \|\| !wasm`) | `socialweb-wasm/internal/browserwebrtc/service.go:1, :79-123`, `service_other.go:1-23`; injected at `socialweb-wasm/internal/socialweb/requester_runner.go:59-60` |
| Page half | ES module imported by the kcp worker and published as one bridge object; Go refuses to start without it and installs its callbacks once | `scripts/browser/kcp_worker.js:159-164` -> `scripts/browser/webrtc_ingress.js`; `service.go:83-86` (`__socialwebWebRTC`), `:131-137` (`__socialwebWebRTC{Open,Data,Close,State}`) |
| State to the page | through resources, not globals: ContractEvents (guest `signalWebRTC`) reconciled by the contractevent controller registered in wasm, pushed to the page by the status feed | `orchestrator.go:335-344`; `socialweb-wasm/internal/statusfeed/feed.go:122-170, :238`; `kcp_worker.js:173-181` (`__kcpState`); page side `scripts/browser/webrtc_guest.js:296-320` |

Related: `socialweb-wasm/internal/browserinfra/{exposure,vaulthost}` substitute controllers for
existing kinds and write the same status shape as the real controllers
(`browserinfra/exposure/README.md` "Exact status written"; registered at `orchestrator.go:208,
:233`). `contrib/wasm/apis/fetch` (`fetch.kcp.io` Fetch CRD) is the older kcp-only demo and not
part of the socialweb pattern.

Consequence for this plan: no new `PolicySandbox` kind (it would be the only hand-written
schema in `socialweb-wasm/manifests`). The sandbox becomes a provider value on the existing
`PolicyEngine` kind, declared in socialweb like `IngressConfig.Provider`, with the probe result
on `PolicyEngine.status.conditions`.

## Options

| | Page-origin authority | Kill / timeout | Memory cap | iOS Safari | Size / boot | Same results as Deno |
| --- | --- | --- | --- | --- | --- | --- |
| (a) `sandbox="allow-scripts"` srcdoc iframe (opaque origin) + per-run module Worker it creates | none: opaque origin has its own (or no) storage, no page DOM, no page channels; fetch carries `Origin: null`, no credentials | `worker.terminate()` from the iframe; iframe removal as backstop | none (as Linux) | sandbox attribute and module Workers are supported; **Worker from `blob:null/...` inside an opaque iframe must be proven on the iPhone** (step 0) | 0 new bytes; ~1 thread + JSC VM per run | same JS, native engine; only the `Deno` shim differs |
| (b) Worker from blob/data URL under a CSP | blob: same origin, CSP does not restrict IndexedDB/OPFS; data: gives an opaque origin but Worker support and CSP inheritance vary by engine | `terminate()` | none | data: Workers in WebKit uncertain | 0 | same |
| (c) QuickJS-ng (or Boa) compiled to wasm, in a Worker | none by construction: only the host functions we expose | interrupt handler + `terminate()` | yes (`JS_SetMemoryLimit`) | works anywhere wasm works | ~1 MB wasm + polyfills (`URL`, `TextEncoder`, `AbortController`, `setTimeout`, `fetch`, `Deno.*`); interpreter speed | different engine: 300 KB bundles with modern syntax/regex/Intl may diverge |
| (d) ShadowRealm | not a security boundary (same agent, same thread) | none: a loop blocks the caller | none | not shipped (flag only) | 0 | n/a |

Not considered further: a cross-origin iframe on a second host (`sandbox.<domain>`) gives the
same isolation as (a) but needs another deploy; JS-level "delete `indexedDB`/`fetch`" hardening
in a same-origin Worker is bypassable (`import()` cannot be deleted) and is not a fence.

## Design (recommended: option (a), wired like the WebRTC ingress)

The browser enforces the fence (origin isolation), as deno enforces it with permission flags,
and the program runs on the same kind of JS engine with nothing new to download. Option (c)
stays the fallback if step 0 fails on the iPhone.

### 0. Feasibility probe on the iPhone (before any other code)

One `phone.sh` snippet, run on the iPhone and in headless Chrome: create the srcdoc iframe of
section 4, have it build a module Worker from a `blob:` URL (then a `data:` URL), post back
`self.origin`, `typeof Deno`, whether top-level `await` works, and the facts of section 5.
Go/no-go for (a); if blob and data Workers both fail in Safari, stop and write the (c) plan.

### 1. API (socialweb; request linux-kcp)

Mirror `IngressConfig` on `PolicyEngineSpec` (`api/policy/v1alpha1/types_policyengine.go:22-32`):

```go
Sandbox *PolicySandboxConfig `json:"sandbox,omitempty"`

type PolicySandboxConfig struct {
	Provider string `json:"provider,omitempty" apigen:"enum=deno|browser"` // empty = deno
	Browser *PolicyBrowserSandboxConfig `json:"browser,omitempty"`
}

type PolicyBrowserSandboxConfig struct {
	TimeoutSeconds *int32 `json:"timeoutSeconds,omitempty" apigen:"default=120,minimum=1"`
}
```

Status needs no new field: `PolicyEngineStatus.Conditions` (`:72-80`) gets a `Fenced` condition
type. Then `make sync-socialweb-manifests` here (`policyengine-apiresourceschema.yaml`).

### 2. Contract (socialweb; request linux-kcp)

- `internal/contract/sandbox`: an optional interface next to `Runner` (`sandbox.go:30-38`),
  type-asserted, so the deno runner and every caller stay unchanged:

  ```go
  type Fence struct { Fenced bool; Reason, Message string }
  type Prober interface { Probe(ctx context.Context) (Fence, error) }
  ```

- `internal/policyengine/controller`: `Config` (`engine.go:39-51`) gains
  `OpenSandbox func(ctx context.Context, spec *policyv1alpha1.PolicySandboxConfig) (sandbox.Runner, error)`,
  the counterpart of requester `Deps.OpenIngress`; the default factory uses it when set, else
  `Config.Sandbox`. A `browser` provider with no `OpenSandbox` fails the factory, which
  `manager.go:82-91` already reports as `Ready=False, reason CreateFailed` (a native host never
  pretends to run the browser sandbox).
- `manager.go` `reconcileState` (:111-131): if the runner is a `Prober`, call it when the engine
  is created and write `Fenced` (`True`/`False`, reason `ProbePassed`/`ProbeFailed`/`NotProbed`,
  message = the compact fact list); when not ready, use that reason instead of the hard-coded
  deno message (:125-130). `Engine.Ready()` stays `Available() && Fenced()`.
- `internal/controllers/policy.Options` (`register.go:25-41`) passes `OpenSandbox` through.

### 3. wasm implementation: `socialweb-wasm/internal/browsersandbox/` (new)

Same shape as `browserwebrtc`:

- `runner.go` (`//go:build js && wasm`): `New(cfg)` returns an error when the page has no
  `__socialwebSandbox` bridge object with `run`/`kill`/`probe` functions (as `service.go:83-86`).
  It implements `sandbox.Runner` and `sandbox.Prober`: `Kind() = "browser"`,
  `Available()` = bridge present, `Fenced()` = last `Probe` passed; `Probe` calls
  `__socialwebSandbox.probe()` and caches the `Fence`.
- `Run`: refuses `Dir != ""` and `Net == false` with an error (the page only runs policy
  programs; the bidder compute kind is not served by the page and must not get a weaker fence
  than it asked for). Marshals `{id, source, env, timeoutMs}`, awaits the Promise from a
  goroutine (pattern `cmd/kcp-wasm/refresher_js.go:33-60`; never from a `js.FuncOf` callback,
  socialweb `CONTRIBUTING.md:1080-1084`), returns `TimedOut` on its own timer or `ctx.Done()` and
  calls `kill(id)`. Output capped at 64 KiB per stream (`deno.go:22`). One run at a time
  (`chan struct{}` of 1) to bound iOS memory: an evaluation is about a dozen `Run`s (one per
  `${{ }}` plus the action), sequential anyway.
- `runner_other.go` (`//go:build !js || !wasm`): `New` returns an error (as
  `service_other.go`), so native tests in `socialweb-wasm` build.

### 4. Page half: `scripts/browser/policy_sandbox.js` (new), `kcp_worker.js`, `app.js`

- `startFrame()` (main thread, `app.js`): appends one hidden
  `<iframe sandbox="allow-scripts" srcdoc="...">` per tab (outlives kcp worker restarts like
  the refresh worker, `app.js:714-720`). No `allow-same-origin`, `allow-popups`,
  `allow-top-navigation`, `allow-forms`, `allow-modals`. The srcdoc meta CSP is defense in depth
  (the page has none): `default-src 'none'; script-src 'unsafe-inline' blob:; worker-src blob:;
  connect-src https:` (keeps PDS/plc fetches, drops `http:`/`ws:`; WebKit's CSP inheritance into
  blob Workers is recorded by the probe, not relied on).
- The iframe script takes one `MessagePort`. Per request `{id, source, env, timeoutMs}` it
  builds a module Worker from `blob:[BOOTSTRAP, source, FOOTER]`, posts
  `{exitCode, stdout, stderr, timedOut}` back, and `terminate()`s the Worker on answer, at
  `timeoutMs`, or on `{kill: id}`. One Worker per `Run`, as deno is one process per `Run`.
- `BOOTSTRAP` gives the program what deno gives it under the policy flags and nothing of the
  page: a `Deno` object with `env` (from `Program.Env`), `exit` (posts the result, closes),
  `stdout/stderr.writeSync` (decoded text, 64 KiB cap), `inspect` (`JSON.stringify`, fallback
  `String`), `readTextFile(Sync)`/`writeTextFile(Sync)` that throw `NotCapable` (deno without
  `--allow-read`; the action shim intercepts the virtual paths first), `console.log/info/debug`
  -> stdout and `console.warn/error` -> stderr (deno's split), `unhandledrejection`/`error` ->
  exit 1. `FOOTER` posts exit 0 once the module's top-level `await` completes. `fetch` is the
  Worker's own.
- `app.js` `spawnKcp` (`:735-760`): `startFrame()` in the existing `Promise.all`; a
  `MessageChannel`, `port2` to the iframe, `port1` transferred in the kcp `init` message
  (`policySandboxPort`). The page main thread is not in the data path.
- `kcp_worker.js`, next to the WebRTC bridge (`:159-164`):
  `if (init.policySandboxPort) globalThis.__socialwebSandbox = sandbox.connect(init.policySandboxPort)`,
  where `connect` returns `{run, kill, probe}`. No `__kcpSandbox*` globals.

### 5. Probe, and how Ready flips (through the resource)

`probe()` runs in the kcp worker (page origin, so it can plant markers): it writes a random
marker into the page's IndexedDB (`kcp-fence-probe`), Cache Storage and OPFS and opens a
`BroadcastChannel`, then runs a probe program through the frame. `Fenced` is true only if:

- `self.origin === "null"` inside the sandbox Worker;
- the marker is not readable there from IndexedDB, Cache Storage or OPFS
  (`navigator.storage.getDirectory()`), and the kcp worker's OPFS/IDB names are not listable;
- nothing arrives on the kcp worker's `BroadcastChannel`;
- `document`, `parent`, `top` are `undefined` in the Worker, and the iframe cannot read
  `window.parent.document`;
- a `Run` that never ends comes back `timedOut` and the next `Run` still works.

The `Fence` goes to the `PolicyEngine` status via section 2: `Fenced` condition with the facts,
and `Ready` (`manager.go:113-121`) becomes true only on a page where the probe passed. The run
controller, the requester's `PolicyEvaluator` and the page all read readiness from the resource.
Network is not denied, by design: the Linux runner grants `--allow-net` to policy code
(`expression.go:300`); what the page must deny is the page's own authority, and the probe proves
exactly that.

### 6. Wiring and instantiation (this repo)

- `socialweb-wasm/internal/socialweb/orchestrator.go:315-326`: replace the engine factory and
  `policyenginectl.Register` with socialweb's
  `internal/controllers/policy.Register(mgr, policy.Options{Bus, OpenSandbox: browsersandbox.Open, Log})`.
  That registers PolicyEngine, PolicyWorkflow, PolicyWorkflowRun and PolicyTrigger controllers,
  the trust resolver `Host` (so `trust/<counterparty>` is seeded by Go over the page's existing
  CORS fetch path, `run.go:277-288`) and bundled actions (`manager.go:247` resets
  `DisableActions`). Check during implementation that `Actions: nil` is fine for policytrigger
  (`controllers/policytrigger/controller.go:43-48`) and that the four policy schemas in
  `socialweb-wasm/manifests` are bound.
- Instantiation, like `createRoot` setting `Provider: webrtc`: the wasm copy of the requester
  app (`socialweb-wasm/overlays/app/marketrequester.go:255`) sets
  `Sandbox: {Provider: browser}` on `compose.PolicyEngineDesired()` (socialweb
  `internal/app/compose/exposures.go:215`). Native hosts never set it.
- Status to the page: add `PolicyEngineGVR` to the status feed (`statusfeed/feed.go:153-158`)
  with `phase`, `ready` and the `Fenced` condition message, and show "policy sandbox: fenced /
  not fenced (<reason>)" in the Compute panel.

### 7. Dev server hardening: `scripts/kubectl_wasm_web.ts`

Reject `Origin: null` on `/debug/*` so sandboxed code cannot write debug log lines or reach the
control stream on the dev server. Production has no `/debug/*`.

### 8. Bundled tangled-vouch action: stable rkeys + single getRecord (socialweb; note for linux-kcp)

The action's own lookups list whole collections: operator via
`#listRecords(did, badgeBlueKeys, 100)` filtered client side (dist `:9476-9498`), vouch set via
`#listRecords(did, sh.tangled.graph.vouch, 100)` keyed by rkey (`:9500-9510`), paginated
(`:9451-9470`). Go's `trust.go` does the same (`ResolveOperatorDidFor` `:103-138` and
`GetVouchedDids` `:147-188` call `ListRecords`). The owner prefers stable rkeys and a single
`com.atproto.repo.getRecord`; linux-kcp-rkeys is converting `trust.go`. The action
(`@publicdomainrelay/policy-deno-typescript`, rebuilt into `dist/index.js`) should follow in the
same change: vouch = `getRecord(self, sh.tangled.graph.vouch, rkey=<operator DID>)` (the rkey is
already the vouched DID), operator = `getRecord(subject, badgeBlueKeys, rkey=<stable rkey>)`.
For the page this means one CORS GET per lookup instead of up to N pages, and a seeded
`trust/<counterparty>` stays the normal path.

## Verification

1. Step 0 probe on the iPhone (`phone.sh`) and headless Chrome (`page-hold.ts`).
2. Golden programs: a native Go test in `socialweb-wasm` runs the `requester-tangled-vouch`
   workflow with the deno runner wrapped in a recorder, fixed inputs (self DID, counterparty DID,
   RFP/Bid refs, `mode: scope`, seeded `cache["trust/<did>"]` for one allow and one deny case, so
   no network), and writes every `Program`, `Result` and the verdict to
   `socialweb-wasm/internal/browsersandbox/testdata/*.json`.
3. `deno test` in `scripts/browser` (`policy_sandbox_test.js`): BOOTSTRAP + each golden
   `Program` in a module Worker; `exitCode` and `stdout` (incl. the `__PE_RESULT__` line) equal
   golden; 64 KiB cap; an endless program is killed; `Deno.readTextFileSync("/etc/hosts")` fails
   like deno without `--allow-read`.
4. socialweb (linux-kcp): a manager test with a fake `Prober` runner asserting `Fenced` and
   `Ready` conditions for pass and fail, and a fake unfenced runner asserting Deny (no such test
   exists today).
5. Browser check (headless Chrome, then iPhone): `PolicyEngine` status shows `Fenced=True` with
   every fact; a sandbox `fetch(location.origin + "/kcp.wasm", {credentials: "include"})`
   arrives with `Origin: null` (dev server log).
6. End to end on the page: `d.rest` creates a `PolicyWorkflowRun` for the golden allow/deny
   inputs; verdicts equal the Linux runner's. A debug switch that fails the probe shows
   `Fenced=False`, `Ready=False` with the reason, and Deny.
7. iPhone memory: memstats and tab survival through 20 evaluations; wall time per evaluation in
   the debug log.
8. `browser-demo.ts` VM run with the requester workflow set (after rollout step 4); release the VM.

## Rollout

1. linux-kcp: sections 1, 2 (and 8 with the rkeys work); release a socialweb commit.
2. This repo: sections 3-7 on `kcp-wasm-memory-storage-1` against that commit
   (`REV=<commit> DEPLOY=1 build-page.sh`, manifests sync committed). With no workflow set on
   the page's MarketRequester nothing runs yet (`internal/market/requester/policy.go:30-43`), so
   it ships on, no switch.
3. Verification 1-7 on headless Chrome and the iPhone.
4. linux-kcp flips the requester default workflow (`requester-tangled-vouch`; page copy
   `socialweb-wasm/overlays/app/marketrequester.go:36, :146`); the page picks it up with the next
   `build-page.sh` sync. Then verification 8.

## Risks / open items

- WebKit may refuse a Worker from a `blob:null` URL inside an opaque-origin iframe; step 0
  decides, fallback is (c) QuickJS-ng in a plain Worker behind the same `__socialwebSandbox`
  bridge (~1 MB, polyfills, engine drift).
- No memory cap: a runaway program can push iOS past its limit and kill the whole tab (Linux has
  no cap either, but there it kills one process). Mitigated by one run at a time and the engine's
  run deadline; (c) is the only option with a real cap.
- iframe code shares the page's main thread on iOS (no site isolation), so only the per-run
  Worker may run program code; the iframe script must stay a thin relay.
- The sandbox can still make CORS requests with `Origin: null` to any https host (same as
  `--allow-net` on Linux). Observation for the owner, not a proposal: on Linux `--allow-net`
  without `--no-remote` also lets policy code reach `127.0.0.1` services and remote `import()`.
- A future page CSP must keep `worker-src blob:` and allow `about:srcdoc` frames, or the probe
  fails closed (`Fenced=False`, Deny).
- JSC vs V8 differences in the bundled actions; the golden test catches the ones we exercise.
- Sections 1-2 are socialweb changes owned by linux-kcp; until they land the page cannot follow
  the pattern (rev 1's globals would work without them, but the owner asked for the pattern).

Open questions for the owner:
- Is network-allowed policy code (parity with Linux `Net: true`) acceptable in the page, or do
  you want it stricter (`connect-src` allowlist / pre-seeded trust only; section 8 makes the
  seeded path the normal one)?
- Is (a) acceptable without a memory cap on iOS, or is the cap worth (c)'s ~1 MB and drift?
- `spec.sandbox.provider` on PolicyEngine (this rev) vs keeping only the existing
  `Config.Sandbox` injection plus the `Fenced` condition (smaller socialweb diff, no API field)?
- Before rollout step 4: does aliceoa's Tangled vouch graph include the lead's bidder operators?
  If not, every winner is rejected (`ContractOutcomePolicyRejected`, `runner.go:490-493`).

## Results

- 2026-10-30 headless Chrome (drives-oauth, dev fence page, fenced-sandbox e97abe788 @ socialweb 8d79dff0, alice signed in, no VM): `policy sandbox: fenced, ready (ProbePassed: idbPlanted=yes cachePlanted=yes origin=null shim=ok dom=none idb=denied:SecurityError cache=denied:SecurityError opfs=denied:SecurityError bc=silent frameOrigin=null parent=blocked timeout=killed rerun=ok)` at 09:16:25Z. PASS on Chromium.
- Fixed on the way: (1) the page PolicyEngine had no sandbox provider (defaulted to deno, CreateFailed); socialweb 8d79dff0 adds `PolicyEngineSettings.sandbox` and the page createRoot sets `provider=browser` (576416dcf). (2) Chromium refuses module Workers from `blob:null` in the opaque frame ("Refused to cross-origin redirects of the top-level worker script"); the per-run Worker now starts from `data:`, CSP `worker-src data:` (e97abe788). Check: `scripts/browser-dev/policy-sandbox-check.ts`.
- Pending: iOS Safari check of this build (owner, 14:00Z).
- 2026-10-30 fence VM run (drives-oauth, fenced-sandbox f6f16a9fd: page contracts carry `spec.policy: requester-tangled-vouch`): PolicyEngine fenced=True 09:22:06Z; PolicyWorkflowRun 18bf6111 Succeeded 09:22:26.5Z in the browser sandbox; hat-7 (bob) bid allowed, accept 09:22:27.8Z, Held 42.6 s after the click, Released 09:22:53Z, hat-7 orphan check clean. PASS.
- Fail closed (socialweb 8d79dff0): a PolicyWorkflowRun waits Pending EngineUnavailable until `engine.Ready()` (`internal/controllers/policyworkflowrun/controller.go` engineFor), which needs `Sandbox.Fenced()` (`internal/policyengine/controller/engine.go` Ready); `workflow/expression.go` runDeno refuses when not fenced; the page runner's `Fenced()` is set only by a passing probe.
