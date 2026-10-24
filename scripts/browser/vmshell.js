// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// Pure helpers for the page's "Request VM shell" flow: a ComputeContract with
// a plan 0032 shell tunnel (wootty behind opentunnel on the guest) and the
// secrets bundle that delivers the wootty token. The page writes the bundle
// into the browser vault before it applies the contract, because the
// requester fails a contract whose secretsRef is missing.

export const SHELL_TAG = "wootty";
export const SHELL_PORT = 7681;
export const WOOTTY_ENV = "/etc/wootty/wootty.env";
export const SECRETS_MOUNT = "requester";
// hat-9; the local bidder is did:plc:2l2wdry77c7jtucbslstagkj.
export const DEFAULT_BIDDERS = ["did:plc:4vcig7s43z3nyqrnepmcfzqt"];

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

export function secretsPath(name) {
  return `contracts/${name}/secrets`;
}

/** The secrets bundle field: JSON text of [{path, value}]. */
export function secretsBundle(token) {
  return JSON.stringify([{ path: WOOTTY_ENV, value: `WOOTTY_AUTH_TOKEN=${token}\n` }]);
}

export function parseBidders(text) {
  const dids = String(text ?? "").split(/[\s,]+/).filter((s) => s.startsWith("did:"));
  return dids.length > 0 ? dids : [...DEFAULT_BIDDERS];
}

export function contractManifest({ name, bidders, ownerDid }) {
  const spec = {
    vmName: name,
    bidderDids: bidders,
    secretsRef: { mount: SECRETS_MOUNT, path: secretsPath(name) },
    tunnels: [{ tag: SHELL_TAG, port: SHELL_PORT, shell: { envFile: WOOTTY_ENV } }],
  };
  if (ownerDid) spec.ownerDid = ownerDid;
  return {
    apiVersion: "socialweb.computer/v1alpha1",
    kind: "ComputeContract",
    metadata: { name },
    spec,
  };
}

/**
 * The wootty link for a status-feed contract: the "wootty" tagged address from
 * status.contract.networks with the token in the fragment, or "" while the
 * address has not been reported.
 */
export function shellLink(contract, token) {
  const network = (contract?.networks ?? []).find((n) => n?.tag === SHELL_TAG);
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
