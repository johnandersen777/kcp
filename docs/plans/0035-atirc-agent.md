# Per-VM atirc agent: opt-in cloud-init module, long token in the secrets bundle, `events.guest.agentStart` trigger, page + CLI (rev 6)

## Context

The page (`/Users/johnandersen777/src/kcp/wasm`, branch `kcp-wasm-memory-storage-1`) signs alice in with atproto OAuth, requests a VM from the socialweb market, and gives it SSH-over-WebRTC plus a wootty web shell. We add an atirc agent per VM: a **long** atirc token minted when the VM request creates its RFP, named `<handle-nick>-<rfp-rkey>` (e.g. `aliceoa-3l…`), delivered through the existing requester-vault secrets bundle as `ATIRC_TOKEN`, and a guest systemd unit that runs the atirc README's root/new-account block and `atirc agent run`.

Rev 4 change (2026-10-29, user): the rev 3 trigger (an Agent button that ran a script over `ssh` over WebRTC, in tmux) invented a new pattern. Rev 4 follows the established wootty shell pattern (socialweb plan 0032, `internal/atproto/cloudinit/tunnels.go`) end to end:

| wootty (plan 0032) | atirc agent (this plan) |
| --- | --- |
| opt-in per contract: `spec.tunnels[].shell {envFile}` | opt-in per contract: `spec.agent {envFile}` |
| requester `contractModules` adds `cloudinit.TunnelsModule` | requester `contractModules` adds `cloudinit.AgentModule` |
| `wootty-<tag>.service` + `/usr/local/bin/wootty-run.sh`, started by `runcmd` `systemctl start --no-block`, `Restart=always`, `RestartSec=5`, `After=… SecretsReadyUnits` | `atirc-agent.service` + `/usr/local/bin/atirc-agent-run.sh`, same unit shape, **not** started at boot |
| requester→guest events (`guest.signalWebRTC`, `guest.secrets`): outbound ContractEvent → requester publishes → bidder checks (`CheckGuestEvent`, `IsRequesterGuestEventNSID`) and queues → guest agent `receive` | Agent button → outbound ContractEvent `guest.agentStart` → same path → guest agent runs `systemctl start --no-block atirc-agent.service` |
| run script waits for the secrets ready file and the binary, refuses a missing token | same |
| binary from `socialweb-guest-downloads` via `setup-guest-binaries.service`; `guestseed.GuestBinariesNeeded` detects the unit path | same (`atirc-<arch>`; detection on `atirc-agent.service`) |
| token made by the page, only in the secrets bundle | token minted by the requester, written into the same bundle |

No ssh, no tmux, no rootfs bake. One new guest event type (`events.guest.agentStart`, user decision 2026-10-29, Option B: no harness spend until the user clicks).

User decisions (2026-10-29):
- atirc deployment `https://irc.fedfork.com` (DID `did:web:irc.fedfork.com`, service `#atirc`); live `/health` shows an iroh ticket, `/.well-known/did.json` matches.
- Add CORS to atirc, mirroring bsky.social PDS (verified: preflight 2xx, `access-control-allow-origin: *`, `access-control-allow-methods: GET,HEAD,PUT,PATCH,POST,DELETE`, `access-control-allow-headers: authorization,content-type,dpop`; actual responses ACAO `*`).
- Rev 4: trigger through user_data, not ssh.
- Rev 5: Option B — the unit is installed by user_data but started only by a requester-originated `events.guest.agentStart` from the page's Agent button.

Repos and ownership:
- this repo — page JS, tests, wasm glue.
- `socialweb-computer-kcp` — **linux-kcp** (peer agent; reached over atirc, nick contains `linux-kcp`) edits and re-deploys; we send requests and review. A change of this size gets a socialweb plan doc (`docs/plans/0035-…yaml`, the 0032 format), owned by linux-kcp.
- `atirc` — owned by **atirc-dev** (peer agent on the same atirc channel): CORS, irc.fedfork.com redeploy, versioned linux/amd64 binaries. linux/arm64 is out of scope for now (user).
- Comms: atirc default channel `#johnandersen777_bsky_social`, we post as `mac-kcp`.
- aliceoa's agent harness on irc.fedfork.com is configured and working (user, 2026-10-29).

## Design

### 1. atirc CORS (atirc repo; rebuild/redeploy irc.fedfork.com)

Unchanged from rev 3, still needed: the page's requester (Go in wasm, browser fetch) POSTs `chat.atirc.mint` cross-origin.

- `cli/Cargo.toml`: add `tower-http = { version = "0.6", features = ["cors"] }` (0.6.11 already in `Cargo.lock`; axum 0.8).
- `cli/src/server/xrpc.rs` `router()`: put `/xrpc/*` in a sub-router and layer **only that sub-router** (a whole-router layer would expose `/u/{uuid}`, which returns a long token, and `/dl/*` cross-origin):
  - `allow_origin(Any)`, `allow_methods([GET, HEAD, PUT, PATCH, POST, DELETE])`,
  - `allow_headers([header::AUTHORIZATION, header::CONTENT_TYPE, HeaderName::from_static("dpop")])` — a fixed list; `Any` sends `*`, which Fetch does not apply to `Authorization`.
  - tower-http answers preflight with 200 (not 204); Vary differs from bsky (harmless).
- Redeploy preserves the **entire atirc data dir** (`tokens/key`, `iroh/key`, `oauth/key`, `agents/<did>/config.toml`, `sessions/`, `links/`, `history/`). The scratch Dockerfile sets no HOME, so set `ATIRC_DATA_DIR` explicitly and keep `service_endpoint`. The deployment must include `138d03b` (persisted long-token key); tokens minted before it die on restart. Confirm the deployed commit/path with the owner.

### 2. OAuth scope (socialweb; request linux-kcp)

`internal/atproto/oauth/scope.go`: add to `RequesterOAuthScope`:

```
rpc:chat.atirc.mint?aud=*
```

Decided (linux-kcp + atirc-dev, 2026-10-30, atirc seq 587-588): exact aud, `rpc:chat.atirc.mint?aud=did:web:irc.fedfork.com%23atirc`; the requester calls getServiceAuth with aud `did:web:irc.fedfork.com#atirc` (same string; atirc verify accepts it and the bare DID). drives-oauth proves it after deploy; fall back to `aud=*` only if the PDS refuses. The build regenerates `requester-scope.txt`; existing sessions become `ScopeChanged` → fresh sign-in. Do not touch `internal/app/compose/atprotoscope.go`.

### 3. API: `spec.agent` (socialweb; request linux-kcp)

`api/v1alpha1/types_computecontract.go`, next to `ComputeContractShell`:

```go
// spec.agent: run `atirc agent run` on the guest with ATIRC_TOKEN from the secrets bundle.
Agent *ComputeContractAgent `json:"agent,omitempty"`

type ComputeContractAgent struct {
	EnvFile string `json:"envFile,omitempty" apigen:"userEndpoint,default=/etc/atirc/atirc.env"`
}
```

- Validation like the shell's: `spec.agent` without `spec.secretsRef` is invalid (the token comes only in the bundle).
- Regenerate deepcopy/CRD/schemas; the page build syncs `socialweb-wasm/manifests` (else `field not declared in schema`).
- The page sets the contract through its EphemeralInfra template (plan 0029): verify the infra template schema/lexicon carries `agent` the way it carries `tunnels`; add it there if not.

### 4. Mint at VM request and persist it to the secrets bundle (socialweb; request linux-kcp)

Files: `internal/market/requester/runner.go`, `identity.go` (reuse `q.oauthServiceAuth()`), `secrets.go`, `requester.go` (Config), `internal/services/requester/wiring.go` (vault write capability).

Delivery is via the vault: the sealed fallback re-reads the OpenBao bundle (`guestsecrets.go:119`) and is the wasm default (Firehose + `SecretsFallback: Sealed`); `secretsState.addEntries` (replaces by path) only feeds the inbound route; the seal is one-shot and the receipt-resume path never re-runs `requestOnce`. Writing the minted entry into the bundle makes both delivery modes and reloads see it.

- Config default `AtircURL = "https://irc.fedfork.com"`; audience `did:web:irc.fedfork.com`.
- **Wiring change is mandatory:** `services/requester/wiring.go:200` resolves `vault.CapabilityRead`, and the read handle returns `ErrReadOnly` on Write (`controllers/vault/provider.go:262`). Resolve `vault.CapabilityWrite` (the bidder's pattern, `services/bidder/wiring.go:291`; the write handle also reads). The write grant exists for `<mount>/contracts/*` (`internal/app/marketrequester/marketrequester.go:183`), which covers the page's `contracts/<name>/secrets`. A write failure logs and is non-fatal.
- New helper `(q *Requester) ensureAtircToken(ctx, contract, rfpURI string)` — returns nothing; every failure logs and returns (an error would fail the contract via `Run`):
  1. Skip when `contract.Spec.Agent == nil` (**only opted-in contracts mint**: long tokens are not revocable), `contract.Spec.SecretsRef == nil`, `q.handle == ""` (did:key/local identities have no handle), or `q.oauthServiceAuth() == nil` (no `deps.ServiceAuth` wired) — log why. **No identity-mode gate**: `oauthServiceAuth()` (`identity.go:46`) only needs `deps.ServiceAuth`, so it serves the page's OAuth identity and the CLI's PDS-account identity alike (section 7a).
  2. Read the bundle (`q.readSecretsBundle`); find the entry whose path is `spec.agent.envFile`; return if it has an `ATIRC_TOKEN=` line with a **non-empty** value (line scan; the `ATIRC_TOKEN=\n` placeholder must not count).
  3. `rfpURI` empty → return.
  4. `rkey` = last path segment (a TID: `createSigned` → `CreateSignedRecord`); `base` = first dot-label of `q.handle`, lowercased, capped to 14 chars; `subhandle = base + "-" + rkey + "." + strings.ToLower(q.handle)`. atirc's `nick_from_subhandle` keeps the label (≤32 with its `bot-` prefix for a digit-leading label), so the nick is ≤32 chars, valid for `useradd`.
  5. `short = q.oauthServiceAuth().ServiceAuth(ctx, "did:web:irc.fedfork.com", "chat.atirc.mint")`. atirc rejects replay; do not retry with the same token.
  6. `POST <AtircURL>/xrpc/chat.atirc.mint`, `Authorization: Bearer <short>`, JSON `{"subhandle": …}`, on `q.deps.HTTP` with a bounded timeout; parse `token`.
  7. Merge by path (`envFile`, value `ATIRC_TOKEN=<token>\n`, mode `0600`), `kv.Write(ctx, ref.Mount, ref.Path, map[string]any{"bundle": text})` (Write replaces the data map; `bundle` is its only key). If a live runner exists (`q.runnerFor`), also `addEntries` after nil-checking its `secretsState` (not restored on reload).
  8. Log `requester_atirc_minted` (never the token) or `requester_atirc_mint_failed` (placeholder stays).
- Call sites:
  - `requestOnce`: after the RFP if/else **and after** the `writeStatus` "the RFP is submitted" line (`runner.go:393`), with the `rfpURI` declared at :367 (the resume branch fills it at :369).
  - `Run`/resume: after the `SecretsRef != nil && SecretsFetchedAt == ""` block (`runner.go:200-205`); nil-guard `contract.Status.Contract` and pass its `RFPURI` when present.
- Timing: the mint runs right after the RFP, the guest boots after accept (40-72 s to Held), so the bundle normally carries the token before the guest asks. Race: the page dies between RFP and the vault write and a restored runner writes after the guest's sealed request was answered → the guest has the placeholder; `atirc-agent.service` keeps restarting with a clear journal line until a reboot re-requests secrets. Note it.

### 5. Guest module `atirc-agent.service` (socialweb; request linux-kcp)

Files: `internal/atproto/cloudinit/agent.go` (new, beside `tunnels.go`), `internal/market/requester/secrets.go` `contractModules` (:426), `internal/guestseed/binaries.go`.

- `cloudinit.AgentModule(AgentOptions{EnvFile})` (a constructor like `TunnelsModule`), added in `contractModules` when `contract.Spec.Agent != nil`.
- Writes, in the wootty shape (`tunnels.go:219-240`, `unitFile`):
  - `/etc/systemd/system/atirc-agent.service`:
    - `[Unit]` `Description=atirc agent`, `After=network-online.target setup-guest-binaries.service <SecretsReadyUnits>`, `Wants=network-online.target setup-guest-binaries.service <GuestSecretsUnit>`, `StartLimitIntervalSec=0`
    - `[Service]` `Type=simple`, `EnvironmentFile=-<envFile>`, `ExecStart=/usr/local/bin/atirc-agent-run.sh`, `Restart=always`, `RestartSec=5` (wootty uses 5; consider 30 since a missing token does not heal quickly), journal output.
  - `/usr/local/bin/atirc-agent-run.sh` (root:root 0700), a Go raw string like `woottyRunScript`:
    ```bash
    #!/usr/bin/env bash
    set -euo pipefail
    if [ ! -f "/run/socialweb-guest-agent/secrets.ready" ] && [ ! -f "<SecretsDoneStamp>" ]; then
      echo "atirc-agent: the secrets bundle is not written yet; the agent waits" >&2; exit 1
    fi
    if [ ! -x /usr/local/bin/atirc ]; then
      echo "atirc-agent: /usr/local/bin/atirc is not installed yet (setup-guest-binaries.service); the agent waits" >&2; exit 1
    fi
    if [ -z "${ATIRC_TOKEN:-}" ]; then
      echo "atirc-agent: ATIRC_TOKEN from the secrets bundle is empty (mint failed or pending); the agent waits" >&2; exit 1
    fi
    export ATIRC_TOKEN
    nick="$(/usr/local/bin/atirc nick)"
    getent group wheel >/dev/null || groupadd wheel
    mkdir -p /etc/sudoers.d
    printf '%%wheel ALL=(ALL) NOPASSWD: ALL\n' > /etc/sudoers.d/wheel
    chmod 440 /etc/sudoers.d/wheel
    id "$nick" >/dev/null 2>&1 || useradd -m -G wheel -s /bin/bash "$nick"
    exec runuser -u "$nick" -- /usr/local/bin/atirc agent run
    ```
    Use the package constants (`GuestSecretsReadyPath`, `SecretsDoneStamp`, `GuestBinariesUnit`) as `woottyRunScript` does. `runuser -u` cannot take `--login`, and `-w/--whitelist-environment` only applies to `--login`. Without login, `runuser -u` keeps the environment (and sets HOME/USER/LOGNAME/SHELL for the target), so `ATIRC_TOKEN` passes through with no `-w`. (The atirc README's `runuser -w ATIRC_TOKEN -u …` has the same no-op flag; tell the atirc owner.) `atirc agent run` sets its own working directory (a 0700 temp dir) and runs the harness as `<harness> -p <prompt>`; no TTY or tmux needed. A user who wants to watch reads `journalctl -u atirc-agent` or the atirc channel.
  - `/etc/socialweb-guest-agent/agent.json` (`cloudinit.GuestAgentAgentPath = GuestAgentConfigDir + "/agent.json"`, root:root 0644), content `{"unit": "atirc-agent.service"}` — the same per-feature config file pattern as `tunnels.json`/`secrets.json`; it tells the guest agent it may start that unit and makes the bidder install the guest agent.
  - `RuncmdPrepend`: `systemctl daemon-reload` only. **No** `systemctl start` and no `enable`: the unit starts only on `guest.agentStart` (section 5a).
- `guestseed` (`binaries.go:110` `GuestBinariesNeeded`): add `GuestBinaryAtirc = "atirc"` and `GuestBinaryNeeds.Atirc`; a write_files path equal to `cloudinit.GuestAgentAgentPath` sets `Agent = true` (installs and starts `socialweb-guest-agent.service`, as `tunnels.json`/`secrets.json` do) and `Atirc = true`, and appends `atirc-agent.service` to `out.Units`; `Names()`/`Any()` include atirc. `out.Units` only feeds `Before=` of `setup-guest-binaries.service` (`binaries.go:~230`), so listing the unit orders the install before it and does not start it.

### 5a. `events.guest.agentStart` (socialweb; request linux-kcp)

A requester-originated guest event, wired exactly like `guest.secrets`/`guest.signalWebRTC`:

- Lexicon `lexicons/com/publicdomainrelay/temp/compute/events/guest/agentStart.json`, modeled on `secretsRequested.json`: record, `key: any`, required `createdAt`, optional `signatures`; description "The requester asks the guest agent to start the guest's agent unit (spec.agent); requester-originated only; carries no secret." Regenerate `internal/atproto/market/zz_generated.lexicons.go` (`ComputeEventsGuestAgentStartNSID`, `ParseComputeEventsGuestAgentStart`, the record type).
- `internal/atproto/market/guest.go:42` `IsRequesterGuestEventNSID`: add it (not to `IsGuestOriginatedEventNSID`), so the bidder refuses it from a guest (`guestidentity/service.go:329`) and accepts it from the requester (`market/bidder/guestevents.go:82`).
- `internal/guestidentity/guestrecord.go:388` `CheckGuestEvent`: a case that parses it and checks `createdAt` (as `secretsRequested`).
- `api/v1alpha1/types_contractevent.go:45`: add the NSID to the `ContractEventSpec.NSID` enum; regenerate the CRD and sync the wasm manifests.
- Requester (`internal/market/requester/contractevents.go`): `validateGuestFields` (:61) accepts it with no kind/tag/key/ciphertext; the encode switch (:357) builds `market.ComputeEventsGuestAgentStart{CreatedAt: q.nowISO()}`; `observeOutboundContractEvent` (:221) refuses it (status message, no publish) when the owning contract has no `spec.agent`.
- Guest agent:
  - `internal/guestagent/wire.go`: `AgentStartNSID = nsidPrefix + "events.guest.agentStart"`.
  - `config.go` `LoadConfig`: read optional `agent.json` (`AgentFile`), `{unit}`; the unit must match `^[a-z0-9][a-z0-9-]*\.service$` or `ErrConfig`.
  - `subscribe.go:168` `receive`: `case record.Type == AgentStartNSID:` → with no configured unit, log debug and ignore; else run `systemctl start --no-block <unit>` (`exec.CommandContext`, bounded timeout) and log `guest_agent_start` with the result (never the env file). Idempotent: `systemctl start` on a running unit is a no-op, `a.seen` and the persisted cursor stop replays across agent restarts.
- No guest reply event: the page reads the ContractEvent's own phase (`Published`/`Submitted`) and the nick appears on the atirc channel. A `guest.agentStarted` ack (like `secretsApplied`) is an addition for later if the page must show unit state.
- AGENTS.md: list `guest.agentStart` in the "Guest events live under …" bullet with the requester-originated ones.

### 6. atirc in `socialweb-guest-downloads` (socialweb; request linux-kcp)

Replaces rev 3's rootfs bake and tmux preinstall (neither is needed). The bidder installs the binary; the guest never fetches it from irc.fedfork.com, GitHub or the internet (AGENTS.md "Guest transports").

- Pin (atirc-dev, 2026-10-30, atirc seq 585): `https://irc.fedfork.com/dl/v0.1.0-9459165`, sha256 `9eab39c20ec84d0df9f45b085fcb77eebd65f341b115a5f5db6ef2b595e67491`, 39379048 bytes (identical to the live server binary).
- `internal/versions/versions.json`: `guestDownloads.files.atirc.fetch["linux/amd64"]` `{url, sha256}` of an **immutable, versioned** atirc linux binary (not `irc.fedfork.com/dl/{arch}`, which serves whatever binary is running). The atirc owner publishes versioned release binaries; the pin follows that, as wootty's does.
- `scripts/build-guest-downloads.sh`: add an `atirc-x86_64` block next to `wootty-<arch>` (`fetch_pinned`/sha256 check). linux/arm64 is out of scope (user): pin only `linux/amd64` and skip `atirc-aarch64` with an explicit guard/log line; an aarch64 bidder then finds no atirc download and `setup-guest-binaries` fails for `spec.agent` contracts there (document amd64-only).
- Re-release `socialweb-guest-downloads`, update the bidder's pin, reinstall bidders (user step, as for any guest-downloads change).

### 7. This repo: page

Files: `scripts/browser/vmshell.js`, `scripts/browser/app.js`, `scripts/browser/vmshell_test.js`.

- Opt-in control in the Compute panel next to the Comms select: an "Agent" checkbox, stored in localStorage (`socialweb.compute.agent`), default **on** (user, 2026-10-29). Show it only when the served schema has `spec.agent` (as `woottyServed()` gates wootty). Next to it, a link to `https://irc.fedfork.com` ("set up your agent") where the user configures the harness the Agent button runs.
- `vmshell.js`:
  - `ATIRC_ENV = "/etc/atirc/atirc.env"`.
  - `secretsBundle({ woottyToken, agent })` stays a JSON string (`ContractSecrets.Bundle` is a string): `[...(woottyToken ? [{path: WOOTTY_ENV, value: "WOOTTY_AUTH_TOKEN=…\n"}] : []), ...(agent ? [{path: ATIRC_ENV, value: "ATIRC_TOKEN=\n", mode: "0600"}] : [])]`. The placeholder keeps the bundle non-empty (both consumers reject an empty bundle) and gives the requester the path to fill.
  - `contractManifest`/`infraContract({ …, agent })`: set `spec.agent = {}` and `spec.secretsRef` when `agent || wootty`; return the `secret` entry in that case.
- `app.js` `requestVM`: the vault put runs when `wootty || agent` (hoisted out of `if (wootty)`); wootty token generation stays conditional.
- `vmshell_test.js`: wootty-only (unchanged expectations), agent-only (one entry, path, mode `0600`, empty value, `secretsRef` set, `spec.agent` set), both (two entries), neither (no `secretsRef`, `secret === null`).
- Agent button (`app.js` `shellAction` :2698): shown when the contract is Held, has `spec.agent`, and `!computeReleaseAsked.has(contract.name)`. Click creates an outbound ContractEvent through the same helper the WebRTC signaling uses (`webrtc_guest.js` builds `{apiVersion: "socialweb.computer/v1alpha1", kind: "ContractEvent", metadata: {name}, spec}` at :115 and creates it through kcp): `name = <contract>-agent-<suffix>`, `spec = {contractRef: {name: contract}, nsid: NSID_AGENT_START}`. Put the builder (`agentStartEvent(contract, suffix)`) and `NSID_AGENT_START` in `vmshell.js` (tested) or next to the signaling builders in `webrtc_guest.js` — whichever already exports the ContractEvent builder; do not duplicate it. Log "asked <contract> to start the atirc agent"; disable the button while that event is not yet `Published` and show the event's phase/message (a requester refusal shows here).
- No ssh, no `agentScript`/`agentCommand`, no `runInTerminal` changes.

### 7a. CLI request flow (socialweb; request linux-kcp)

User requirement (2026-10-30, atirc seq 413): agents launch from `socialweb-computer run request` too, not only the page. Same requester code (sections 4-5a); the CLI adds only the opt-in and the start command, in the existing `run request` / `run request proxy` shapes.

- **Opt-in:** `run request --agent[=<envFile>]` (`cmd/socialweb-computer/request.go`, `requestOptions` :47, parsed next to `--tunnel` at :306) sets `contract.Agent = &ComputeContractAgent{EnvFile}`; add it to `deploy/apps/option-map.yaml` next to `secrets`.
- **Bundle:** `--agent` needs a secretsRef. At :1066-1143 the CLI reads `--secrets` and `PutSecrets` writes it to the invocation's own OpenBao at `contracts/<RequesterContract>/secrets` (`internal/stack/request/request.go:48`, inside the requester's `<mount>/contracts/*` write grant). With `--agent`, merge an `{path: <envFile>, value: "ATIRC_TOKEN=\n", mode: "0600"}` placeholder into that bundle (creating a one-entry bundle when `--secrets` is absent), so `SecretsRef` is set and `ensureAtircToken` fills it.
- **Identity:** the CLI's requester signs with its MarketIdentity (PDS account / app password, or OAuth); `deps.ServiceAuth` yields the `chat.atirc.mint` service-auth token either way (section 4 step 1 has no OAuth-only gate). A did:key/local identity has no handle → no mint, logged.
- **Start:** new `run request agent <name>` (main.go command map next to `"run request proxy"` :83; `openRequest(ctx, vals, rest, "agent")` like ssh/proxy at :1386/:1422) creates the outbound ContractEvent through `internal/stack/request/webrtc.go` `ContractSignaling.create` (:38, name prefix `cli-`): add `func (s *ContractSignaling) PublishAgentStart(ctx) error { return s.create(ctx, "agent", v1alpha1.ContractEventSpec{NSID: v1alpha1.ContractEventNSIDGuestAgentStart}) }` and the `ContractEventNSIDGuestAgentStart` constant beside `ContractEventNSIDGuestPublicKey`. It waits for the event to be `Published` (or prints the requester's refusal) and exits; `run request --agent --hold` users run it after Held.
- **Tests:** flag → `spec.agent`; bundle merge (with/without `--secrets`, placeholder path/mode); `run request agent` creates one outbound ContractEvent with the agentStart NSID for the invocation's contract.
- **OAuth logins for CLI tests:** the `linux-kcp-drives-oauth-*` agent drives OAuth sign-ins (user, alice, bob accounts, requester association) with the creds in `~/.*_source`. Existing mechanism, nothing new: `run request … -- --atproto-oauth-qr` sets the MarketIdentity's `status.pendingAction` (`scanQr`/`signInAgain`, `internal/marketidentity/atproto/oauth.go:292`) to `https://qr.fedfork.com/#oauth=<cliDID>&n=<nonce>` (`oauthclient/qr.go:182`) and logs it; drives-oauth opens that URL and signs in; the qr page transfers the session to the CLI (service-auth to cliDID), which logs `signed in as <handle>`. The CLI writes no file for it. Reference: `~/.tmp/0034/acc34.sh` `signin()`, socialweb `scripts/oauth-signin.ts`. Test harness hook (linux host): drives-oauth's `~/.tmp/drives-oauth/signin-watch.ts --log <request.log> --account ALICE|BOB [--pid <cli pid>]` tails the CLI log, signs in for each new `pendingAction` URL, exits on `signed in as`; a CLI test tees `run request` to a log and starts it (or asks drives-oauth on atirc with log path + account). (`--test-owner-sessions-file` is the app-password alternative and excludes the oauth modes, `requester.go:333`.)

### 8. Scope fallback + build/deploy (this repo)

- `scripts/serve-public.ts` `REQUESTER_SCOPE` (used by `scripts/kcp_wasm.ts` and as the missing-file fallback): regenerate from current `RequesterOAuthScope` (already missing several scopes) so it includes `rpc:chat.atirc.mint?aud=*`.
- `requester-scope.txt` lives in `bin/wasm` and `public/` (build output): regenerate via `build-page.sh` with `DEPLOY=1`, or pass `--scope-file`.
- Build/deploy: our page changes committed; linux-kcp's socialweb commit reachable in `SOCIALWEB_SRC`; `REV=<commit> DEPLOY=1 scripts/browser-dev/build-page.sh` with `MAIN_CHECKOUT=/Users/johnandersen777/src/kcp/wasm` if built here. Production: `scripts/deploy.sh` regenerates `/oauth-client-metadata.json` (owner step).

## Coordination with linux-kcp (atirc)

Over atirc with the peer whose nick contains `linux-kcp` (rev 3 named the old NDJSON channel 192.168.0.20:7720). One request:
1. `scope.go`: add `rpc:chat.atirc.mint?aud=*`.
2. `spec.agent` API (+ infra template carries it), `cloudinit.AgentModule` (+ `agent.json`), `contractModules` hook, `guestseed` atirc need/install.
2a. `events.guest.agentStart`: lexicon + generated code, `IsRequesterGuestEventNSID`, `CheckGuestEvent`, ContractEvent NSID enum, requester validate/encode/refuse-without-`spec.agent`, guest agent `agent.json` + `receive` case.
3. requester: `AtircURL` default; `ensureAtircToken` (only for `spec.agent`) with vault write-back; wiring resolves the vault write capability.
4. guest-downloads: atirc pin + build block; re-release; bidder pin.
4a. CLI (section 7a): `run request --agent[=envFile]`, bundle placeholder merge, `run request agent <name>`, `PublishAgentStart`, option-map entry.
5. A `docs/plans/0035-…yaml` for 2-4 and AGENTS.md lines (next to the wootty shell bullet, and `guest.agentStart` in the guest-events bullet).
6. Commit, report `REV`; we build/deploy the page with our committed changes.

To atirc-dev (same channel): CORS on the `/xrpc/*` sub-router only (section 1), redeploy irc.fedfork.com with `138d03b`+ and the data dir preserved, publish an immutable versioned linux/amd64 binary (URL + sha256) for the guest-downloads pin; confirm `chat.atirc.mint` accepts aud `did:web:irc.fedfork.com` from an OAuth-session service-auth token.

## Verification

1. `cd scripts/browser && deno test --allow-read --allow-net` (bundle/manifest cases above).
2. `deno test --allow-read --allow-write scripts/serve-public_test.ts` with a new assertion that `DEFAULT_SCOPE`/`loadScope` output contains `rpc:chat.atirc.mint?aud=*` (the current test compares `DEFAULT_SCOPE` to itself).
3. socialweb (linux-kcp): unit tests for `AgentModule` output (unit/run-script/`agent.json` paths, `After=` list, runcmd has no `start`), `GuestBinariesNeeded` (agent + atirc on `agent.json`), `contractModules` with/without `spec.agent`, the `spec.agent` without `secretsRef` refusal, `ensureAtircToken` skip/idempotence with a fake vault + fake atirc server (placeholder vs real token), `CheckGuestEvent` + direction (bidder refuses `agentStart` from a guest, accepts it from the requester), requester refusal without `spec.agent`, guest agent `receive` (starts the configured unit via a fake runner; ignores without `agent.json`; a replayed event starts nothing new).
4. atirc CORS after redeploy: OPTIONS `/xrpc/chat.atirc.mint` with `Origin`, `Access-Control-Request-Method: POST`, `Access-Control-Request-Headers: authorization,content-type,dpop` → 2xx, ACAO `*`, ACAH has all three; OPTIONS `/u/<x>` has no ACAO (scoping proof). `/health` (`iroh_ticket`), `/.well-known/did.json`.
5. Build with linux-kcp's `REV`; `bin/wasm/requester-scope.txt` and the live `/oauth-client-metadata.json` contain `chat.atirc.mint`. Fresh sign-in (scope change) — PAR scopes must be a subset of the client metadata `scope`, and bsky.social caches client metadata ~10 min, so wait ~10 min after the metadata deploy (else `invalid_scope … not declared in the client metadata`).
6. Request VM with Agent checked; debug log shows `requester_atirc_minted`. After Held, over the existing ssh (read only, our own terminal): `systemctl is-active atirc-agent` is `inactive`. Click Agent: the ContractEvent goes `Published`; then `systemctl is-active atirc-agent`, `journalctl -u atirc-agent -n 20`, `stat -c '%a %U' /etc/atirc/atirc.env`, `grep -c '^ATIRC_TOKEN=.' /etc/atirc/atirc.env` (never `cat`: terminal output streams to `debug.log`). The nick appears on the atirc channel.
7. Request VM with Agent unchecked: no mint log, no `atirc-agent.service`, no atirc binary installed.
7a. CLI: `socialweb-computer run request --agent --hold …` (identity signed in via the drives-oauth agent or an app password) → `requester_atirc_minted`; `run request agent <name>` → event `Published`, nick on the channel; `run request release <name>`.
8. Release the VMs.

## Decision (user, 2026-10-29)

Option B (on-demand start through `events.guest.agentStart`) over boot-start. Cost accepted: one new lexicon and ~6 typed touch points (section 5a).

## Risks / open items

- Harness: configured by the user (works). It survives an irc.fedfork.com redeploy only if the data dir is preserved (atirc-dev). The harness installs claude with `curl` at runtime: guest egress to claude.ai and the model endpoint is needed (observation for the user).
- iroh `endpoint`: tokens may carry an iroh ticket; `agent run` needs guest UDP to the n0 relay. Observation for the user.
- Long tokens are not revocable; VM release leaves them valid. Only opted-in contracts mint; the idempotent vault check prevents duplicate mints on reload.
- linux/arm64 out of scope → amd64 guests only.
- Agent on by default → every VM request mints a non-revocable token (user accepted).
- Scope change forces every page session to sign in again; production metadata must be regenerated first.
- Old VMs (before this feature) have no `spec.agent`; nothing changes for them (no button).
- The token is minted at request time even if Agent is never clicked: it must be in the bundle before the guest's one secrets fetch at boot, and a sealed bundle is never sent unasked. Opt-in (the checkbox) bounds it.
- The guest agent must be running for the event to land; `agent.json` guarantees it is installed even on an XRPC requester without webrtc/sealed secrets.
- A guest reboot leaves the unit stopped (not enabled) and the event is not replayed (persisted cursor); click Agent again.
