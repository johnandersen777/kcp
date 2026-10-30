// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// Pure helpers for the page's Compute panel: contract names, phases and the
// ComputeContract manifest. A VM gets both shells: ssh over WebRTC (plan
// 0034; the page's key in sshAuthorizedKeys) and, when the served schema
// still has spec.tunnels[].shell, the wootty web terminal behind opentunnel
// (plan 0032; a browser-made token in a vault bundle the guest fetches
// through secretsRef). The webrtc shell is up ~2 s after Held; the wootty
// address takes 70-165 s, waiting on its TLS certificate. With the agent
// (plan 0035, spec.agent) the bundle also holds an ATIRC_TOKEN placeholder
// the requester fills with a minted atirc token; the guest starts the agent
// on an events.guest.agentStart ContractEvent (the Agent button).

export const WOOTTY_TAG = "wootty";
export const WOOTTY_PORT = 7681;
export const WOOTTY_ENV = "/etc/wootty/wootty.env";
export const ATIRC_ENV = "/etc/atirc/atirc.env";
export const SSH_TAG = "ssh";
export const SECRETS_MOUNT = "requester";
// The requester policy on every page contract, as the CLI's `run request`
// defaults contract.policy (socialweb requester.DefaultPolicyWorkflow); the
// PolicyEngine runs it in the fenced browser sandbox (plan 0037).
export const REQUESTER_POLICY = "requester-tangled-vouch";

/** The per-tab EphemeralInfra record lives long enough to outlast a page session. */
export const ORIGIN_INFRA_TTL_SECONDS = 30 * 24 * 3600;

const INFRA_RECORD_CAP = 64;

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

export function randomTabId(bytes = crypto.getRandomValues(new Uint8Array(6))) {
  const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789";
  return Array.from(bytes, (b) => alphabet[b % alphabet.length]).join("");
}

/** FNV-1a 64-bit, 16 lowercase hex chars. Synchronous and secure-context free. */
export function fnv1a64(text) {
  let hash = 0xcbf29ce484222325n;
  for (const byte of new TextEncoder().encode(String(text))) {
    hash = ((hash ^ BigInt(byte)) * 0x100000001b3n) & 0xffffffffffffffffn;
  }
  return hash.toString(16).padStart(16, "0");
}

/**
 * The EphemeralInfra object name, which is also the atproto record rkey. One
 * record per tab: the origin and a stable per-tab id, with the origin hash so
 * scheme/port changes cannot collide. Kept short enough that
 * einfra-<name>-vmshell-xxxxxx stays a DNS label.
 */
export function originInfraName(origin, tabId) {
  const raw = String(origin ?? "").trim().toLowerCase();
  let host = raw;
  try {
    host = new URL(raw).host;
  } catch {
    // Not an absolute origin: sanitize the raw value below.
  }
  const slug = host.replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 12).replace(/-+$/g, "") || "origin";
  const tab = String(tabId ?? "").toLowerCase().replace(/[^a-z0-9]/g, "").slice(0, 6) || "tab";
  return `web-${slug}-${fnv1a64(raw)}-${tab}`;
}

/** Pure JS mirror of internal/contract/atprotorepo.RepoName. */
export function repoName(did) {
  let name = ("repo-" + String(did ?? "")).toLowerCase()
    .replace(/[^a-z0-9.-]+/g, "-")
    .replace(/-+/g, "-")
    .replace(/^[-.]+|[-.]+$/g, "");
  if (!name) name = "repo";
  return name.slice(0, 253).replace(/[-.]+$/g, "");
}

export function infraRepoRef(did) {
  return { group: "socialweb.computer", kind: "AtprotoRepo", name: repoName(did) };
}

export function infraCondition(infra, type) {
  return (infra?.status?.conditions ?? []).find((condition) => condition?.type === type) ?? null;
}

export function recordSynced(infra) {
  return infraCondition(infra, "Synced")?.status === "True";
}

/** The record sync failure to show, if the record is invalid or unavailable. */
export function recordSyncError(infra) {
  const valid = infraCondition(infra, "RecordValid");
  if (valid?.status === "False") return valid.message || "the EphemeralInfra record is invalid";
  const synced = infraCondition(infra, "Synced");
  if (synced?.status === "False" && (synced.reason === "RecordUnavailable" || synced.reason === "RecordInvalid")) {
    return synced.message || "the EphemeralInfra record is unavailable";
  }
  return "";
}

export function infraRecordURI(infra, did, name) {
  if (!did || !name) return "";
  const uri = `at://${did}/computer.socialweb.ephemeralInfra/${name}`;
  return infra?.status?.record?.uri === uri ? uri : "";
}

/**
 * Replace or append a contract, pruning the oldest released entries when the
 * record is full. Never drops an active entry. Returns the dropped names so
 * the caller can remove their secrets.
 */
export function addContract(contracts, entry, cap = INFRA_RECORD_CAP) {
  const list = Array.isArray(contracts) ? contracts.slice() : [];
  const index = list.findIndex((contract) => contract?.name === entry.name);
  if (index >= 0) list[index] = entry;
  else list.push(entry);
  const dropped = [];
  while (list.length > cap) {
    const oldest = list.findIndex((contract) => contract?.state === "Released");
    if (oldest < 0) throw new Error(`the EphemeralInfra record is full (${cap} contracts); release a VM first`);
    dropped.push(list[oldest].name);
    list.splice(oldest, 1);
  }
  return { contracts: list, dropped };
}

/** Requested infra entries whose child ComputeContract has already finished. */
export function finishedInfraContracts(infra, contracts) {
  const name = infra?.metadata?.name;
  if (!name) return [];
  const terminal = new Set((contracts ?? [])
    .filter((contract) => contract?.phase === "Released" || contract?.phase === "Failed")
    .map((contract) => contract.name));
  return (infra?.spec?.contracts ?? []).filter((entry) =>
    entry?.state === "Requested" && terminal.has(infraContractName(name, entry.name)));
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

/**
 * The secrets bundle field: JSON text of [{path, value, mode?}]. The agent
 * entry is an empty ATIRC_TOKEN the requester fills in after the RFP; it also
 * keeps an agent-only bundle non-empty (both consumers reject an empty one).
 */
export function secretsBundle({ woottyToken, agent = false }) {
  return JSON.stringify([
    ...(woottyToken ? [{ path: WOOTTY_ENV, value: `WOOTTY_AUTH_TOKEN=${woottyToken}\n` }] : []),
    ...(agent ? [{ path: ATIRC_ENV, value: "ATIRC_TOKEN=\n", mode: "0600" }] : []),
  ]);
}

/**
 * The page tab's EphemeralInfra: every VM this tab requests is a contract in
 * it, so the requester only tracks this tab's contracts and deleting (or the
 * deadline of) the infra releases and collects all of them. Deadline, not
 * Complete: a tab outlives its first released VM. record binds the object to
 * the tab's atproto record; without it the infra is local to browser kcp.
 */
export function sessionInfraManifest(name, ttlSeconds = ORIGIN_INFRA_TTL_SECONDS, record = null) {
  const spec = { description: "socialweb.computer page session", ttlSeconds, endWhen: "Deadline", contracts: [] };
  if (record) spec.record = record;
  return {
    apiVersion: "socialweb.computer/v1alpha1",
    kind: "EphemeralInfra",
    metadata: { name },
    spec,
  };
}

/** The ComputeContract the infra controller creates for contract short in infra. */
export function infraContractName(infra, short) {
  return `einfra-${infra}-${short}`;
}

/**
 * One EphemeralInfra contract entry (and its secrets entry, with wootty or
 * the agent) for the VM short; cc is the ComputeContract name it becomes.
 * The template is the one contractManifest builds.
 */
export function infraContract({ infra, short, wootty = false, agent = false, sshKeys = [] }) {
  const cc = infraContractName(infra, short);
  const { spec } = contractManifest({ name: cc, wootty, agent, sshKeys });
  const { secretsRef, ...template } = spec;
  return {
    cc,
    contract: { name: short, state: "Requested", ...template },
    secret: secretsRef ? { contract: short, secretsRef } : null,
  };
}

/**
 * A ComputeContract for the page: the webrtc ssh tunnel (sshKeys must hold
 * the page's key), with wootty the opentunnel wootty tunnel, with agent the
 * atirc agent, and with either the secretsRef of their bundle.
 */
export function contractManifest({ name, ownerDid, wootty = false, agent = false, sshKeys = [] }) {
  // No bidderDids: the requester's discovery picks the bidders.
  const spec = { vmName: name, policy: REQUESTER_POLICY };
  spec.tunnels = [{ tag: SSH_TAG, port: 22, transport: "webrtc" }];
  if (wootty || agent) spec.secretsRef = { mount: SECRETS_MOUNT, path: secretsPath(name) };
  if (agent) spec.agent = { envFile: ATIRC_ENV };
  if (wootty) {
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

/** True when an APIResourceSchema's ComputeContract has spec.agent (plan 0035). */
export function schemaHasAgent(schema) {
  return (schema?.spec?.versions ?? []).some((v) => v.schema?.properties?.spec?.properties?.agent !== undefined);
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
