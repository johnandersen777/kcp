# Fenced policy sandbox in the page: opaque-origin iframe + per-run Worker (rev 1, plan only)

Status: proposed, waiting for owner approval. No code is written until it is approved.

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
  `manager.go:113` turns that into the PolicyEngine `Ready` condition; the run controller
  (`internal/controllers/policyworkflowrun/controller.go:159-173`) only runs on a ready engine.
  A refused expression falls back to the pure-Go property-path resolver (`expression.go:108-121`),
  an action step fails, the verdict is Deny (`CONTRIBUTING.md:1121-1127`).
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
  `com.atproto.repo.getRecord`, `listRecords`, around lines 9147-9462). The trust lookups read
  `cache["trust/<counterparty>"]` first (`operatorOf.json`, `vouchedBy.json`, dist ~9827), which
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
- The kcp Go runtime lives in `scripts/browser/kcp_worker.js` (a dedicated Worker, no DOM). The
  page already has the bridge shape needed: Go calls a global JS function that returns a Promise,
  the kcp worker relays to a thread the page owns (`cmd/kcp-wasm/refresher_js.go:19-60`,
  `kcp_worker.js:217-234`, `app.js:569-573`, `:714-720`).
- No CSP, COOP or COEP on either host: `scripts/kubectl_wasm_web.ts` sets none, the Caddy site
  from `scripts/deploy.sh:100-110` sets only `Cache-Control` and the metadata `Content-Type`.

## Options

| | Page-origin authority | Kill / timeout | Memory cap | iOS Safari | Size / boot | Same results as Deno |
| --- | --- | --- | --- | --- | --- | --- |
| (a) `sandbox="allow-scripts"` srcdoc iframe (opaque origin) + per-run module Worker it creates | none: opaque origin has its own (or no) storage, no page DOM, no page channels; fetch carries `Origin: null`, no credentials | `worker.terminate()` from the iframe; iframe removal as backstop | none (as Linux) | sandbox attribute and module Workers are supported; **Worker from `blob:null/...` inside an opaque iframe must be proven on the iPhone** (step 0) | 0 new bytes; ~1 thread + JSC VM per run | same JS, native engine; only the `Deno` shim differs |
| (b) Worker from blob/data URL under a CSP | blob: same origin, CSP does not restrict IndexedDB/OPFS; data: gives an opaque origin but Worker support and CSP inheritance vary by engine and it is not killable from Go without the same relay | `terminate()` | none | data: Workers in WebKit uncertain | 0 | same |
| (c) QuickJS-ng (or Boa) compiled to wasm, in a Worker | none by construction: only the host functions we expose | interrupt handler + `terminate()` | yes (`JS_SetMemoryLimit`) | works anywhere wasm works | ~1 MB wasm + polyfills (`URL`, `TextEncoder`, `AbortController`, `setTimeout`, `fetch`, `Deno.*`); interpreter speed | different engine: 300 KB bundles with modern syntax/regex/Intl may diverge |
| (d) ShadowRealm | not a security boundary (same agent, same thread) | none: a loop blocks the caller | none | not shipped (flag only) | 0 | n/a |

Not considered further: a cross-origin iframe on a second host (`sandbox.<domain>`) gives the
same isolation as (a) but needs another deploy and OAuth-free origin; JS-level "delete
`indexedDB`/`fetch`" hardening in a same-origin Worker is bypassable (`import()` cannot be
deleted) and is not a fence.

## Design (recommended: option (a))

The browser enforces the fence (origin isolation), exactly as deno enforces it with permission
flags, and the program runs on the same kind of JS engine with nothing new to download.
Option (c) stays the fallback if step 0 fails on the iPhone.

### 0. Feasibility probe on the iPhone (before any other code)

One `d.call`/`phone.sh` snippet, run on the iPhone and in headless Chrome: create the srcdoc
iframe below, have it build a module Worker from a `blob:` URL (then a `data:` URL), post back
`self.origin`, `typeof Deno`, whether top-level `await` works, and the storage facts of the
probe in section 3. Go/no-go for (a); if blob and data Workers both fail in Safari, stop and
write the (c) plan instead.

### 1. Page sandbox: `scripts/browser/policy_sandbox.js` (new)

- `startPolicySandbox()` (main thread) appends a hidden
  `<iframe sandbox="allow-scripts" srcdoc="...">` once per tab (it outlives kcp worker restarts
  like the refresh worker, `app.js:714-720`). No `allow-same-origin`, `allow-popups`,
  `allow-top-navigation`, `allow-forms` or `allow-modals`.
- The srcdoc carries a meta CSP as defense in depth (it only tightens; the page has none):
  `default-src 'none'; script-src 'unsafe-inline' blob:; worker-src blob:; connect-src https:`.
  `connect-src https:` keeps policy fetches to PDS/plc working and drops `http:`/`ws:` (LAN,
  localhost over http). It does not gate `Ready` (WebKit's CSP inheritance into blob Workers is
  recorded by the probe, not relied on).
- The iframe script listens for one `MessagePort` from the page. Per request
  `{id, source, env, timeoutMs}` it creates a module Worker from
  `blob:[BOOTSTRAP, source, FOOTER]`, posts `{exitCode, stdout, stderr, timedOut}` back on the
  port, and `terminate()`s the Worker when it answers, at `timeoutMs`, or on `{kill: id}`.
- `BOOTSTRAP` gives the program what deno gives it under the policy flags and nothing of the
  page: a `Deno` object with `env` (from `Program.Env`), `exit` (posts the result, closes),
  `stdout/stderr.writeSync` (append decoded text, 64 KiB cap each like `deno.go:22`), `inspect`
  (`JSON.stringify` fallback `String`), `readTextFile(Sync)`/`writeTextFile(Sync)` that throw
  `NotCapable` (deno without `--allow-read`; the action shim intercepts the virtual paths before
  they are reached), `console.log/info/debug` -> stdout and `console.warn/error` -> stderr (deno's
  split), `unhandledrejection`/`error` -> exit code 1. `FOOTER` posts exit 0 after the module's
  top-level `await` completes, which is when deno would exit. `fetch` is the Worker's own.
- One Worker per `Run`, as deno is one process per `Run`: no state carries between runs.

### 2. kcp worker relay: `scripts/browser/kcp_worker.js`, `scripts/browser/app.js`

- `app.js` `spawnKcp` (`:735-760`): `await startPolicySandbox()` in the existing `Promise.all`,
  make a `MessageChannel`, give `port2` to the iframe and transfer `port1` in the kcp `init`
  message with `policySandbox: {fenced}`. The page main thread is not in the data path.
- `kcp_worker.js`: if `init.policySandbox` is present, define
  `globalThis.__kcpSandboxFenced = init.policySandbox.fenced === true` and
  `globalThis.__kcpSandboxRun(programJSON) -> Promise<resultJSON>` over `port1`, plus
  `__kcpSandboxKill(id)`, same shape as `__kcpRefreshTokens` (`:221-234`).

### 3. Proving the fence, and how Ready flips

`startPolicySandbox()` runs a probe program through the same path before kcp boots (tens of ms,
parallel to the kcp.wasm fetch). Before the probe the page writes a random marker into its own
IndexedDB (`kcp-fence-probe`), Cache Storage and OPFS, and listens on a `BroadcastChannel`. The
probe reports what it can see. `fenced` is true only if all hold:

- `self.origin === "null"`;
- the marker is not readable from IndexedDB (open fails or yields an empty new database), Cache
  Storage and OPFS (`navigator.storage.getDirectory()`), and the page's kcp OPFS/IDB names are
  not listable;
- the page receives nothing on the probe's `BroadcastChannel` post;
- `typeof document`, `parent`, `top` are `undefined` in the Worker, and the iframe's
  `window.parent.document` access throws (cross-origin);
- a `Run` that never ends comes back `timedOut` and the next `Run` still works (kill works).

Facts and verdict go to the debug log (`policy sandbox fenced=<bool> <facts>`). Ready then flips
with no socialweb change: the new Go runner's `Fenced()` reads `__kcpSandboxFenced` set at init,
so `Engine.Ready()` (`engine.go:165`) and the PolicyEngine `Ready` condition (`manager.go:113`)
become true only on a page where the probe passed. Network is not denied, by design: the Linux
runner grants `--allow-net` to policy code (`expression.go:300`). What the page must deny is the
page's own authority, and the probe proves exactly that.

### 4. Go runner: `socialweb-wasm/internal/pagesandbox/runner_js.go` (new)

- Implements `internal/contract/sandbox.Runner` (allowed: module path
  `github.com/johnandersen777/socialweb-computer-kcp/wasm` is under the socialweb root).
  `Kind() = "page-iframe"`, `Available()` = `__kcpSandboxRun` is a function, `Fenced()` =
  `__kcpSandboxFenced === true`.
- `Run`: refuses `Dir != ""` and `Net == false` with an error (the page only runs policy
  programs; the bidder compute kind is not served by the page and must not get a weaker fence
  than it asked for). Marshals `{id, source, env, timeoutMs}` (default 2 min like
  `deno.go:20`), awaits the Promise from a goroutine (pattern `refresher_js.go:33-60`; never from
  a `js.FuncOf` callback, `CONTRIBUTING.md:1080-1084`), returns `TimedOut` on its own timer or
  `ctx.Done()` and calls `__kcpSandboxKill(id)`.
- One run at a time (`chan struct{}` of 1) to bound iOS memory: an evaluation is about a dozen
  `Run`s (one per `${{ }}` plus the action), sequential anyway.

### 5. Wiring: `socialweb-wasm/internal/socialweb/orchestrator.go:315-326`

Replace the engine factory and `policyenginectl.Register` with socialweb's
`internal/controllers/policy.Register(mgr, policy.Options{Bus: cfg.Bus, Sandbox:
pagesandbox.New(), Log: ...})`. That registers PolicyEngine, PolicyWorkflow, PolicyWorkflowRun
and PolicyTrigger controllers, the trust resolver `Host` (so `trust/<counterparty>` is seeded by
Go over the page's existing CORS fetch path, `run.go:277-288`) and bundled actions
(`manager.go:247` resets `DisableActions`). Check during implementation that `Actions: nil` is
fine for policytrigger (`controllers/policytrigger/controller.go:43-48`) and that the four
schemas in `socialweb-wasm/manifests` (policyengine, policyworkflow, policyworkflowrun,
policytrigger) are all bound. If `policy.Register` does more than the page wants, compose the
same calls directly in the orchestrator.

### 6. Dev server hardening: `scripts/kubectl_wasm_web.ts`

Reject `Origin: null` on `/debug/*` so sandboxed code cannot write debug log lines or reach the
control stream on the dev server. Production has no `/debug/*`.

### socialweb interface changes

None required. `worker.Fenced()` stays false (it is right for that runner). Optional later
(linux-kcp): upstream the page runner as `internal/sandbox/pageframe`, and add a socialweb test
that runs a non-fenced fake runner and asserts Deny (none exists today).

## Verification

1. Step 0 probe on the iPhone (session via `phone.sh`) and headless Chrome (`page-hold.ts`):
   blob/data Worker inside the opaque iframe, facts logged.
2. Golden programs: a native Go test in `socialweb-wasm` (build tag `!js`) runs the
   `requester-tangled-vouch` workflow with the deno runner wrapped in a recorder, fixed inputs
   (self DID, counterparty DID, RFP/Bid refs, `mode: scope`, seeded
   `cache["trust/<did>"]` with `operatorOf.json`/`vouchedBy.json` for one allow and one deny
   case, so no network), and writes every `Program` and `Result` plus the verdict to
   `socialweb-wasm/internal/pagesandbox/testdata/*.json`.
3. `deno test` in `scripts/browser` (`policy_sandbox_test.js`): runs the BOOTSTRAP + each golden
   `Program` in a module Worker and asserts `exitCode`, `stdout` (incl. the `__PE_RESULT__` line)
   equal to golden; output cap at 64 KiB; an endless program is killed; a program reading
   `Deno.readTextFileSync("/etc/hosts")` fails like deno without `--allow-read`.
4. Browser check (headless Chrome, then iPhone): the probe returns `fenced=true` with every fact;
   a `fetch(location.origin + "/kcp.wasm", {credentials: "include"})` from the sandbox carries
   `Origin: null` (dev server log); `import("https://example.invalid/x.js")` fails under the
   iframe CSP where inherited (recorded, not gating).
5. End to end on the page: `d.rest` creates a `PolicyWorkflowRun` for the golden allow/deny
   inputs; the verdicts equal the Linux runner's. PolicyEngine shows `Ready=True` only with the
   probe passing (force `fenced=false` via a debug switch and see `Pending` + Deny).
6. iPhone memory: memstats and tab survival through 20 evaluations; record wall time per
   evaluation in the debug log.
7. `browser-demo.ts` VM run with the requester workflow set (after rollout step 3), release the VM.

## Rollout

1. Land 1-6 above on `kcp-wasm-memory-storage-1` (wasm page agent owns `scripts/browser/*` and
   `socialweb-wasm/*`); `REV=<socialweb> DEPLOY=1 build-page.sh`. With no workflow set on the
   page's MarketRequester nothing runs yet (`internal/market/requester/policy.go:30-43`), so
   this is safe to ship on, no switch.
2. Verification 1-6 on headless Chrome and the iPhone.
3. linux-kcp flips the requester default workflow (`requester-tangled-vouch`; page copy
   `socialweb-wasm/overlays/app/marketrequester.go:36, :146`) after 2 passes; the page picks it up
   with the next `build-page.sh` sync.
4. Phone VM test with the flipped default.

## Risks / open items

- WebKit may refuse a Worker from a `blob:null` URL inside an opaque-origin iframe; step 0
  decides, fallback is (c) QuickJS-ng in a plain Worker (~1 MB, polyfills, engine drift).
- No memory cap: a runaway program can push iOS past its limit and kill the whole tab (Linux has
  no cap either, but there it kills one process). Mitigated by one run at a time and the engine's
  run deadline; (c) is the only option with a real cap.
- iframe code shares the page's main thread on iOS (no site isolation), so only the per-run
  Worker may run program code; the iframe script must stay a thin relay.
- The sandbox can still make CORS requests with `Origin: null` to any https host (same as
  `--allow-net` on Linux). Observation for the owner, not a proposal: on Linux `--allow-net`
  without `--no-remote` also lets policy code reach `127.0.0.1` services and remote `import()`.
- A future page CSP must keep `worker-src blob:` and allow `about:srcdoc` frames, or the probe
  fails closed (Deny, not unfenced runs).
- JSC vs V8 differences in the bundled actions; the golden test catches the ones we exercise.

Open questions for the owner:
- Is network-allowed policy code (parity with Linux `Net: true`) acceptable in the page, or do
  you want the page stricter (`connect-src` allowlist / pre-seeded trust only)?
- Is (a) acceptable without a memory cap on iOS, or is the cap worth (c)'s ~1 MB and drift?
- Before step 3: does aliceoa's Tangled vouch graph include the lead's bidder operators? If not,
  the flipped default rejects every winner (`ContractOutcomePolicyRejected`, `runner.go:490-493`).
