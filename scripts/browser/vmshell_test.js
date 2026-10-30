// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import {
  ORIGIN_INFRA_TTL_SECONDS,
  addContract,
  associationSubject,
  contractManifest,
  contractName,
  finishedInfraContracts,
  fnv1a64,
  infraContract,
  infraRecordBinding,
  infraRecordStale,
  r2StartShown,
  releaseNotes,
  schemaHasGuestTools,
  infraRecordURI,
  infraRepoRef,
  originInfraName,
  randomTabId,
  recordSyncError,
  recordSynced,
  repoName,
  sessionInfraManifest,
  isActive,
  isReleasing,
  orderContracts,
  randomToken,
  schemaHasAgent,
  schemaHasShell,
  secretsBundle,
  shellLink,
  RELEASE_FIELD_MANAGER,
  canReleaseAgain,
  phaseLabel,
  releaseAnswered,
  releaseApply,
  releasePending,
  releaseRequestedAt,
  requestRelease,
} from "./vmshell.js";

function assert(cond, message) {
  if (!cond) throw new Error(message);
}

Deno.test("token is 32 base64url chars", () => {
  assert(/^[A-Za-z0-9_-]{32}$/.test(randomToken()), "token");
});

Deno.test("contract name is a DNS label", () => {
  assert(contractName(new Uint8Array([0, 1, 35, 36, 255, 10])) === "vmshell-ab9adk", contractName(new Uint8Array([0, 1, 35, 36, 255, 10])));
  assert(/^vmshell-[a-z0-9]{6}$/.test(contractName()), "random name");
});

Deno.test("manifest without wootty carries only the webrtc ssh tunnel", () => {
  const m = contractManifest({
    name: "vmshell-abc123", ownerDid: "did:plc:me",
    sshKeys: ["ssh-ed25519 AAAA page"],
  });
  assert(m.spec.bidderDids === undefined, "no bidders listed: the requester discovers them");
  assert(JSON.stringify(m.spec.tunnels) === '[{"tag":"ssh","port":22,"transport":"webrtc"}]', "tunnels");
  assert(m.spec.secretsRef === undefined && m.spec.agent === undefined, "no secretsRef, no agent");
  assert(m.spec.sshAuthorizedKeys[0] === "ssh-ed25519 AAAA page" && m.spec.ttlSecondsAfterFinished === undefined, "keys, no ttl");
  assert(m.spec.ownerDid === "did:plc:me" && m.spec.userData === undefined, "owner, no userData");
  assert(m.spec.policy === "requester-tangled-vouch", "requester policy");
});

Deno.test("manifest with wootty carries both tunnels and the secretsRef", () => {
  const m = contractManifest({ name: "vmshell-both01", bidders: ["did:plc:x"], ownerDid: "", wootty: true });
  const tags = Object.fromEntries(m.spec.tunnels.map((t) => [t.tag, t]));
  assert(JSON.stringify(tags.ssh) === '{"tag":"ssh","port":22,"transport":"webrtc"}', "ssh");
  assert(JSON.stringify(tags.wootty) === '{"tag":"wootty","port":7681,"transport":"opentunnel","shell":{"envFile":"/etc/wootty/wootty.env"}}', "wootty");
  assert(m.spec.secretsRef.mount === "requester" && m.spec.secretsRef.path === "contracts/vmshell-both01/secrets", "secretsRef");
  assert(m.spec.agent === undefined, "no agent");
});

Deno.test("manifest with the agent carries spec.agent and the secretsRef", () => {
  const m = contractManifest({ name: "vmshell-agent1", agent: true });
  assert(JSON.stringify(m.spec.agent) === '{"envFile":"/etc/atirc/atirc.env"}', "agent");
  assert(m.spec.secretsRef.path === "contracts/vmshell-agent1/secrets", "secretsRef");
  assert(m.spec.tunnels.length === 1, "no wootty tunnel");
});

Deno.test("schemaHasAgent reads spec.properties.agent", () => {
  const schema = (props) => ({ spec: { versions: [{ schema: { properties: { spec: { properties: props } } } }] } });
  assert(schemaHasAgent(schema({ tunnels: {}, agent: {} })), "served");
  assert(!schemaHasAgent(schema({ tunnels: {} })), "not served");
  assert(!schemaHasAgent(null), "missing");
});

Deno.test("schemaHasShell reads tunnels.items.properties.shell", () => {
  const schema = (props) => ({ spec: { versions: [{ schema: { properties: { spec: { properties: { tunnels: { items: { properties: props } } } } } } }] } });
  assert(schemaHasShell(schema({ tag: {}, shell: {} })), "rc110");
  assert(!schemaHasShell(schema({ tag: {}, transport: {} })), "rc109");
  assert(!schemaHasShell(null), "missing");
});

Deno.test("bundle delivers the wootty env file", () => {
  const entries = JSON.parse(secretsBundle({ woottyToken: "tok" }));
  assert(entries.length === 1 && entries[0].path === "/etc/wootty/wootty.env", "path");
  assert(entries[0].value === "WOOTTY_AUTH_TOKEN=tok\n" && entries[0].mode === undefined, "value");
});

Deno.test("bundle with the agent holds an empty ATIRC_TOKEN for the requester to fill", () => {
  const agent = JSON.parse(secretsBundle({ agent: true }));
  assert(JSON.stringify(agent) === '[{"path":"/etc/atirc/atirc.env","value":"ATIRC_TOKEN=\\n","mode":"0600"}]', JSON.stringify(agent));
  const both = JSON.parse(secretsBundle({ woottyToken: "tok", agent: true }));
  assert(both.map((e) => e.path).join() === "/etc/wootty/wootty.env,/etc/atirc/atirc.env", "both");
  assert(JSON.parse(secretsBundle({})).length === 0, "neither");
});

Deno.test("shell link uses the wootty network and the token fragment", () => {
  const contract = {
    networks: [
      { tag: "ssh", kind: "webrtc", address: "sha-256 AB:CD" },
      { tag: "wootty", kind: "https", address: "https://0123456789abcdef.abcdefghijkl.opentunnel.xyz" },
    ],
  };
  assert(shellLink(contract, "a+b/c") === "https://0123456789abcdef.abcdefghijkl.opentunnel.xyz/#token=a%2Bb%2Fc", shellLink(contract, "a+b/c"));
  assert(shellLink({ networks: [] }, "t") === "", "no address yet");
  assert(shellLink({ networks: [{ tag: "wootty", address: "javascript:alert(1)" }] }, "t") === "", "non-https rejected");
  assert(shellLink(contract, "") === "", "no token");
});

Deno.test("isActive and isReleasing follow phase and release", () => {
  const cases = [
    [{ phase: "Held" }, true, false],
    [{ phase: "Requesting" }, true, false],
    [{ phase: "Held", release: true }, false, true],
    [{ phase: "Released", release: true }, false, false],
    [{ phase: "Failed" }, false, false],
  ];
  for (const [c, active, releasing] of cases) {
    if (isActive(c) !== active || isReleasing(c) !== releasing) throw new Error(JSON.stringify(c));
  }
});

Deno.test("orderContracts puts Released last and keeps the rest in feed order", () => {
  const names = orderContracts([
    { name: "a", phase: "Released" },
    { name: "b", phase: "Held" },
    { name: "c", phase: "Released" },
    { name: "d", phase: "Failed" },
    { name: "e", phase: "Pending" },
  ]).map((c) => c.name);
  assert(names.join() === "b,d,e,a,c", names.join());
});

Deno.test("infraContract: a Requested entry, its secrets entry and the child name", () => {
  const out = infraContract({ infra: "s1", short: "vmshell-abc", wootty: true, sshKeys: ["ssh-ed25519 K"] });
  assert(out.cc === "einfra-s1-vmshell-abc", "cc name");
  assert(out.contract.name === "vmshell-abc" && out.contract.state === "Requested", "entry");
  assert(out.contract.vmName === "einfra-s1-vmshell-abc" && out.contract.sshAuthorizedKeys[0] === "ssh-ed25519 K", "template");
  assert(out.contract.policy === "requester-tangled-vouch", "policy in the template");
  assert(out.contract.tunnels.length === 2 && out.contract.secretsRef === undefined, "tunnels, no secretsRef in the template");
  assert(out.secret.contract === "vmshell-abc" && out.secret.secretsRef.path.includes("einfra-s1-vmshell-abc"), "secrets");
  const none = infraContract({ infra: "s1", short: "x" });
  assert(none.secret === null && none.contract.agent === undefined, "no wootty or agent, no secrets entry");
  const agent = infraContract({ infra: "s1", short: "y", agent: true });
  assert(agent.contract.agent?.envFile === "/etc/atirc/atirc.env" && agent.contract.secretsRef === undefined, "agent template");
  assert(agent.secret.secretsRef.path === "contracts/einfra-s1-y/secrets" && agent.contract.tunnels.length === 1, "agent secrets");
  const infra = sessionInfraManifest("s1");
  assert(infra.kind === "EphemeralInfra" && infra.spec.endWhen === "Deadline" && infra.spec.contracts.length === 0, "infra");
});

Deno.test("fnv1a64 matches the known FNV-1a vector", () => {
  assert(fnv1a64("a") === "af63dc4c8601ec8c", fnv1a64("a"));
});

Deno.test("origin infra name is stable, per-tab and DNS/atproto safe", () => {
  const origin = "https://kcpwasm--johnandersen777-bsky-social.fedproxy.com";
  const name = originInfraName(origin, "abc123");
  assert(name === originInfraName(origin, "abc123"), "deterministic");
  assert(/^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(name), name);
  assert(name.length <= 40, name);
  assert(originInfraName("http://127.0.0.1:8090", "abc123") !== originInfraName("https://127.0.0.1:8090", "abc123"), "scheme");
  assert(originInfraName("https://x", "abc123") !== originInfraName("https://x", "def456"), "tab");
  assert(originInfraName("", "") === originInfraName("", ""), "empty fallback");
  assert(/^[a-z0-9]{6}$/.test(randomTabId(new Uint8Array([0, 1, 35, 36, 255, 10]))), "tab id");
});

Deno.test("repoName mirrors the Go AtprotoRepo name", () => {
  assert(repoName("did:plc:lpfuqerea3deuoyrn7ojser4") === "repo-did-plc-lpfuqerea3deuoyrn7ojser4", repoName("did:plc:lpfuqerea3deuoyrn7ojser4"));
  assert(repoName("DID:WEB:Example.COM:User") === "repo-did-web-example.com-user", repoName("DID:WEB:Example.COM:User"));
  assert(repoName("did:plc:" + "x".repeat(300)).length === 253, "cap");
  assert(infraRepoRef("did:plc:me").name === "repo-did-plc-me", "ref");
});

Deno.test("session infra manifest binds the tab record", () => {
  const record = { repoRef: infraRepoRef("did:plc:me"), sync: "Both" };
  const infra = sessionInfraManifest("web-origin-abc-tab123", undefined, record);
  assert(infra.spec.ttlSeconds === ORIGIN_INFRA_TTL_SECONDS, "ttl");
  assert(infra.spec.record.repoRef.name === "repo-did-plc-me" && infra.spec.record.sync === "Both", "record");
  assert(sessionInfraManifest("local").spec.record === undefined, "unbound manifest");
});

Deno.test("record conditions drive the sync wait", () => {
  const withConditions = (conditions) => ({ status: { conditions } });
  assert(!recordSynced(withConditions([{ type: "Synced", status: "False", reason: "Creating" }])), "creating");
  assert(recordSynced(withConditions([{ type: "Synced", status: "True", reason: "InSync" }])), "in sync");
  assert(recordSyncError(withConditions([{ type: "Synced", status: "False", reason: "RecordUnavailable", message: "no repo.read" }])) === "no repo.read", "unavailable");
  assert(recordSyncError(withConditions([{ type: "RecordValid", status: "False", message: "bad record" }])) === "bad record", "invalid");
  assert(recordSyncError(withConditions([{ type: "SyncConflict", status: "True", reason: "RecordWon", message: "record won" }])) === "", "conflict is not fatal");
  assert(infraRecordURI({ status: { record: { uri: "at://did:plc:me/computer.socialweb.ephemeralInfra/web-x" } } }, "did:plc:me", "web-x") ===
    "at://did:plc:me/computer.socialweb.ephemeralInfra/web-x", "uri");
});

Deno.test("addContract replaces, appends and prunes released entries", () => {
  const released = { name: "old", state: "Released" };
  const active = { name: "live", state: "Requested" };
  const policy = "requester-tangled-vouch";
  const out = addContract([released, active], { name: "new", state: "Requested", policy }, 2);
  assert(out.contracts.map((c) => c.name).join() === "live,new", out.contracts.map((c) => c.name).join());
  assert(out.dropped.join() === "old", out.dropped.join());
  const replaced = addContract([active], { ...active, state: "Released" }, 1);
  assert(replaced.contracts[0].state === "Released" && replaced.dropped.length === 0, "replace");
  let full = false;
  try {
    addContract([active], { name: "new", state: "Requested", policy }, 1);
  } catch (err) {
    full = /record is full/.test(err.message);
  }
  assert(full, "full active record refuses");
});

Deno.test("finishedInfraContracts marks only terminal requested entries", () => {
  const infra = { metadata: { name: "web-origin-abc-tab123" }, spec: { contracts: [
    { name: "vmshell-a", state: "Requested" },
    { name: "vmshell-b", state: "Requested" },
    { name: "vmshell-c", state: "Released" },
  ] } };
  const contracts = [
    { name: "einfra-web-origin-abc-tab123-vmshell-a", phase: "Released" },
    { name: "einfra-web-origin-abc-tab123-vmshell-b", phase: "Held" },
    { name: "einfra-other-vmshell-c", phase: "Failed" },
  ];
  const done = finishedInfraContracts(infra, contracts);
  assert(done.length === 1 && done[0].name === "vmshell-a", JSON.stringify(done));
});

Deno.test("association: a subject goes on the template with a secretsRef, even without the agent", () => {
  const did = "did:plc:ewvi7nxzyoun6zhxrhs64oiz";
  const m = contractManifest({ name: "vmshell-assoc1", association: did });
  assert(JSON.stringify(m.spec.association) === JSON.stringify({ subject: did }), "association, refFile left to socialweb");
  assert(m.spec.secretsRef?.path === "contracts/vmshell-assoc1/secrets" && m.spec.agent === undefined, "secretsRef without the agent");
  const out = infraContract({ infra: "s1", short: "z", association: did });
  assert(out.contract.association?.subject === did && out.contract.secretsRef === undefined, "template carries it");
  assert(out.secret?.secretsRef.path === "contracts/einfra-s1-z/secrets", "secrets entry for the ref file");
});

Deno.test("association: none means no field and no secretsRef", () => {
  const m = contractManifest({ name: "vmshell-plain1" });
  assert(m.spec.association === undefined && m.spec.secretsRef === undefined, "no association, no secretsRef");
  assert(infraContract({ infra: "s1", short: "p" }).contract.association === undefined, "template without it");
});

Deno.test("associationSubject: empty, valid, invalid and own DID", () => {
  const self = "did:plc:lpfuqerea3deuoyrn7ojser4";
  assert(associationSubject("  ", self).subject === "" && !associationSubject("", self).error, "empty is no association");
  assert(associationSubject(" did:plc:ewvi7nxzyoun6zhxrhs64oiz ", self).subject === "did:plc:ewvi7nxzyoun6zhxrhs64oiz", "plc, trimmed");
  assert(associationSubject("did:web:example.com", self).subject === "did:web:example.com", "web");
  for (const bad of ["alice.bsky.social", "did:key:z6Mk", "did:plc:", "did:plc:a b", "at://did:plc:x/c/k"]) {
    assert(associationSubject(bad, self).error, `rejects ${bad}`);
  }
  assert(/own DID/.test(associationSubject(self, self).error), "own DID rejected");
});

Deno.test("infra record binding watches through the jetstream EventSources", () => {
  const b = infraRecordBinding("did:plc:me");
  assert(b.sync === "Both" && b.repoRef.kind === "AtprotoRepo", "repo, both ways");
  assert(JSON.stringify(b.eventSourceRefs.map((r) => [r.group, r.kind, r.name])) ===
    JSON.stringify([["events.socialweb.computer", "EventSource", "jetstream-bsky-us-east"],
      ["events.socialweb.computer", "EventSource", "jetstream-bsky-us-west"]]), "sources");
  assert(!infraRecordStale(b), "current binding");
  const { eventSourceRefs: _, ...old } = b;
  assert(infraRecordStale(old), "a saved binding without eventSourceRefs is repaired");
  assert(infraRecordStale({ ...b, sync: "Publish" }) && infraRecordStale(null), "publish-only or missing");
});

Deno.test("always-run/empty-policy-default-tangled-vouch: addContract refuses an active entry without requester-tangled-vouch", () => {
  const { contract } = infraContract({ infra: "s1", short: "ok" });
  assert(addContract([], contract).contracts[0].policy === "requester-tangled-vouch", "page entry carries it");
  // decision follows: a refusal returns nothing to PATCH (setInfraContract
  // writes only what addContract returns) and leaves the record as it was.
  const record = [{ name: "keep", state: "Held", policy: "requester-tangled-vouch" }];
  const before = JSON.stringify(record);
  for (const policy of [undefined, "", "open", "requester-only-me"]) {
    let threw = false, out;
    try {
      out = addContract(record, { ...contract, policy });
    } catch (err) {
      threw = /refusing contract/.test(err.message);
    }
    assert(threw && out === undefined && JSON.stringify(record) === before, `refused policy ${policy}, nothing written`);
  }
  const released = { name: "old", state: "Released" };
  assert(addContract([], released).contracts[0].policy === undefined, "a release needs no policy");
});

Deno.test("always-run/empty-policy-default-tangled-vouch: other active entries in the record are written back with the policy", () => {
  const { contract } = infraContract({ infra: "s1", short: "new" });
  const record = [
    { name: "legacy", state: "Requested" },
    { name: "open", state: "Staged", policy: "open" },
    { name: "done", state: "Released" },
  ];
  const out = addContract(record, contract).contracts;
  const by = Object.fromEntries(out.map((c) => [c.name, c.policy]));
  assert(by.legacy === "requester-tangled-vouch" && by.open === "requester-tangled-vouch", "active entries repaired");
  assert(by.done === undefined && record[0].policy === undefined, "released left alone, input not mutated");
});

Deno.test("always-run/empty-policy-default-tangled-vouch: no manifest option drops or changes the policy", () => {
  for (const opts of [{}, { wootty: true }, { agent: true }, { association: "did:plc:x" }, { policy: "open" }, { ownerDid: "did:plc:me" }]) {
    assert(contractManifest({ name: "vmshell-p", ...opts }).spec.policy === "requester-tangled-vouch", JSON.stringify(opts));
  }
});

Deno.test("guest tools: on adds guestTools [socialweb-computer] to the template, policy kept", () => {
  const m = contractManifest({ name: "vmshell-r2", guestTools: true });
  assert(JSON.stringify(m.spec.guestTools) === '["socialweb-computer"]', "guestTools");
  assert(m.spec.policy === "requester-tangled-vouch", "policy kept");
  const out = infraContract({ infra: "s1", short: "r2", guestTools: true, agent: true });
  assert(JSON.stringify(out.contract.guestTools) === '["socialweb-computer"]' && out.contract.policy === "requester-tangled-vouch", "template");
  assert(addContract([], out.contract).contracts[0].guestTools.length === 1, "addContract accepts it");
});

Deno.test("guest tools: R2 asks for its own association with a secretsRef", () => {
  const m = contractManifest({ name: "vmshell-r2a", guestTools: true });
  assert(JSON.stringify(m.spec.association) === '{"fromGuest":true}', "fromGuest");
  assert(m.spec.secretsRef?.path === "contracts/vmshell-r2a/secrets" && m.spec.agent === undefined, "secretsRef without agent");
  const out = infraContract({ infra: "s1", short: "r2a", guestTools: true });
  assert(out.contract.association?.fromGuest === true && out.secret?.secretsRef, "template and secrets entry");
  let refused = "";
  try {
    contractManifest({ name: "vmshell-r2b", guestTools: true, association: "did:plc:x" });
  } catch (err) {
    refused = err.message;
  }
  assert(refused === "R2 associates itself; leave Associate DID empty", `refused: ${refused}`);
});

Deno.test("guest tools: off sends no field", () => {
  const off = contractManifest({ name: "vmshell-x" });
  assert(off.spec.association === undefined && off.spec.secretsRef === undefined, "no association, no secretsRef");
  assert(contractManifest({ name: "vmshell-x" }).spec.guestTools === undefined, "default off");
  assert(contractManifest({ name: "vmshell-x", guestTools: false }).spec.guestTools === undefined, "off");
  assert(infraContract({ infra: "s1", short: "x" }).contract.guestTools === undefined, "template off");
});

Deno.test("schemaHasGuestTools reads spec.properties.guestTools", () => {
  const schema = (props) => ({ spec: { versions: [{ schema: { properties: { spec: { properties: props } } } }] } });
  assert(schemaHasGuestTools(schema({ guestTools: {} })) && !schemaHasGuestTools(schema({})) && !schemaHasGuestTools(null), "served");
});

Deno.test("Start R2: shown only on a Held contract with R2 guest tools and fromGuest", () => {
  const held = { name: "c", phase: "Held", guestTools: ["socialweb-computer"], fromGuest: true };
  assert(r2StartShown(held), "Held R2 contract");
  assert(!r2StartShown({ ...held, phase: "Pending" }), "not Held");
  assert(!r2StartShown({ ...held, guestTools: [] }) && !r2StartShown({ ...held, guestTools: undefined }), "no guest tools");
  assert(!r2StartShown({ ...held, fromGuest: false }), "no fromGuest");
  assert(!r2StartShown({ ...held, release: true }) && !r2StartShown(null), "releasing or missing");
});

Deno.test("A8 release notes: cascade wait, request phase and token revokes", () => {
  const ev = { name: "c-r2-release", contract: "c", direction: "outbound", nsid: "com.publicdomainrelay.temp.compute.events.guest.r2Release", phase: "Published" };
  const waiting = releaseNotes({ name: "c", phase: "Releasing", step: "releasingGuestRequester", atircRevokes: "Pending" }, [ev]);
  assert(waiting[0] === "R2 release: waiting for the guest requester to release its contracts (up to 24 min), request Published", waiting[0]);
  assert(waiting[1] === "tokens: revoking", waiting[1]);
  const after = releaseNotes({ name: "c", phase: "Released", step: "released", atircRevokes: "Done" }, [ev]);
  assert(after[0].startsWith("R2 release: asked (Published)") && after[1] === "tokens: revoked", after.join("|"));
  assert(/ABANDONED/.test(releaseNotes({ name: "c", atircRevokes: "Abandoned" })[0]), "abandoned flagged");
  assert(releaseNotes({ name: "c", phase: "Held" }, [{ ...ev, contract: "other" }]).length === 0, "nothing for a plain contract");
});

Deno.test("A8 release notes from status.contract.r2Release (rc127 phases)", () => {
  const c = (r2Release) => ({ name: "c", phase: "Releasing", step: "releasingGuestRequester", r2Release });
  const note = (r2) => releaseNotes(c(r2))[0];
  assert(note({ phase: "Requested", requestedAt: "t1" }) === "R2 release: asked t1, waiting (up to 24 min)", "requested, no parsable time");
  const at = "2026-10-30T12:00:00Z", tenLater = Date.parse(at) + 10 * 60_000;
  assert(releaseNotes(c({ phase: "Requested", requestedAt: at }), [], tenLater)[0] ===
    `R2 release: asked ${at}, waiting (up to 24 min, ~14 min left)`, "time left after a reload");
  assert(releaseNotes(c({ phase: "Requested", requestedAt: at }), [], tenLater + 60 * 60_000)[0].endsWith("~0 min left)"), "never negative");
  assert(note({ phase: "Released", ok: true, requestedAt: "t1", answeredAt: "t2" }) === "R2 release: guest released t2", "released");
  assert(note({ phase: "Failed", message: "socialweb-r2-release.service failed", answeredAt: "t2" }) ===
    "R2 release: FAILED: socialweb-r2-release.service failed", "failed (ok omitted)");
  assert(note({ phase: "TimedOut", message: "no r2Released in 12m0s" }) ===
    "R2 release: TIMED OUT after 24 min; bidder backstop releases R2's VMs after the association expires", "timed out");
  assert(note({ phase: "SomethingNew", message: "m" }) === "R2 release: [SomethingNew] m", "unknown phase raw");
  assert(releaseNotes(c({ phase: "Requested", requestedAt: "t1" })).length === 1, "the status replaces the wait line");
});

// --- release requests (socialweb rc127 H1, computecontracts 30) ---

const API = "/apis/socialweb.computer/v1alpha1";

// A fake kcpREST over one ComputeContract (and optionally its EphemeralInfra);
// a PATCH applies the SSA body's spec onto the stored contract.
function fakeKcp(cc, infra = null) {
  const calls = [];
  const rest = (method, path, body, contentType) => {
    calls.push({ method, path, body, contentType });
    if (method === "GET" && path === `${API}/computecontracts/${cc?.metadata?.name}`) return Promise.resolve(cc);
    if (method === "GET" && infra && path === `${API}/ephemeralinfras/${infra.metadata.name}`) return Promise.resolve(infra);
    if (method === "GET") return Promise.reject(Object.assign(new Error(`GET ${path}: 404`), { status: 404 }));
    if (method === "PATCH" && cc) {
      cc.spec = { ...cc.spec, ...body.spec };
      return Promise.resolve(cc);
    }
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  };
  const infraSets = [];
  const setInfraContract = (name, entry) => {
    calls.push({ method: "setInfraContract", name, entry });
    infraSets.push([name, entry]);
    return Promise.resolve();
  };
  return { rest, calls, infraSets, setInfraContract };
}

Deno.test("release: releaseRequestedAt is now, and strictly after a previous request", () => {
  const now = new Date("2026-10-30T12:00:00.123Z");
  assert(releaseRequestedAt("", now) === "2026-10-30T12:00:00.123Z", "fresh");
  assert(releaseRequestedAt(undefined, now) === "2026-10-30T12:00:00.123Z", "undefined previous");
  assert(releaseRequestedAt("2026-10-30T11:00:00.000Z", now) === "2026-10-30T12:00:00.123Z", "older previous");
  assert(releaseRequestedAt("2026-10-30T12:00:00.123Z", now) === "2026-10-30T12:00:00.124Z", "same ms bumps");
  assert(releaseRequestedAt("2026-10-30T13:00:00.000Z", now) === "2026-10-30T13:00:00.001Z", "clock behind the stored one");
  assert(releaseRequestedAt("garbage", now) === "2026-10-30T12:00:00.123Z", "unparsable previous");
});

Deno.test("release: the SSA body sets spec.release with releaseRequestedAt under the release manager", () => {
  const a = releaseApply(API, "einfra-x-vmshell-1", "2026-10-30T12:00:00.000Z");
  assert(RELEASE_FIELD_MANAGER === "socialweb.computer/requester-release", RELEASE_FIELD_MANAGER);
  assert(a.path === `${API}/computecontracts/einfra-x-vmshell-1?fieldManager=socialweb.computer%2Frequester-release&force=true`, a.path);
  assert(a.contentType === "application/apply-patch+yaml", a.contentType);
  assert(JSON.stringify(a.body) === JSON.stringify({
    apiVersion: "socialweb.computer/v1alpha1",
    kind: "ComputeContract",
    metadata: { name: "einfra-x-vmshell-1" },
    spec: { release: true, releaseRequestedAt: "2026-10-30T12:00:00.000Z" },
  }), JSON.stringify(a.body));
});

Deno.test("release: a plain contract gets the timestamped apply and no infra write", async () => {
  const k = fakeKcp({ metadata: { name: "vm1" }, spec: {} });
  const now = new Date("2026-10-30T12:00:00.000Z");
  const at = await requestRelease({ rest: k.rest, api: API, name: "vm1", setInfraContract: k.setInfraContract, now });
  const patches = k.calls.filter((c) => c.method === "PATCH");
  assert(at === now.toISOString() && patches.length === 1, JSON.stringify(k.calls));
  assert(patches[0].body.spec.release === true && patches[0].body.spec.releaseRequestedAt === at, JSON.stringify(patches[0]));
  assert(patches[0].path === `${API}/computecontracts/vm1?fieldManager=socialweb.computer%2Frequester-release&force=true`, patches[0].path);
  assert(patches[0].contentType === "application/apply-patch+yaml", `SSA, not merge-patch: ${patches[0].contentType}`);
  assert(JSON.stringify(Object.keys(patches[0].body.spec)) === JSON.stringify(["release", "releaseRequestedAt"]), "both fields, nothing else");
  assert(k.calls.every((c) => c.method === "GET" || c.method === "PATCH"), "GET + one PATCH (apply) only");
  assert(k.infraSets.length === 0, "no infra");
});

Deno.test("release: a session-infra contract is applied first, then its entry marked Released", async () => {
  const cc = {
    metadata: { name: "einfra-t-vmshell-ab", labels: { "infra.socialweb.computer/contract": "vmshell-ab" },
      ownerReferences: [{ kind: "EphemeralInfra", name: "t" }] },
    spec: {},
  };
  const infra = { metadata: { name: "t" }, spec: { contracts: [{ name: "vmshell-ab", state: "Requested", policy: "p" }] } };
  const k = fakeKcp(cc, infra);
  const at = await requestRelease({ rest: k.rest, api: API, name: cc.metadata.name, setInfraContract: k.setInfraContract,
    now: new Date("2026-10-30T12:00:00.000Z") });
  const order = k.calls.filter((c) => c.method !== "GET").map((c) => c.method);
  assert(JSON.stringify(order) === JSON.stringify(["PATCH", "setInfraContract"]), JSON.stringify(order));
  assert(cc.spec.releaseRequestedAt === at && cc.spec.release === true, JSON.stringify(cc.spec));
  assert(k.infraSets[0][0] === "t" && k.infraSets[0][1].state === "Released" && k.infraSets[0][1].policy === "p", JSON.stringify(k.infraSets));
});

Deno.test("release: a second Release (button, Release again, d.release, leftover sweep) sends a fresh timestamp", async () => {
  const cc = {
    metadata: { name: "einfra-t-vmshell-ab", labels: { "infra.socialweb.computer/contract": "vmshell-ab" },
      ownerReferences: [{ kind: "EphemeralInfra", name: "t" }] },
    spec: {},
  };
  const infra = { metadata: { name: "t" }, spec: { contracts: [{ name: "vmshell-ab", state: "Requested", policy: "p" }] } };
  const k = fakeKcp(cc, infra);
  const now = new Date("2026-10-30T12:00:00.000Z");
  const first = await requestRelease({ rest: k.rest, api: API, name: cc.metadata.name, setInfraContract: k.setInfraContract, now });
  infra.spec.contracts[0].state = "Released";
  const second = await requestRelease({ rest: k.rest, api: API, name: cc.metadata.name, setInfraContract: k.setInfraContract, now });
  const later = await requestRelease({ rest: k.rest, api: API, name: cc.metadata.name, setInfraContract: k.setInfraContract,
    now: new Date("2026-10-30T12:05:00.000Z") });
  assert(first < second && second < later, `${first} ${second} ${later}`);
  const stamps = k.calls.filter((c) => c.method === "PATCH").map((c) => c.body.spec.releaseRequestedAt);
  assert(JSON.stringify(stamps) === JSON.stringify([first, second, later]), JSON.stringify(stamps));
  assert(cc.spec.releaseRequestedAt === later, "the newest request is in the spec");
  assert(k.infraSets.length === 1, "an already Released infra entry is not rewritten");
});

Deno.test("release: a missing contract is never created by the apply", async () => {
  const k = fakeKcp(null);
  let failed = false;
  await requestRelease({ rest: k.rest, api: API, name: "gone", setInfraContract: k.setInfraContract }).catch(() => failed = true);
  assert(failed && !k.calls.some((c) => c.method === "PATCH"), JSON.stringify(k.calls));
});

Deno.test("release: status.release answers only the request whose requestedAt matches", () => {
  const t1 = "2026-10-30T11:00:00.000Z", t2 = "2026-10-30T12:00:00.000Z";
  const failed = { name: "c", phase: "Failed", release: true, releaseFailed: true, message: "vm.delete was not acknowledged" };
  assert(releaseAnswered({ ...failed }), "no releaseRequestedAt (pre-rc127): answered");
  assert(releaseAnswered({ ...failed, releaseRequestedAt: t1, releaseAnsweredAt: t1 }), "same request");
  assert(!releaseAnswered({ ...failed, releaseRequestedAt: t2, releaseAnsweredAt: t1 }), "newer request");
  assert(!releaseAnswered({ ...failed, releaseRequestedAt: t2 }), "no answer yet");

  const pending = { ...failed, releaseRequestedAt: t2, releaseAnsweredAt: t1 };
  assert(releasePending(pending) && isReleasing(pending) && !isActive(pending), "a newer request on a failed release is pending");
  assert(!releasePending({ ...pending, releaseFailed: false }), "Failed without ReleaseFailed is not restarted");
  assert(!releasePending({ ...pending, phase: "Released" }), "Released stays Released");
  assert(!releasePending({ ...failed, releaseRequestedAt: t1, releaseAnsweredAt: t1 }), "answered: the Failed is current");

  const label = phaseLabel(pending);
  assert(label.phase === "Releasing" && label.text === ` Releasing · release requested ${t2}`, JSON.stringify(label));
  const answered = phaseLabel({ ...failed, releaseRequestedAt: t1, releaseAnsweredAt: t1 });
  assert(answered.phase === "Failed" && answered.text === " Failed", JSON.stringify(answered));
  assert(phaseLabel({ phase: "Held", release: true }).text === " Held · releasing", "releasing a held VM");
  assert(phaseLabel({ phase: "Released", release: true }).text === " Released", "released");
  assert(phaseLabel({}).text === " Pending", "no phase");

  const notes = releaseNotes({ ...pending, atircRevokes: "Abandoned" });
  assert(notes[0] === `release: requested ${t2}, pending (the earlier release failed: vm.delete was not acknowledged)`, notes[0]);
  assert(notes[1] === "tokens: revoke ABANDONED, revoke them by hand (earlier attempt)", notes[1]);
  const current = releaseNotes({ ...failed, releaseRequestedAt: t1, releaseAnsweredAt: t1, atircRevokes: "Done" });
  assert(current.length === 1 && current[0] === "tokens: revoked", current.join("|"));
});

Deno.test("release: Release again only on a failed release that answers the latest request", () => {
  const t1 = "2026-10-30T11:00:00.000Z", t2 = "2026-10-30T12:00:00.000Z";
  const failed = { name: "c", phase: "Failed", release: true, releaseFailed: true, releaseRequestedAt: t1, releaseAnsweredAt: t1 };
  assert(canReleaseAgain(failed), "offered");
  assert(canReleaseAgain(failed, t1), "this tab sent the answered request");
  assert(!canReleaseAgain(failed, t2), "this tab sent a newer one the feed has not shown yet");
  assert(!canReleaseAgain({ ...failed, releaseRequestedAt: t2 }), "a newer request is pending");
  assert(!canReleaseAgain({ ...failed, releaseFailed: false }), "not a release failure");
  assert(!canReleaseAgain({ ...failed, phase: "Released" }) && !canReleaseAgain({ ...failed, phase: "Held" }), "other phases");
  assert(canReleaseAgain({ name: "c", phase: "Failed", release: true, releaseFailed: true }), "pre-rc127 contract without a timestamp");
});

Deno.test("release: every page release path goes through requestRelease", async () => {
  const app = await Deno.readTextFile(new URL("./app.js", import.meta.url));
  const demo = await Deno.readTextFile(new URL("../browser-demo.ts", import.meta.url));
  assert(!/release:\s*true/.test(app), "app.js writes spec.release only through vmshell.requestRelease");
  assert(/async function releaseContract\(name\) \{[\s\S]*?vmshell\.requestRelease\(/.test(app), "releaseContract uses requestRelease");
  assert(/release: \(name\) => releaseContract\(name\)/.test(app), "d.release is releaseContract");
  const callers = [...app.matchAll(/releaseContract\(name\)/g)].length;
  assert(callers === 4, `releaseContract callers (definition, Release, Release again, d.release): ${callers}`);
  assert(!/release:\s*true|merge-patch/.test(demo) && /d\.release\(/.test(demo), "the demo's leftover sweep uses d.release");
});
