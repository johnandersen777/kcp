// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import {
  contractManifest,
  contractName,
  isActive,
  isReleasing,
  randomToken,
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
  assert(m.spec.secretsRef === undefined, "no secretsRef");
  assert(m.spec.sshAuthorizedKeys[0] === "ssh-ed25519 AAAA page" && m.spec.ttlSecondsAfterFinished === undefined, "keys, no ttl");
  assert(m.spec.ownerDid === "did:plc:me" && m.spec.userData === undefined, "owner, no userData");
});

Deno.test("manifest with wootty carries both tunnels and the secretsRef", () => {
  const m = contractManifest({ name: "vmshell-both01", bidders: ["did:plc:x"], ownerDid: "", wootty: true });
  const tags = Object.fromEntries(m.spec.tunnels.map((t) => [t.tag, t]));
  assert(JSON.stringify(tags.ssh) === '{"tag":"ssh","port":22,"transport":"webrtc"}', "ssh");
  assert(JSON.stringify(tags.wootty) === '{"tag":"wootty","port":7681,"transport":"opentunnel","shell":{"envFile":"/etc/wootty/wootty.env"}}', "wootty");
  assert(m.spec.secretsRef.mount === "requester" && m.spec.secretsRef.path === "contracts/vmshell-both01/secrets", "secretsRef");
});

Deno.test("schemaHasShell reads tunnels.items.properties.shell", () => {
  const schema = (props) => ({ spec: { versions: [{ schema: { properties: { spec: { properties: { tunnels: { items: { properties: props } } } } } } }] } });
  assert(schemaHasShell(schema({ tag: {}, shell: {} })), "rc110");
  assert(!schemaHasShell(schema({ tag: {}, transport: {} })), "rc109");
  assert(!schemaHasShell(null), "missing");
});

Deno.test("bundle delivers the wootty env file", () => {
  const entries = JSON.parse(secretsBundle("tok"));
  assert(entries.length === 1 && entries[0].path === "/etc/wootty/wootty.env", "path");
  assert(entries[0].value === "WOOTTY_AUTH_TOKEN=tok\n", "value");
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
