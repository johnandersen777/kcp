// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// Pure helpers for the page's Compute panel: contract names, phases and the
// ComputeContract manifest (a bare VM, or one with the plan 0034 webrtc ssh
// tunnel).

// hat-9; the local bidder is did:plc:2l2wdry77c7jtucbslstagkj.
export const DEFAULT_BIDDERS = ["did:plc:4vcig7s43z3nyqrnepmcfzqt"];

export function contractName(bytes = crypto.getRandomValues(new Uint8Array(6))) {
  const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789";
  return "vmshell-" + Array.from(bytes, (b) => alphabet[b % alphabet.length]).join("");
}

const FINISHED = new Set(["Released", "Failed"]);

/** A contract that still holds (or is getting) a VM and is not being released. */
export function isActive(contract) {
  return !contract?.release && !FINISHED.has(contract?.phase ?? "Pending");
}

/** A contract whose release was asked for but has not finished. */
export function isReleasing(contract) {
  return Boolean(contract?.release) && !FINISHED.has(contract?.phase ?? "Pending");
}

export function parseBidders(text) {
  const dids = String(text ?? "").split(/[\s,]+/).filter((s) => s.startsWith("did:"));
  return dids.length > 0 ? dids : [...DEFAULT_BIDDERS];
}

/**
 * A ComputeContract for the page. shell "webrtc" (the default) carries the
 * plan 0034 ssh tunnel (sshKeys must hold the page's key); false or "none" is
 * a bare VM reached through sshKeys.
 */
export function contractManifest({ name, bidders, ownerDid, shell = "webrtc", sshKeys = [], ttlSeconds = null }) {
  const spec = { vmName: name, bidderDids: bidders };
  if (shell === "webrtc") spec.tunnels = [{ tag: "ssh", port: 22, transport: "webrtc" }];
  if (sshKeys.length > 0) spec.sshAuthorizedKeys = sshKeys;
  if (ttlSeconds !== null) spec.ttlSecondsAfterFinished = ttlSeconds;
  if (ownerDid) spec.ownerDid = ownerDid;
  return {
    apiVersion: "socialweb.computer/v1alpha1",
    kind: "ComputeContract",
    metadata: { name },
    spec,
  };
}
