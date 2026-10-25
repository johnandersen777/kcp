# AGENTS.md: browser kcp + socialweb (iPhone) test handoff

This fork (`origin` https://github.com/johnandersen777/kcp, branch
`kcp-wasm-memory-storage`, tag `iphone-working-v0`) runs real kcp and the
socialweb control plane compiled to js/wasm in a browser tab. The page
requests a VM from the socialweb market (bidders run by the lead),
waits for it to be Held, and opens a shell on it: `ssh` over WebRTC from the
page's terminal (primary), or the wootty web shell over opentunnel (fallback).

State at `iphone-working-v0` (2026-10-25): on an iPhone in Firehose mode (no
ingress), request -> Held 40-72 s, ssh datachannel open 4-5 s after the offer
(host/udp on the home LAN), release through the tab's EphemeralInfra. Idle
allocation ~100-150 MiB/min, 3-4 GCs/min. Open: 1-3 GB/min while a contract
runs, from socialweb re-applying unchanged ComponentLinks/EventSources/
EventStreams (~900 no-op applies/min); the lead is fixing it.

## Layout

| Path | What |
| --- | --- |
| `~/src/kcp/kcp-wasm` | main checkout on the lead's host (hat-8, Fedora, user johnandersen777), branch `kcp-wasm-memory-storage`; the dev server serves this tree (page JS from `scripts/browser/`, wasm from `bin/wasm/`) |
| `~/src/kcp/kcp-wasm/.claude/worktrees/<name>` | worktrees (agents get one each): edit and build here, then fast-forward the main checkout |
| `bin/wasm/` | built wasm, not in git; a fresh worktree copies it from the main checkout (`cp -a ~/src/kcp/kcp-wasm/bin/wasm/. bin/wasm/`) |
| `~/src/kcp/socialweb-computer-kcp` | socialweb source (the lead's; read only for agents; `git -c safe.directory='*'`) |
| `~/.tmp/claude-kcpwasm` | dev-server debug files (`debug.log`, `control.ndjson`, profiles) and the lead's helpers: `takeover.sh`, `tunnel-watchdog.sh`, `page-hold.ts`, `boot-smoke.ts` |
| `scripts/browser/` | the page: `app.js`, `kcp_worker.js`, `oauth_refresh_worker.js`, `vmshell.js`, `webrtc_guest.js`, `ssh_cmd.js`, `identity_ui.js`, `debug_link.js` (debug log + control stream) |
| `socialweb-wasm/` | Go glue: `cmd/kcp-wasm` (wasm main, refresher bridge, memstats), `internal/socialweb/orchestrator.go` (MarketRequester `app`, comms mode, events.Register), `overlays/compose/compose.go` (Config.Rest) |
| `contrib/wasm/storage` | kcp's in-memory storage for wasm (per-resource update counters live here) |
| `scripts/prepare-kcp-patched-modules.sh` | patches k8s modules for wasm (incl. the JSON fast path for apply bodies) |
| `scripts/kubectl_wasm_web.ts` | dev server (static files + `/debug/*` hooks) |
| `scripts/browser-demo.ts` | drives a phone through the whole VM test and records it to `demos/` |
| `scripts/browser-dev/{build-page.sh,phone.sh}` | build/deploy and one-shot page commands |
| `contrib/wasm/FUTURE_WORK.md` | open items (build-time snapshot secrets, Firehose on a phone) |

## Build and deploy

```sh
export TMPDIR=~/.tmp                                                        # never /tmp
cd ~/src/kcp/kcp-wasm/.claude/worktrees/<name>
REV=<socialweb commit, e.g. main> DEPLOY=1 scripts/browser-dev/build-page.sh   # ~1 min incremental
git add ...; git commit                                                     # commit in the worktree
git -C ~/src/kcp/kcp-wasm merge --ff-only <worktree branch>                 # page JS is live now
git -C ~/src/kcp/kcp-wasm push origin kcp-wasm-memory-storage
# helper modules (kubectl, kubectl-slim, goat, yq, ssh) into bin/wasm, rarely needed:
make build-kubectl-wasm build-goat-wasm build-yq-wasm build-sshclient-wasm  # goat/yq: GO_CLI_TOOLCHAIN=go1.27.3 (the default)
```

Notes:
- `build-page.sh` defaults: `SOCIALWEB_SRC=~/src/kcp/socialweb-computer-kcp`,
  `MAIN_CHECKOUT=~/src/kcp/kcp-wasm` (DEPLOY target), `TMPDIR=~/.tmp`.
- The build mirrors socialweb at REV into `.work/socialweb-mirror`, 3-way merges
  the compose.go overlay onto it and syncs `socialweb-wasm/manifests`
  (commit those). A new socialweb schema needs that sync or the page logs
  `field not declared in schema`.
- Merging into the main checkout deploys page JS immediately; `bin/wasm/kcp.wasm`
  is only replaced by `DEPLOY=1`. Do not merge half-finished page changes while a
  phone test runs.
- The page re-applies the socialweb manifests when the kcp.wasm ETag changes
  (recorded only after a full boot).
- Commit the user's own uncommitted edits in the main checkout before merging
  (small commit of their diff), never discard them.
- Browser unit tests: `cd scripts/browser && deno test --allow-read --allow-net`.
  Storage tests: `cd contrib/wasm/storage && GOTOOLCHAIN=go1.26.9 go test ./`.

## Dev server, public URL, phone

The lead runs the :8090 server and the tunnel; agents do not touch them and
start their own server on a free port for tests (no `--origin`, page at
`http://127.0.0.1:<port>/?debuglog=1`, debug files under `~/.tmp/<own dir>`).

```sh
D=~/.tmp/claude-kcpwasm; mkdir -p $D
cd ~/src/kcp/kcp-wasm && deno run --allow-read --allow-net --allow-write=$D \
  scripts/kubectl_wasm_web.ts --port=8090 \
  --origin=https://kcpwasm--johnandersen777-bsky-social.fedproxy.com \
  --debug-log=$D/debug.log --debug-control=$D/control.ndjson \
  2>&1 | tee $D/server8090.log                              # run in the background

bash $D/takeover.sh 2>&1 | tee -a $D/tunnel.log             # public tunnel, background
bash $D/tunnel-watchdog.sh 2>&1 | tee -a $D/watchdog.log    # background
```

- `takeover.sh` waits until the public URL fails twice (60 s apart), then
  loops `ssh -NnT -p 2222 ... -R kcpwasm:80:127.0.0.1:8090
  johnandersen777.bsky.social@fedproxy.com`.
- The fedproxy legacy ssh relay drops the whole forward after one failed
  forwarded-tcpip channel open while ssh stays connected, so
  `tunnel-watchdog.sh` kills that ssh when the public URL fails twice (checked
  every 60 s) and the takeover loop reconnects it.
- Phone/desktop URL: https://kcpwasm--johnandersen777-bsky-social.fedproxy.com
  ("no route configured for host", a 404, from fedproxy = the tunnel is down).
- Production is static files; the `/debug/*` hooks exist only on this dev
  server. Profiles turn on only when `HEAD /debug/control` answers 204.
  `scripts/deploy.sh` builds `public/` from `bin/wasm`, writes the OAuth
  client metadata for https://shell.socialweb.computer and ships it to the
  socialweb.computer host (`root@socialweb.computer -p 1997`, Caddy site
  `/etc/caddy/sites/shell.socialweb.computer.caddy`). Another origin is
  another OAuth client and browser storage: sign in again there.
- iOS gets low-memory mode automatically (no snapshot, lazy kubectl, GOGC 25,
  watch cache off). iOS kills the tab near ~630 MiB of wasm memory.

## Debug log and remote control of a page

- Every page served by the dev server streams its console, worker logs, Go
  logs (`kcp: go:` lines), terminal output (`term`) and command results to
  `~/.tmp/claude-kcpwasm/debug.log`, one line each:
  `<time> [<session> <ua>] <level> <message>`. The session id is per tab
  (sessionStorage); the iPhone tab was `l5ti1s`.
- Commands: `scripts/browser-dev/phone.sh <session> '<js body using d>' [timeout]`
  writes one command to `control.ndjson`; the page runs it (async function
  body, debug handle `d`) and logs `result <id> ok|error ...`.
- `d`: `run(line)` (terminal line + Enter), `type(text)`, `screen()`,
  `text(sel)`, `click(sel)`, `set(sel, value)`, `state()` (status feed),
  `rest(method, path, body, contentType)` (kcp REST), `call(fn, payload)`,
  `release(name)`, `reload()`, `reset()` (stop kcp, clear IndexedDB/OPFS/
  localStorage except debug switches, reload: a fresh kcp, needs sign-in).
- Reload or reset the phone yourself; do not ask the user. After sending
  `d.reload()`/`d.reset()` remove that line from `control.ndjson`
  (`grep -v 'd.reload()' ... > tmp && cat tmp > control.ndjson`) so a replay
  cannot loop (the server replays the last 2 minutes on reconnect).
- Outages (`scripts/browser/debug_link.js`): while the dev server or the
  tunnel is unreachable (fedproxy's "no route" is a 404; only a 404/405 before
  the first delivery means a static host) the page keeps up to 2000 log lines
  (oldest dropped), retries with backoff up to 30 s and logs
  `debug log reconnected after <s> s; <n> lines dropped`. The control stream
  reopens with backoff after errors, an open stalled 30 s, or 45 s without the
  server's 15 s `ping` event, and right after the log reconnects.
- localStorage switches (set with `d`, take effect on reload):
  `kcp.heapProfile=<s>` (Go heap+goroutine profiles uploaded to
  `~/.tmp/claude-kcpwasm/{heap,goroutine}-<session>-<t>.pb.gz`; adds memory, iOS
  may kill the tab), `kcp.gogcReady=<n>`, `socialweb.commsMode=Firehose|Both`
  (also the Compute panel's Comms select), `kcp.debugGo=0` (Go logs off).
- Memory/churn: with debugGo on, Go logs every 60 s a `memstats:` line
  (heap, totalAlloc, numGC, gcCPU, goroutines) and an `updates (written/noop
  per resource)` line. Allocation per minute = delta of totalAlloc.
- Profiles: `GOTOOLCHAIN=go1.26.9 go tool pprof -sample_index=alloc_space -base <older> -top -cum <newer>`.

## Running the VM test

```sh
D=~/.tmp/claude-kcpwasm
cd ~/src/kcp/kcp-wasm && deno run --allow-read=$D --allow-write=$D,$PWD/demos scripts/browser-demo.ts \
  --log=$D/debug.log --control=$D/control.ndjson \
  --out=demos --session=<id>              # or --ua=iPhone / --ua=X11 to wait for the next page load
```

Headless (no phone): start `browser-demo.ts --ua=X11` first, then
`deno run -A $D/page-hold.ts ~/.aliceoa_bsky_social_source <page URL>`: a
puppeteer-core headless Chrome (`/usr/bin/google-chrome --no-sandbox`) that
loads the page, signs in alice with the two-tap flow (first tap on
`#identity-signin` prepares the link, the second, "Continue sign-in", opens
the PDS popup; it fills the handle and accepts consent) and holds the tab
until `$D/page-hold.stop` exists. Its UA tag in the debug log is `X11`.
`$D/boot-smoke.ts [url] [secs]` loads a page and reports `kcp ready in` and
the first memstats line.

It waits for kcp and a signed-in MarketIdentity, releases leftovers, taps
Request VM (the tab's EphemeralInfra gets a contract; its ComputeContract is
`einfra-<infra>-vmshell-...`), waits for Held, runs `ssh <vm>` over WebRTC,
types `PS1="$ "`, `ip a`, `uname -a`, exits, releases and polls kubectl until
Released. A failed run releases its VM. Output: `demos/<t>-<session>.{timeline.txt,term.txt,log}`.
Start it after the page connected (`--session`) or before a reload (`--ua`);
starting it a second after a reload misses the connect line.

Release real VMs after every test. Do not create contracts against the bidders
without a reason.

## Sign-in (OAuth)

- The page is a public atproto OAuth client: client_id
  `<origin>/oauth-client-metadata.json`, redirect `<origin>/oauth/callback/requester`;
  the tab is paused while on the PDS and finishes the sign-in on return.
- The account is `aliceoa.bsky.social` (did:plc:lpfuqerea3deuoyrn7ojser4) on
  bsky.social. The user stated its credential is in
  `~/.aliceoa_bsky_social_source`; never print, log or commit it.
- After `d.reset()` the identity shows "names neither pdsUrl nor a resolvable
  did" until Sign in is tapped (enter the handle first, or the default
  bsky.social server is used).
- Refresh tokens are single use; the page refreshes them in
  `oauth_refresh_worker.js` (socialweb oauthclient TokenRefresher seam), which
  stores each rotated set in IndexedDB before answering. `SessionExpired /
  invalid_grant` means sign in again.
- A new scope in socialweb (`requester-scope.txt`) needs a fresh sign-in.

## The lead (and other agents)

The peer Mac is gone. The lead runs everything on its Fedora host (hat-8,
192.168.0.20, user johnandersen777): the dev server and tunnel, the bidders
(local = hat-8, plus hat-9, hat-7), the relay and the socialweb repo, and
decides socialweb design; the user directs it. Agents work in worktrees and
report back to the lead. Rules agreed with the user:
- Networking decisions (TURN, candidates, NAT, relays, which streams) are the
  user's: report observations, do not pick fixes.
- One owner per file area; coordinate conflicts through the lead.
- Agents commit in their worktree; the lead merges and pushes. Ask the lead
  before relying on an unreleased commit.
- Test peers never get a real shell; never type into a terminal you did not start.
- Use `~/.tmp` (set `TMPDIR`), never `/tmp`.

## Monitors worth keeping

- page milestones: `tail -n0 -F ~/.tmp/claude-kcpwasm/debug.log | command grep --line-buffered <session> | command grep --line-buffered -E "debug log (on|reconnected)|kcp ready in|MarketIdentity|ComputeContract .* (Held|Failed)|over WebRTC|uncaught"` (`command grep`: the tool grep ignores `--line-buffered`)
- per-minute allocation: memstats `totalAlloc` deltas (see above).
