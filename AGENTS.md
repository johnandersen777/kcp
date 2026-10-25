# AGENTS.md: browser kcp + socialweb (iPhone) test handoff

This fork (`origin` https://github.com/johnandersen777/kcp, branch
`kcp-wasm-memory-storage`, tag `iphone-working-v0`) runs real kcp and the
socialweb control plane compiled to js/wasm in a browser tab. The page
requests a VM from the socialweb market (bidders run by the linux-kcp lead),
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
| `~/src/kcp/wasm` | main checkout, branch `kcp-wasm-memory-storage`; the dev server serves this tree (page JS from `scripts/browser/`, wasm from `bin/wasm/`) |
| `~/src/kcp/wasm/.claude/worktrees/integration` | worktree, branch `kcp-wasm-integration`: edit and build here, then fast-forward the main checkout |
| `~/src/linux-kcp/socialweb-computer-kcp` | socialweb source (the lead's; read only for us; `git -c safe.directory='*'`) |
| `scripts/browser/` | the page: `app.js`, `kcp_worker.js`, `oauth_refresh_worker.js`, `vmshell.js`, `webrtc_guest.js`, `ssh_cmd.js`, `identity_ui.js` |
| `socialweb-wasm/` | Go glue: `cmd/kcp-wasm` (wasm main, refresher bridge, memstats), `internal/socialweb/orchestrator.go` (MarketRequester `app`, comms mode, events.Register), `overlays/compose/compose.go` (Config.Rest) |
| `contrib/wasm/storage` | kcp's in-memory storage for wasm (per-resource update counters live here) |
| `scripts/prepare-kcp-patched-modules.sh` | patches k8s modules for wasm (incl. the JSON fast path for apply bodies) |
| `scripts/kubectl_wasm_web.ts` | dev server (static files + `/debug/*` hooks) |
| `scripts/browser-demo.ts` | drives a phone through the whole VM test and records it to `demos/` |
| `scripts/browser-dev/{build-page.sh,phone.sh}` | build/deploy and one-shot page commands |
| `contrib/wasm/FUTURE_WORK.md` | open items (build-time snapshot secrets, Firehose on a phone) |

## Build and deploy

```sh
cd ~/src/kcp/wasm/.claude/worktrees/integration
REV=<socialweb commit, e.g. main> DEPLOY=1 scripts/browser-dev/build-page.sh   # ~1 min incremental
git add ...; git commit                                                     # commit in the worktree
git -C ~/src/kcp/wasm merge --ff-only kcp-wasm-integration                   # page JS is live now
git -C ~/src/kcp/wasm push origin kcp-wasm-memory-storage
```

Notes:
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

```sh
mkdir -p /tmp/claude-kcpwasm
cd ~/src/kcp/wasm && deno run --allow-read --allow-net --allow-write=/tmp/claude-kcpwasm \
  scripts/kubectl_wasm_web.ts --port=8090 \
  --origin=https://kcpwasm--johnandersen777-bsky-social.fedproxy.com \
  --debug-log=/tmp/claude-kcpwasm/debug.log --debug-control=/tmp/claude-kcpwasm/control.ndjson \
  2>&1 | tee /tmp/claude-kcpwasm/server8090.log            # run in the background

while true; do ssh -NnT -p 2222 -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 \
  -o ServerAliveCountMax=3 -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no \
  -o PasswordAuthentication=no -i ~/.ssh/id_ed25519 \
  -R kcpwasm:80:127.0.0.1:8090 johnandersen777.bsky.social@fedproxy.com; sleep 3; done \
  2>&1 | tee -a /tmp/claude-kcpwasm/tunnel.log               # public tunnel, background
```

- Phone/desktop URL: https://kcpwasm--johnandersen777-bsky-social.fedproxy.com
  ("no route configured for host" from fedproxy = the tunnel is down).
- Production is static files; the `/debug/*` hooks exist only on this dev
  server. Profiles turn on only when `HEAD /debug/control` answers 204.
- iOS gets low-memory mode automatically (no snapshot, lazy kubectl, GOGC 25,
  watch cache off). iOS kills the tab near ~630 MiB of wasm memory.

## Debug log and remote control of a page

- Every page served by the dev server streams its console, worker logs, Go
  logs (`kcp: go:` lines), terminal output (`term`) and command results to
  `/tmp/claude-kcpwasm/debug.log`, one line each:
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
  `/tmp/claude-kcpwasm/{heap,goroutine}-<session>-<t>.pb.gz`; adds memory, iOS
  may kill the tab), `kcp.gogcReady=<n>`, `socialweb.commsMode=Firehose|Both`
  (also the Compute panel's Comms select), `kcp.debugGo=0` (Go logs off).
- Memory/churn: with debugGo on, Go logs every 60 s a `memstats:` line
  (heap, totalAlloc, numGC, gcCPU, goroutines) and an `updates (written/noop
  per resource)` line. Allocation per minute = delta of totalAlloc.
- Profiles: `GOTOOLCHAIN=go1.26.9 go tool pprof -sample_index=alloc_space -base <older> -top -cum <newer>`.

## Running the VM test

```sh
cd ~/src/kcp/wasm && deno run --allow-read=/tmp/claude-kcpwasm \
  --allow-write=/tmp/claude-kcpwasm,$PWD/demos scripts/browser-demo.ts \
  --log=/tmp/claude-kcpwasm/debug.log --control=/tmp/claude-kcpwasm/control.ndjson \
  --out=demos --session=l5ti1s            # or --ua=iPhone to wait for the next page load
```

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

## Talking to the linux-kcp lead (and other agents)

NDJSON over netcat. The lead runs the bidders (local = hat-8 192.168.0.20,
hat-9, hat-7), the relay and the socialweb repo, and decides socialweb design;
the user directs it.

```sh
# inbox: everything sent to this Mac on :7721 lands in /tmp/agent-7721.log
mkfifo /tmp/agent-7721.fifo 2>/dev/null; while true; do nc -l 7721 < /tmp/agent-7721.fifo | tee -a /tmp/agent-7721.log; done
# outbox to the lead (192.168.0.20:7720): a sender loop reads a FIFO
mkfifo /tmp/agent-7720.fifo 2>/dev/null; while true; do nc -v 192.168.0.20 7720 < /tmp/agent-7720.fifo | tee -a /tmp/agent-7720.log; sleep 2; done
# send one message
jq -cn --arg text "[kcp-wasm] ..." '{from:"kcp-wasm", ts:(now|todate), kind:"info", text:$text}' > /tmp/agent-7720.fifo
# read
tail -n 5 /tmp/agent-7721.log | jq -r '.ts + " " + .from + ": " + .text'
```

Watch the inbox with a monitor (`tail -n0 -F /tmp/agent-7721.log`). The lead
reaches this Mac by LAN IP (it moved .69 -> .94 once; tell the lead if it
changes). Rules agreed with the user:
- Networking decisions (TURN, candidates, NAT, relays, which streams) are the
  user's: report observations, do not pick fixes.
- Coordinate conflicts with the other agent directly; one owner per file area.
- Commit and push as you go; ask the lead before relying on an unreleased commit.
- Test peers never get a real shell; never type into a terminal you did not start.
- `ssh hat-8` (local bidder) works for read-only checks; it prints a host-key
  warning because the host dual-boots; that is expected.

## Monitors worth keeping

- lead inbox: `tail -n0 -F /tmp/agent-7721.log`
- phone milestones: `tail -n0 -F /tmp/claude-kcpwasm/debug.log | grep l5ti1s | grep -E "debug log on|kcp ready in|MarketIdentity|ComputeContract .* (Held|Failed)|over WebRTC|uncaught"`
- per-minute allocation: memstats `totalAlloc` deltas (see above).
