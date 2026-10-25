// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import { DEFAULT_SSH_CONFIG, dropKnownHost, mergeKnownHosts, resolveHost, sshHosts, TEST_PEER_HOST } from "./ssh_cmd.js";

function assert(cond, message) {
  if (!cond) throw new Error(message);
}

const throws = (fn, re) => {
  try {
    fn();
  } catch (err) {
    assert(re.test(err.message), err.message);
    return;
  }
  throw new Error("expected an error");
};

Deno.test("seeded config routes vmshell-* through kcp-webrtc as root", () => {
  assert(/^Host vmshell-\* einfra-\*$/m.test(DEFAULT_SSH_CONFIG), "host block covers infra contract names");
  assert(/^\s+User root$/m.test(DEFAULT_SSH_CONFIG), "user");
  assert(/^\s+ProxyCommand kcp-webrtc %h %p$/m.test(DEFAULT_SSH_CONFIG), "proxy");
});

Deno.test("resolveHost: Held contracts and the test peer", () => {
  const state = {
    contracts: [
      { name: "vmshell-a", phase: "Held" },
      { name: "vmshell-b", phase: "Bidding" },
      { name: "vmshell-c", phase: "Held", release: true },
    ],
  };
  assert(resolveHost(state, "vmshell-a").contract.name === "vmshell-a", "held");
  throws(() => resolveHost(state, "vmshell-b"), /Bidding, not Held/);
  throws(() => resolveHost(state, "vmshell-c"), /released/);
  throws(() => resolveHost(state, "nope"), /no such ComputeContract/);
  throws(() => resolveHost(state, TEST_PEER_HOST), /no such/);
  assert(resolveHost(state, TEST_PEER_HOST, { testPeer: true }).kind === "testpeer", "test peer");
  assert(sshHosts(state).join() === "vmshell-a,vmshell-b", sshHosts(state).join());
  assert(sshHosts(state, { testPeer: true }).includes(TEST_PEER_HOST), "test peer completes");
});

Deno.test("mergeKnownHosts appends once and never rewrites", () => {
  const first = mergeKnownHosts("", "vmshell-a", ["ssh-ed25519 AAAA1"]);
  assert(first === "vmshell-a ssh-ed25519 AAAA1\n", first);
  const again = mergeKnownHosts(first, "vmshell-a", ["ssh-ed25519 BBBB2"]);
  assert(again === first, "a changed key is left for the client to reject");
  const other = mergeKnownHosts(first, "vmshell-b", ["ssh-ed25519 CCCC3", "ecdsa-sha2-nistp256 DDDD4"]);
  assert(other.split("\n").filter(Boolean).length === 3, other);
  assert(mergeKnownHosts("other,vmshell-a ssh-rsa X\n", "vmshell-a", ["ssh-ed25519 Y"]) === "other,vmshell-a ssh-rsa X\n", "comma host");
  const dropped = dropKnownHost("# c\nvmshell-a ssh-ed25519 A\nvmshell-b ssh-ed25519 B\n", "vmshell-a");
  assert(dropped === "# c\nvmshell-b ssh-ed25519 B\n", dropped);
});
