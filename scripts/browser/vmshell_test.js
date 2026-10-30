// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import {
  ORIGIN_INFRA_TTL_SECONDS,
  addContract,
  contractManifest,
  contractName,
  finishedInfraContracts,
  fnv1a64,
  infraContract,
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
  const out = addContract([released, active], { name: "new", state: "Requested" }, 2);
  assert(out.contracts.map((c) => c.name).join() === "live,new", out.contracts.map((c) => c.name).join());
  assert(out.dropped.join() === "old", out.dropped.join());
  const replaced = addContract([active], { ...active, state: "Released" }, 1);
  assert(replaced.contracts[0].state === "Released" && replaced.dropped.length === 0, "replace");
  let full = false;
  try {
    addContract([active], { name: "new", state: "Requested" }, 1);
  } catch {
    full = true;
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
