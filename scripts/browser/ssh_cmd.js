// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// Pure helpers for the shell's `ssh` command: the seeded ~/.ssh/config, host
// resolution (a host is a ComputeContract name) and known_hosts entries from
// status.contract.hostKeys. The config itself is parsed in Go
// (contrib/wasm/sshclient/sshconfig.go).

export const DEFAULT_SSH_CONFIG = `# Seeded by the kcp-wasm page. Hosts are ComputeContract names (a tab's
# EphemeralInfra names them einfra-<infra>-vmshell-...); kcp-webrtc is the
# built-in proxy that reaches the guest's sshd over WebRTC.
Host vmshell-* einfra-*
  User root
  ProxyCommand kcp-webrtc %h %p
`;

// Earlier seeds, replaced by DEFAULT_SSH_CONFIG when found unchanged.
export const OLD_SSH_CONFIGS = [`# Seeded by the kcp-wasm page. Hosts are ComputeContract names; kcp-webrtc
# is the built-in proxy that reaches the guest's sshd over WebRTC.
Host vmshell-*
  User root
  ProxyCommand kcp-webrtc %h %p
`];

// With ?sshTestPeer=, this name reaches contrib/wasm/sshclient/testpeer.
export const TEST_PEER_HOST = "vmshell-testpeer";

/**
 * What an ssh host names: the test peer, or an active ComputeContract that is
 * Held. Throws with an ssh-style reason otherwise.
 */
export function resolveHost(state, host, { testPeer = false } = {}) {
  if (testPeer && host === TEST_PEER_HOST) return { kind: "testpeer" };
  const contract = (state?.contracts ?? []).find((c) => c.name === host);
  if (!contract) throw new Error("no such ComputeContract");
  if (contract.release || ["Released", "Failed"].includes(contract.phase)) {
    throw new Error(`the contract is ${contract.release ? "released" : contract.phase}`);
  }
  if (contract.phase !== "Held") throw new Error(`the contract is ${contract.phase ?? "Pending"}, not Held yet`);
  return { kind: "contract", contract };
}

/** Active contract names for completion. */
export function sshHosts(state, { testPeer = false } = {}) {
  const names = (state?.contracts ?? [])
    .filter((c) => !c.release && !["Released", "Failed"].includes(c.phase))
    .map((c) => c.name);
  return testPeer ? [...names, TEST_PEER_HOST] : names;
}

/** known_hosts text without the lines that name host. */
export function dropKnownHost(text, host) {
  return String(text ?? "").split("\n")
    .filter((l) => l.startsWith("#") || !l.trim().split(/\s+/)[0].split(",").includes(host))
    .join("\n");
}

/**
 * known_hosts text with hostKeys for host appended when no line names the
 * host yet. Existing lines are never rewritten, so a changed key shows up as
 * a mismatch (OpenSSH's "REMOTE HOST IDENTIFICATION HAS CHANGED").
 */
export function mergeKnownHosts(text, host, hostKeys) {
  const lines = String(text ?? "").split("\n").filter((l) => l.trim() !== "");
  const named = lines.some((l) => !l.startsWith("#") && l.trim().split(/\s+/)[0].split(",").includes(host));
  if (!named) {
    for (const key of hostKeys ?? []) lines.push(`${host} ${key.trim()}`);
  }
  return lines.length > 0 ? `${lines.join("\n")}\n` : "";
}
