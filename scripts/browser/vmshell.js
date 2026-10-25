// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// Pure helpers for the page's Compute panel: contract names, phases and the
// ComputeContract manifest. A VM gets both shells: ssh over WebRTC (plan
// 0034; the page's key in sshAuthorizedKeys) and, when the served schema
// still has spec.tunnels[].shell, the wootty web terminal behind opentunnel
// (plan 0032; a browser-made token in a vault bundle the guest fetches
// through secretsRef). The webrtc shell is up ~2 s after Held; the wootty
// address takes 70-165 s, waiting on its TLS certificate.

export const WOOTTY_TAG = "wootty";
export const WOOTTY_PORT = 7681;
export const WOOTTY_ENV = "/etc/wootty/wootty.env";
export const SSH_TAG = "ssh";
export const SECRETS_MOUNT = "requester";

export function base64url(bytes) {
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/** 24 random bytes, base64url without padding: 32 chars, wootty's minimum. */
export function randomToken() {
  return base64url(crypto.getRandomValues(new Uint8Array(24)));
}

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

/** Feed order with Released contracts below everything else. */
export function orderContracts(contracts) {
  return [...contracts].sort((a, b) => (a?.phase === "Released") - (b?.phase === "Released"));
}

export function secretsPath(name) {
  return `contracts/${name}/secrets`;
}

/** The secrets bundle field: JSON text of [{path, value}]. */
export function secretsBundle(token) {
  return JSON.stringify([{ path: WOOTTY_ENV, value: `WOOTTY_AUTH_TOKEN=${token}\n` }]);
}

/**
 * A ComputeContract for the page: the webrtc ssh tunnel (sshKeys must hold
 * the page's key) and, with wootty, the opentunnel wootty tunnel and the
 * secretsRef of its token bundle.
 */
export function contractManifest({ name, ownerDid, wootty = false, sshKeys = [] }) {
  // No bidderDids: the requester's discovery picks the bidders.
  const spec = { vmName: name };
  spec.tunnels = [{ tag: SSH_TAG, port: 22, transport: "webrtc" }];
  if (wootty) {
    spec.secretsRef = { mount: SECRETS_MOUNT, path: secretsPath(name) };
    spec.tunnels.unshift({ tag: WOOTTY_TAG, port: WOOTTY_PORT, transport: "opentunnel", shell: { envFile: WOOTTY_ENV } });
  }
  if (sshKeys.length > 0) spec.sshAuthorizedKeys = sshKeys;
  if (ownerDid) spec.ownerDid = ownerDid;
  return {
    apiVersion: "socialweb.computer/v1alpha1",
    kind: "ComputeContract",
    metadata: { name },
    spec,
  };
}

/** True when an APIResourceSchema's ComputeContract tunnels have the wootty shell field. */
export function schemaHasShell(schema) {
  return (schema?.spec?.versions ?? []).some((v) =>
    v.schema?.properties?.spec?.properties?.tunnels?.items?.properties?.shell !== undefined);
}

/**
 * The wootty link for a status-feed contract: the "wootty" tagged https
 * address from status.contract.networks with the token in the fragment (it
 * never reaches the tunnel or a server log), or "" while it is not reported.
 */
export function shellLink(contract, token) {
  const network = (contract?.networks ?? []).find((n) => n?.tag === WOOTTY_TAG);
  if (!network?.address || !token) return "";
  let url;
  try {
    url = new URL(network.address);
  } catch {
    return "";
  }
  if (url.protocol !== "https:") return "";
  url.hash = `token=${encodeURIComponent(token)}`;
  return url.toString();
}
