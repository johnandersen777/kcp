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
  releaseWorkPending,
  canReleaseAgain,
  phaseLabel,
  releaseAnswered,
  finishedEntry,
  latestStamp,
  releaseTimedOut,
  stampAfter,
  updateInfraEntry,
  withReleaseSent,
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

// --- release requests (socialweb r128 10558978, ephemeralinfras 11) ---

const API = "/apis/socialweb.computer/v1alpha1";
const T0 = "2026-10-30T12:00:00.000Z";

function sessionCC(extra = {}) {
  return {
    metadata: { name: "einfra-t-vmshell-ab", labels: { "infra.socialweb.computer/contract": "vmshell-ab" },
      ownerReferences: [{ kind: "EphemeralInfra", name: "t" }] },
    spec: { vmName: "einfra-t-vmshell-ab", ...extra },
  };
}

function sessionInfra(entry = {}) {
  return {
    metadata: { name: "t", resourceVersion: "7" },
    spec: {
      contracts: [
        { name: "vmshell-zz", state: "Requested", policy: "p", vmName: "other" },
        { name: "vmshell-ab", state: "Requested", policy: "p", vmName: "einfra-t-vmshell-ab", ...entry },
      ],
      secrets: [{ contract: "vmshell-ab", secretsRef: { mount: "m", path: "x" } }],
    },
  };
}

// A fake kcpREST over one ComputeContract and its EphemeralInfra: a merge
// patch of the infra replaces spec.contracts (an atomic list) and checks the
// resourceVersion; conflicts (number of 409s to answer first) simulate a
// concurrent writer.
function fakeKcp(cc, infra = null, conflicts = 0) {
  const calls = [];
  const rest = (method, path, body, contentType) => {
    calls.push({ method, path, body: body === undefined ? undefined : structuredClone(body), contentType });
    if (method === "GET" && cc && path === `${API}/computecontracts/${cc.metadata.name}`) return Promise.resolve(structuredClone(cc));
    if (method === "GET" && infra && path === `${API}/ephemeralinfras/${infra.metadata.name}`) return Promise.resolve(structuredClone(infra));
    if (method === "GET") return Promise.reject(Object.assign(new Error(`GET ${path}: 404 not found`), { status: 404 }));
    if (method === "PATCH" && infra && path === `${API}/ephemeralinfras/${infra.metadata.name}`) {
      if (conflicts > 0 || (body.metadata?.resourceVersion && body.metadata.resourceVersion !== infra.metadata.resourceVersion)) {
        conflicts--;
        infra.metadata.resourceVersion = String(Number(infra.metadata.resourceVersion) + 1);
        return Promise.reject(Object.assign(new Error(`PATCH ${path}: 409 conflict`), { status: 409 }));
      }
      infra.spec = { ...infra.spec, ...body.spec };
      infra.metadata.resourceVersion = String(Number(infra.metadata.resourceVersion) + 1);
      return Promise.resolve(structuredClone(infra));
    }
    return Promise.reject(new Error(`unexpected ${method} ${path}`));
  };
  return { rest, calls, writes: () => calls.filter((c) => c.method !== "GET") };
}

Deno.test("release: releaseRequestedAt is now, and strictly after a previous request", () => {
  const now = new Date("2026-10-30T12:00:00.123Z");
  assert(releaseRequestedAt("", now) === "2026-10-30T12:00:00.123Z", "fresh");
  assert(releaseRequestedAt(undefined, now) === "2026-10-30T12:00:00.123Z", "undefined previous");
  assert(releaseRequestedAt("2026-10-30T11:00:00.000Z", now) === "2026-10-30T12:00:00.123Z", "older previous");
  assert(releaseRequestedAt("2026-10-30T12:00:00.123Z", now) === "2026-10-30T12:00:00.124Z", "same ms bumps");
  assert(releaseRequestedAt("2026-10-30T13:00:00.000Z", now) === "2026-10-30T13:00:00.001Z", "clock behind the stored one");
  assert(releaseRequestedAt("garbage", now) === "2026-10-30T12:00:00.123Z", "unparsable previous");
  assert(latestStamp("", "2026-10-30T11:00:00Z", "2026-10-30T12:00:00.5Z", "garbage") === "2026-10-30T12:00:00.5Z", "latest");
  assert(latestStamp(undefined, "") === "", "none");
  assert(stampAfter("2026-10-30T12:00:00.500Z", "2026-10-30T12:00:00Z"), "parsed, not compared as strings");
  assert(!stampAfter("", "2026-10-30T12:00:00Z") && stampAfter(T0, "") && !stampAfter(T0, T0), "edges");
});

Deno.test("release: Release merge-patches only the EphemeralInfra entry, never the ComputeContract", async () => {
  const cc = sessionCC();
  const infra = sessionInfra();
  const k = fakeKcp(cc, infra);
  const at = await requestRelease({ rest: k.rest, api: API, name: cc.metadata.name, now: new Date(T0) });
  assert(at === T0, at);
  const writes = k.writes();
  assert(writes.length === 1, JSON.stringify(writes));
  const [w] = writes;
  assert(w.method === "PATCH", w.method);
  assert(w.path === `${API}/ephemeralinfras/t`, `no fieldManager: the page's default manager owns spec.contracts: ${w.path}`);
  assert(w.contentType === "application/merge-patch+json", w.contentType);
  assert(JSON.stringify(w.body) === JSON.stringify({
    spec: {
      contracts: [
        { name: "vmshell-zz", state: "Requested", policy: "p", vmName: "other" },
        { name: "vmshell-ab", state: "Released", policy: "p", vmName: "einfra-t-vmshell-ab", releaseRequestedAt: T0 },
      ],
    },
    metadata: { resourceVersion: "7" },
  }), JSON.stringify(w.body));
  assert(!k.calls.some((c) => c.method !== "GET" && c.path.includes("/computecontracts")), "no write to computecontracts");
  assert(infra.spec.contracts.length === 2 && infra.spec.secrets.length === 1, "the other entry and the secrets stay");
  assert(JSON.stringify(cc.spec) === JSON.stringify(sessionCC().spec), "the contract is untouched");
});

Deno.test("release: Release again sends a newer stamp on the entry, after the entry's and the contract's", async () => {
  const cc = sessionCC({ release: true, releaseRequestedAt: T0 });
  const infra = sessionInfra({ state: "Released", releaseRequestedAt: T0 });
  const k = fakeKcp(cc, infra);
  const now = new Date(T0); // the clock has not moved past the stored request
  const second = await requestRelease({ rest: k.rest, api: API, name: cc.metadata.name, now });
  assert(second === "2026-10-30T12:00:00.001Z", second);
  const later = await requestRelease({ rest: k.rest, api: API, name: cc.metadata.name, now: new Date("2026-10-30T12:05:00.000Z") });
  assert(later === "2026-10-30T12:05:00.000Z", later);
  const stamps = k.writes().map((w) => w.body.spec.contracts[1].releaseRequestedAt);
  assert(JSON.stringify(stamps) === JSON.stringify([second, later]), JSON.stringify(stamps));
  assert(k.writes().every((w) => w.path === `${API}/ephemeralinfras/t` && w.body.spec.contracts[1].state === "Released"), "entry only");
  assert(infra.spec.contracts[1].releaseRequestedAt === later, "the newest request is in the entry");

  // the contract carries a newer stamp than the entry (an earlier rc127 page): still strictly after it
  const ahead = sessionCC({ release: true, releaseRequestedAt: "2026-10-30T13:00:00.000Z" });
  const k2 = fakeKcp(ahead, sessionInfra({ state: "Released", releaseRequestedAt: T0 }));
  const third = await requestRelease({ rest: k2.rest, api: API, name: ahead.metadata.name, now: new Date(T0) });
  assert(third === "2026-10-30T13:00:00.001Z", third);
  assert(!k2.calls.some((c) => c.method !== "GET" && c.path.includes("/computecontracts")), "no write to computecontracts");
});

Deno.test("release: a concurrent infra write is a 409 and a fresh read, never a lost entry", async () => {
  const cc = sessionCC();
  const infra = sessionInfra();
  const k = fakeKcp(cc, infra, 1);
  const at = await requestRelease({ rest: k.rest, api: API, name: cc.metadata.name, now: new Date(T0) });
  const writes = k.writes();
  assert(writes.length === 2 && writes[1].body.metadata.resourceVersion === "8", JSON.stringify(writes.map((w) => w.body.metadata)));
  assert(infra.spec.contracts[1].releaseRequestedAt === at, "written on the retry");
  const stuck = fakeKcp(sessionCC(), sessionInfra(), 5);
  let failed = "";
  await requestRelease({ rest: stuck.rest, api: API, name: cc.metadata.name, now: new Date(T0) }).catch((err) => failed = err.message);
  assert(/409/.test(failed) && stuck.writes().length === 3, `gives up after 3 attempts: ${failed}`);
});

Deno.test("release: the serialize hook runs the read-modify-write in the page's infra queue", async () => {
  const k = fakeKcp(sessionCC(), sessionInfra());
  const order = [];
  const serialize = async (run) => {
    order.push("queued");
    const out = await run();
    order.push("done");
    return out;
  };
  await requestRelease({ rest: k.rest, api: API, name: "einfra-t-vmshell-ab", serialize, now: new Date(T0) });
  const infraCalls = k.calls.filter((c) => c.path.includes("/ephemeralinfras/"));
  assert(JSON.stringify(order) === '["queued","done"]' && infraCalls.length === 2, JSON.stringify(order));
});

Deno.test("release: a missing contract or entry is an error and no write", async () => {
  const gone = fakeKcp(null, sessionInfra());
  let failed = "";
  await requestRelease({ rest: gone.rest, api: API, name: "gone" }).catch((err) => failed = err.message);
  assert(/gone: 404/.test(failed) && gone.writes().length === 0, failed);
  const nul = { rest: (method) => method === "GET" ? Promise.resolve(null) : Promise.reject(new Error("write")) };
  failed = "";
  await requestRelease({ rest: nul.rest, api: API, name: "vm1" }).catch((err) => failed = err.message);
  assert(failed === "ComputeContract vm1 not found", failed);

  const plain = fakeKcp({ metadata: { name: "vm1" }, spec: {} });
  failed = "";
  await requestRelease({ rest: plain.rest, api: API, name: "vm1" }).catch((err) => failed = err.message);
  assert(/belongs to no EphemeralInfra/.test(failed) && plain.writes().length === 0, failed);

  const noEntry = sessionInfra();
  noEntry.spec.contracts.pop();
  const k = fakeKcp(sessionCC(), noEntry);
  failed = "";
  await requestRelease({ rest: k.rest, api: API, name: "einfra-t-vmshell-ab" }).catch((err) => failed = err.message);
  assert(/EphemeralInfra t has no entry for einfra-t-vmshell-ab/.test(failed) && k.writes().length === 0, failed);
});

Deno.test("markFinished: finishedEntry keeps releaseRequestedAt and skips an entry a Release already wrote", async () => {
  assert(finishedEntry({ name: "a", state: "Released", releaseRequestedAt: T0 }) === null, "already Released: no write");
  assert(finishedEntry({ name: "a", state: "Staged" }) === null, "Staged");
  const kept = finishedEntry({ name: "a", state: "Requested", policy: "p", releaseRequestedAt: T0 });
  assert(kept.state === "Released" && kept.releaseRequestedAt === T0 && kept.policy === "p", JSON.stringify(kept));

  // markFinished read the infra before a Release was queued; the queued
  // update reads it again, so the Release's stamp survives.
  const infra = sessionInfra();
  const k = fakeKcp(sessionCC(), infra);
  const stale = structuredClone(infra.spec.contracts[1]);
  await requestRelease({ rest: k.rest, api: API, name: "einfra-t-vmshell-ab", now: new Date(T0) });
  const wrote = await updateInfraEntry({ rest: k.rest, api: API, infra: "t", match: (c) => c.name === stale.name, update: finishedEntry });
  assert(wrote === null && k.writes().length === 1, "no second write");
  assert(infra.spec.contracts[1].releaseRequestedAt === T0 && infra.spec.contracts[1].state === "Released", JSON.stringify(infra.spec.contracts[1]));

  // a Requested entry is marked Released with every other field (and the other entries) intact
  const k2 = fakeKcp(sessionCC(), sessionInfra({ releaseRequestedAt: T0 }));
  const done = await updateInfraEntry({ rest: k2.rest, api: API, infra: "t", match: (c) => c.name === "vmshell-ab", update: finishedEntry });
  const [w] = k2.writes();
  assert(done.state === "Released" && w.contentType === "application/merge-patch+json" && w.path === `${API}/ephemeralinfras/t`, JSON.stringify(w));
  assert(w.body.spec.contracts[1].releaseRequestedAt === T0 && w.body.spec.contracts[0].state === "Requested", JSON.stringify(w.body));

  const app = await Deno.readTextFile(new URL("./app.js", import.meta.url));
  const mark = app.match(/async function markFinishedInfraContracts[\s\S]*?\n}\n/)?.[0] ?? "";
  assert(/infraQueued\(\(\) => vmshell\.updateInfraEntry\(/.test(mark) && /update: vmshell\.finishedEntry/.test(mark),
    "markFinishedInfraContracts updates the fresh entry inside the queue");
  assert(!/setInfraContract\(/.test(mark), "and never writes the entry it read before queueing");
});

Deno.test("release: until the contract carries this tab's stamp the row says release requested", () => {
  const t1 = "2026-10-30T11:00:00.000Z", t2 = "2026-10-30T12:00:00.000Z";
  const held = { name: "c", phase: "Held" };
  const sent = withReleaseSent(held, t2);
  assert(sent.release && sent.releaseRequestedAt === t2 && !isActive(sent) && releaseWorkPending(sent), JSON.stringify(sent));
  assert(phaseLabel(sent).text === " Held · releasing", phaseLabel(sent).text);
  assert(withReleaseSent(held, "") === held && withReleaseSent(held) === held, "nothing sent");
  const copied = { ...held, release: true, releaseRequestedAt: t2 };
  assert(withReleaseSent(copied, t2) === copied, "the controller copied it: the feed is current");
  const released = { name: "c", phase: "Released", release: true, releaseRequestedAt: t1 };
  assert(withReleaseSent(released, t2) === released, "Released stays Released");

  // Release again on a release that timed out: the old timeout is not shown meanwhile
  const timedOut = { name: "c", phase: "Failed", release: true, releaseFailed: true, releaseFailedReason: "ReleaseBound",
    releaseRequestedAt: t1, releaseAnsweredAt: t1, message: "release bound passed" };
  const again = withReleaseSent(timedOut, t2);
  const label = phaseLabel(again);
  assert(label.phase === "Releasing" && label.text === ` Releasing · release requested ${t2}`, JSON.stringify(label));
  assert(!releaseTimedOut(again) && !canReleaseAgain(again, t2) && releaseWorkPending(again), "no timeout, no button, pending");
  assert(releaseNotes(again)[0] === `release: requested ${t2}, pending (the earlier release failed: release bound passed)`, releaseNotes(again)[0]);
  // the controller copied it, the requester has not answered yet
  const copiedAgain = { ...timedOut, releaseRequestedAt: t2 };
  assert(phaseLabel(withReleaseSent(copiedAgain, t2)).text === ` Releasing · release requested ${t2}`, "still requested");
  // answered: the new outcome shows
  const answeredAgain = { ...timedOut, releaseRequestedAt: t2, releaseAnsweredAt: t2 };
  assert(phaseLabel(withReleaseSent(answeredAgain, t2)).text === " Failed · release timed out", "the new answer timed out too");
});

Deno.test("release: a ReleaseBound failure that answers the latest request says timed out and offers Release again", () => {
  const t1 = "2026-10-30T11:00:00.000Z", t2 = "2026-10-30T12:00:00.000Z";
  const timedOut = { name: "c", phase: "Failed", release: true, releaseFailed: true, releaseFailedReason: "ReleaseBound",
    releaseRequestedAt: t1, releaseAnsweredAt: t1 };
  assert(releaseTimedOut(timedOut), "timed out");
  const label = phaseLabel(timedOut);
  assert(label.phase === "Failed" && label.text === " Failed · release timed out", JSON.stringify(label));
  assert(canReleaseAgain(timedOut) && canReleaseAgain(timedOut, t1), "Release again offered");
  assert(!canReleaseAgain(timedOut, t2), "not while this tab's newer request is unseen");
  assert(!releaseTimedOut({ ...timedOut, releaseRequestedAt: t2 }), "a newer request is not answered by the timeout");
  assert(!releaseTimedOut({ ...timedOut, releaseFailedReason: "DestroyFailed" }), "other reasons");
  assert(phaseLabel({ ...timedOut, releaseFailedReason: "DestroyFailed" }).text === " Failed", "a destroy failure keeps the plain label");
  assert(!releaseTimedOut({ ...timedOut, phase: "Released" }), "released");
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
  assert(!/release:\s*true/.test(app), "app.js never writes spec.release");
  assert(!/computecontracts\/\$\{[^}]*\}\?fieldManager|apply-patch/.test(app), "app.js applies nothing to a ComputeContract");
  assert(/async function releaseContract\(name\) \{[\s\S]*?vmshell\.requestRelease\(/.test(app), "releaseContract uses requestRelease");
  assert(/release: \(name\) => releaseContract\(name\)/.test(app), "d.release is releaseContract");
  const callers = [...app.matchAll(/releaseContract\(name\)/g)].length;
  assert(callers === 4, `releaseContract callers (definition, Release, Release again, d.release): ${callers}`);
  assert(!/release:\s*true|merge-patch/.test(demo) && /d\.release\(/.test(demo), "the demo's leftover sweep uses d.release");
});

Deno.test("beforeunload: warns while release work runs in this tab", async () => {
  assert(releaseWorkPending({ phase: "Held", release: true }), "release in progress");
  assert(releaseWorkPending({ phase: "Released", atircRevokes: "Pending" }), "token revokes pending after Released");
  assert(releaseWorkPending({ phase: "Failed", release: true, atircRevokes: "Pending" }), "token revokes pending after Failed");
  assert(releaseWorkPending({ phase: "Releasing", step: "releasingGuestRequester" }), "R2 cascade waiting");
  assert(releaseWorkPending({ name: "c", phase: "Failed", release: true, releaseFailed: true,
    releaseRequestedAt: "2026-10-30T12:00:00.000Z", releaseAnsweredAt: "2026-10-30T11:00:00.000Z" }), "re-release pending");
  assert(!releaseWorkPending({ phase: "Held" }), "a held VM");
  assert(!releaseWorkPending({ phase: "Released", release: true, atircRevokes: "Done" }), "released, revokes done");
  assert(!releaseWorkPending({ phase: "Failed", release: true, atircRevokes: "Abandoned" }), "abandoned: nothing left to run");
  assert(!releaseWorkPending(null), "missing");
  const app = await Deno.readTextFile(new URL("./app.js", import.meta.url));
  assert(/computeReleasing = seen\.some\(vmshell\.releaseWorkPending\)/.test(app), "beforeunload state uses releaseWorkPending");
  assert(/const seen = contracts\.map\(\(c\) => vmshell\.withReleaseSent\(c, computeReleaseAsked\.get\(c\?\.name\)\)\)/.test(app),
    "over the contracts as this tab sees them (a Release sent, not yet copied)");
});
