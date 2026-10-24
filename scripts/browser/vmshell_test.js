// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import {
  contractManifest,
  contractName,
  isActive,
  isReleasing,
  parseBidders,
} from "./vmshell.js";

function assert(cond, message) {
  if (!cond) throw new Error(message);
}

Deno.test("contract name is a DNS label", () => {
  assert(contractName(new Uint8Array([0, 1, 35, 36, 255, 10])) === "vmshell-ab9adk", contractName(new Uint8Array([0, 1, 35, 36, 255, 10])));
  assert(/^vmshell-[a-z0-9]{6}$/.test(contractName()), "random name");
});

Deno.test("manifest defaults to the webrtc ssh tunnel", () => {
  const m = contractManifest({ name: "vmshell-abc123", bidders: parseBidders(""), ownerDid: "did:plc:me" });
  assert(m.spec.bidderDids[0] === "did:plc:4vcig7s43z3nyqrnepmcfzqt", "default bidder");
  assert(JSON.stringify(m.spec.tunnels) === '[{"tag":"ssh","port":22,"transport":"webrtc"}]', "tunnels");
  assert(m.spec.secretsRef === undefined, "no secretsRef");
  assert(m.spec.ownerDid === "did:plc:me" && m.spec.userData === undefined, "owner, no userData");
  assert(parseBidders("did:plc:a, did:plc:b junk").join() === "did:plc:a,did:plc:b", "bidders");
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

Deno.test("contractManifest with a webrtc shell carries the ssh tunnel and no secretsRef", () => {
  const m = contractManifest({
    name: "vmshell-rtc001", bidders: ["did:plc:x"], ownerDid: "", shell: "webrtc", sshKeys: ["ssh-ed25519 AAAA page"],
  });
  if (JSON.stringify(m.spec.tunnels) !== '[{"tag":"ssh","port":22,"transport":"webrtc"}]') throw new Error("tunnels");
  if (m.spec.secretsRef || m.spec.sshAuthorizedKeys?.[0] !== "ssh-ed25519 AAAA page") throw new Error("keys/secrets");
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
