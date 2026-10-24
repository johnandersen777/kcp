// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import {
  contractManifest,
  contractName,
  isActive,
  isReleasing,
  parseBidders,
  randomToken,
  secretsBundle,
  shellLink,
} from "./vmshell.js";

function assert(cond, message) {
  if (!cond) throw new Error(message);
}

Deno.test("token is 32 base64url chars", () => {
  const token = randomToken();
  assert(/^[A-Za-z0-9_-]{32}$/.test(token), token);
});

Deno.test("contract name is a DNS label", () => {
  assert(contractName(new Uint8Array([0, 1, 35, 36, 255, 10])) === "vmshell-ab9adk", contractName(new Uint8Array([0, 1, 35, 36, 255, 10])));
  assert(/^vmshell-[a-z0-9]{6}$/.test(contractName()), "random name");
});

Deno.test("manifest carries secretsRef and the shell tunnel", () => {
  const m = contractManifest({ name: "vmshell-abc123", bidders: parseBidders(""), ownerDid: "did:plc:me" });
  assert(m.spec.secretsRef.mount === "requester", "mount");
  assert(m.spec.secretsRef.path === "contracts/vmshell-abc123/secrets", "path");
  assert(m.spec.bidderDids[0] === "did:plc:4vcig7s43z3nyqrnepmcfzqt", "default bidder");
  assert(JSON.stringify(m.spec.tunnels) === '[{"tag":"wootty","port":7681,"shell":{"envFile":"/etc/wootty/wootty.env"}}]', "tunnels");
  assert(m.spec.ownerDid === "did:plc:me" && m.spec.userData === undefined, "owner, no userData");
  assert(parseBidders("did:plc:a, did:plc:b junk").join() === "did:plc:a,did:plc:b", "bidders");
});

Deno.test("bundle delivers the wootty env file", () => {
  const entries = JSON.parse(secretsBundle("tok"));
  assert(entries.length === 1 && entries[0].path === "/etc/wootty/wootty.env", "path");
  assert(entries[0].value === "WOOTTY_AUTH_TOKEN=tok\n", "value");
});

Deno.test("shell link uses the wootty network and the token fragment", () => {
  const contract = {
    networks: [
      { tag: "other", address: "https://x.example" },
      { tag: "wootty", address: "https://0123456789abcdef.abcdefghijkl.opentunnel.xyz" },
    ],
  };
  assert(shellLink(contract, "a+b/c") === "https://0123456789abcdef.abcdefghijkl.opentunnel.xyz/#token=a%2Bb%2Fc", shellLink(contract, "a+b/c"));
  assert(shellLink({ networks: [] }, "t") === "", "no address yet");
  assert(shellLink({ networks: [{ tag: "wootty", address: "javascript:alert(1)" }] }, "t") === "", "non-https rejected");
  assert(shellLink(contract, "") === "", "no token");
});

Deno.test("contractManifest without a shell is a bare VM with optional ssh key and TTL", () => {
  const m = contractManifest({
    name: "vmshell-bare01", bidders: ["did:plc:x"], ownerDid: "", shell: false,
    sshKeys: ["ssh-ed25519 AAAA test"], ttlSeconds: 3600,
  });
  if (m.spec.tunnels || m.spec.secretsRef) throw new Error("bare VM must not carry the shell tunnel or secretsRef");
  if (m.spec.sshAuthorizedKeys?.[0] !== "ssh-ed25519 AAAA test") throw new Error("ssh key missing");
  if (m.spec.ttlSecondsAfterFinished !== 3600) throw new Error("ttl missing");
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
