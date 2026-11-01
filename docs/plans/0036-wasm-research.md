# Plan 0036, page/wasm side: requester association and policy eval in kcp.wasm

Read-only research by wasm-assoc. kcp-wasm worktree `atirc-agent` (HEAD 6af2d7ada);
socialweb at origin/main 702daf27 (local checkout 62fe79b7). socialweb paths are
relative to ~/src/kcp/socialweb-computer-kcp at origin/main.

## 1. Requester association on the page today: none

- `scripts/browser/app.js:2067` `requesterIdentity()` sends `mode` (from the
  select), `association: "Skip"` (`:2074`), the page-origin OAuth client id and
  redirect, plus handle or `pdsUrl=https://bsky.social`. `patchRequesterIdentity()`
  (`:2083`) merge-patches it into `marketrequesters/app`.
- `scripts/browser/app.html:360-362`: the only enabled mode is `oauthBrowser`;
  `localRepo` is disabled ("Local PDS (pending)"). `app.js:766` and `:2320`
  migrate a stored `localRepo` to `oauthBrowser`.
- In oauthBrowser the requester DID is the signed-in account (aliceoa), which
  is its own operator, so no association is needed or run.
- The wasm overlay copy of the check (same logic as upstream
  `internal/app/compose/identity.go`):
  - `socialweb-wasm/overlays/compose/identity.go:309` `OperatorWatched` sets
    `associateWith`/`associationWatch`/`associationService` only for localRepo
    and not Skip.
  - `:357` `ClaimOpen`, `:390` `associationRequired` (localRepo and not Skip and
    a watch/operator set), `:397` `AssociationWait`, `:413` `OperatorDID`
    (first `approvedAssociations`, else `status.associations`).
  - `socialweb-wasm/overlays/app/marketrequester.go:156` `PlanAccount(...,
    AssociationRequireApproval default)`, `:246` `RequesterSpec.OperatorDID`,
    `:489` `AssociationWait(plan, id, requester_associate)` gates Ready; the
    status texts the page shows are `:492-496` ("waiting for the requester's
    MarketIdentity to sign in" etc).
- `associateConfirm`: `internal/atproto/oauth/scope.go:47` `RPCAssociateConfirm`
  is only in `DIDKeyAssociatorOAuthScope` (`:104-108`), not in
  `RequesterOAuthScope`; the constant `AssociateConfirmNSID`
  (`internal/app/compose/identity.go:32`, overlay copy `:32`) has no caller in Go.
  The page never calls it and never talks to the did-key associator /
  qr.fedfork.com. Its XRPC is served by the old atproto-market associator
  (docs/plans/0023:177-183: verifies service auth, writes badgeBlueKeys in the
  requester repo).
- Existing mechanism (already record based, MarketIdentity side, runs in the
  wasm binary too since `marketidentity.Register` and the record-watch
  controllers are wired, `orchestrator.go:251-262`):
  `deploy/apps/repo-pds/association-watch.prod.yaml` AtprotoRecordWatch on
  `com.publicdomainrelay.temp.badgeBlueKeys` matching `keyId = subject did`,
  `service = requester_associate`; verified by
  `internal/marketidentity/atproto/association.go:76` `associationClaimDid`
  (`challenge` is a DID, `keyId == subject`, `record.Did == challenge`, i.e. the
  record lives in the operator repo), result into `status.associations` and
  condition `Associated`.

## 2. Policy eval in wasm

- `socialweb-wasm/internal/socialweb/orchestrator.go:315-320` (`Sandbox` at :316): engine factory
  with `Sandbox: worker.New()`, `DisableActions: true` (the PolicyEngine CR path
  resets it to false, `internal/policyengine/controller/manager.go:247`).
- `internal/sandbox/worker/worker.go:29` `Fenced()` is false, so
  `internal/policyengine/controller/engine.go:165` `Ready()` is false and
  `internal/policyengine/workflow/expression.go:289` refuses policy code. The
  bundled tangy actions (`internal/policyengine/bundled-actions/tangy/policy-*`,
  JS dist) never run in the page: any requester workflow verdict is Deny
  (commit e73c38b56 message says so).
- The requester links `Workflows = requester-tangled-vouch, requester-only-me,
  requester-mutuals, open` (`overlays/app/marketrequester.go:36`, PolicyRunLink
  `:146`) but the page sets no workflow, so
  `internal/market/requester/policy.go:30/:38` return Allow and
  `internal/market/requester/runner.go:481` skips Evaluate (`policyRef == nil`).
  `accept.go:346` Scope likewise allows.
- Inputs when a run does happen: `PolicyInput` (self/subject/counterparty DID,
  RFP and Bid strong refs, perspective, mode); the Go side seeds
  `cache["trust/<counterparty>"]` from `trust.BuildSnapshotFor`
  (`internal/policyengine/controller/run.go:277-288`, `trust.go:230`) before the
  sandbox runs.
- Where "record X in repo Y references requester Z" is checked in Go today,
  without OAuth, and which runs inside kcp.wasm over fetch:
  - `internal/policyengine/trust/trust.go:103` `ResolveOperatorDidFor`:
    listRecords badgeBlueKeys in the subject repo, `keyId == subject`,
    `challenge` = operator.
  - `internal/market/requester/discovery.go:65` `operatorsOf` and `:103`
    `operatedBy`: operator repo record plus the mirrored record in the subject
    repo (two-repo check). This is what finds the lead's bidders from the
    phone today, so public listRecords from the browser is proven.
- Conclusion: in wasm the association check has to be Go (trust/discovery/
  MarketIdentity), not a policy action, until a fenced sandbox exists in the page.

## 3. What changes on the page/wasm side for record-based association

- Build: discovery, trust, record watch and marketidentity code come from the
  socialweb mirror at REV; only `overlays/compose/identity.go` and
  `overlays/app/marketrequester.go` are wasm copies (3-way merged by
  build-page.sh). A new MarketIdentity/MarketRequester field needs the
  `socialweb-wasm/manifests` sync, else the page logs "field not declared in
  schema".
- Read path: verify by `com.atproto.repo.getRecord` of a pinned at-uri (optional
  cid) in the operator repo. One CORS GET (bsky PDS answers
  `Access-Control-Allow-Origin: *`), cached like trust.go's 5 min TTL. Avoid a
  streaming AtprotoRecordWatch on `app.bsky.feed.post`: iOS low-memory (tab dies
  near 630 MiB, 1-3 GB/min churn already open from no-op applies) and Firehose
  mode has no ingress anyway. If a watch is used, the EventSource must carry
  `wantedDids=<operator>` and `wantedCollections=<collection>`
  (`internal/events/jetstream/jetstream.go:144`).
- Config surface: identify by DID and at-uri (or collection + rkey), never a
  handle: the browser has no DNS TXT; DIDs resolve via plc.directory
  (`orchestrator.go:254`). Match rule for a bsky post: a mention facet with
  `did == requester DID` (or the DID string in text); for a custom record the
  existing `AtprotoRecordMatch{fieldPath, value}` shape fits.
- Writing the marker from the page: the page scope already has
  `repo:com.publicdomainrelay.temp.badgeBlueKeys?action=create&action=update&action=delete`
  (`scripts/serve-public.ts:53`, `RequesterOAuthScope`), so a signed-in operator
  tab can write the operator-side record for an agent DID today, no
  associateConfirm needed. A bsky post as marker needs
  `repo:app.bsky.feed.post?action=create` added to the requester scope
  (fresh sign-in, see AGENTS.md) or the operator posts from any bsky client and
  pastes the at-uri.
- Display: nothing shows association now (Skip). Minimal: show
  `status.associations` / the `Associated` condition of the MarketIdentity in
  the identity panel, and for an operator tab an "associate agent DID" action
  that writes the record and prints its at-uri.
- Open question for linux-kcp: is the non-OAuth agent always a CLI requester
  (VM, plan 0035), with the page only ever the operator side? If yes the page
  needs no new identity mode; if an agent should drive a page requester
  without OAuth, the page needs a non-OAuth mode (localRepo is still
  "pending" in wasm: overlays/atproto pds_store.go / repo_sqlite.go exist but
  the option is disabled).

## V1 result (2026-10-30, page-driven, production)

- Prod page https://shell.socialweb.computer 14e10250a (socialweb rc125 720ca71b), alice, Agent + "socialweb-computer in the VM (R2)" (guestTools + association.fromGuest): Held 17:10:18Z on hat-7; guest installed socialweb-computer from the bidder; agent aliceoa-3mz4bihylhh2s.
- R2 = `run request --association-ref` inside the VM as the agent user (root refused by devinstance.RefuseRoot; non-root workaround), own localRepo did:plc:4bcvjivkb6r24w7qta3lbgtu on its in-VM PDS behind xrpc.fedproxy.com.
- guest.requesterDid 17:15:49Z; page row "R2: ... Pending" 17:15:51Z -> "Verified" 17:15:52Z (one getRecord of R2's associationRequest through xrpc.fedproxy.com).
- R2's own RFP 3mz4btoodgi22: hat-7 bidder-tangled-vouch allowed R2 via alice (bid 17:16:09Z, accept 17:16:16Z), nested VM fc-4bcvjivkb6r2-10005e5b Running 17:16:19Z; R2 joined atirc as r2-xjw6w5wbmgcdh ("r2: online"). PASS.
- Not covered: R2's own agents (grandchildren, atircParentRef unused in the workaround); CLI path blocked by the run request option parsing bug (fixed on r2bin-fix 853e2adf, 8294bacd).
- Release: outer contract Released 17:17:31.9Z from the page; contract-scoped association requester_associate-9c02c8bc668311f157676fe1dce9c79f-xjw6w5wbmgcdh in alice's repo -> getRecord 400 RecordNotFound (deleted); hat-7 orphan check 17:17:44Z clean. R2's own nested contract Released 17:16:31.8Z (releaseOk).
- Revoke: atirc revocations.ndjson revoked_at 2026-10-30T17:17:30Z for r2-xjw6w5wbmgcdh.aliceoa.bsky.social and aliceoa-3mz4bihylhh2s.aliceoa.bsky.social (atirc-dev, server-side). V1 PASS incl cleanup.

## V1 grandchild run (2026-10-30, rc125 workaround)

- Prod page 4cba14c3a, alice, Agent + R2: outer Held 17:23:29Z on hat-7; VM agent aliceoa-3mz4ca3etg627 joined.
- R2 started as the agent user with --agent --hold: did:plc:reephqeca6eeq7hohvikcmot Pending 17:26:15Z -> Verified 17:26:16Z; child token r2-regb26j7a4jna.aliceoa.bsky.social minted 17:26:17Z; R2 joined and stayed subscribed 17:26:28Z ("r2: online (agent run)").
- R2's own RFP accepted by hat-7 17:26:42Z (repo did:plc:reephqeca6eeq7hohvikcmot); nested VM Held 17:26:59Z; `run request agent r2-agent` -> agentStart -> grandchild agent joined atirc as vm-3mz4cgbnb6z22.r2-regb26j7a4jna.aliceoa.bsky.social (token minted below R2's via atircParentRef). PASS: alice -> R2 -> R2 VM agent.
