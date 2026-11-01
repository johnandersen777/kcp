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
export const GUEST_TOOL = "socialweb-computer";
export const R2_ASSOCIATES_ITSELF = "R2 associates itself; leave Associate DID empty";

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

// The EventSources the page's compose runs: its MarketRequester has no relay
// (createRoot), so socialweb's watchSources are the public jetstreams, and
// the MarketRequester's watch selector links grant subscribing to them.
export const INFRA_EVENT_SOURCES = ["jetstream-bsky-us-east", "jetstream-bsky-us-west"];

/**
 * The tab infra's spec.record: synced both ways with the signed-in repo and
 * watched through the jetstream EventSources (without eventSourceRefs it takes
 * a per-watch PDS source no link grants: "bus: link denied").
 */
export function infraRecordBinding(did) {
  return {
    repoRef: infraRepoRef(did),
    sync: "Both",
    eventSourceRefs: INFRA_EVENT_SOURCES.map((name) => ({ group: "events.socialweb.computer", kind: "EventSource", name })),
  };
}

/** True when a live spec.record needs patching to infraRecordBinding. */
export function infraRecordStale(binding) {
  return !binding?.repoRef || (binding.sync && binding.sync !== "Both") || !(binding.eventSourceRefs?.length > 0);
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
 *
 * The always-run rule (owner rule): an active entry without
 * REQUESTER_POLICY is refused, and every other active entry in the record
 * (one an older page or the synced PDS record left without it, or with
 * another policy) is written back with it.
 */
export function addContract(contracts, entry, cap = INFRA_RECORD_CAP) {
  if (entry?.state !== "Released" && entry?.policy !== REQUESTER_POLICY) {
    throw new Error(`refusing contract ${entry?.name}: its policy is ${JSON.stringify(entry?.policy ?? "")}, not ${REQUESTER_POLICY}`);
  }
  const list = (Array.isArray(contracts) ? contracts : []).map((contract) =>
    contract?.state === "Released" || contract?.policy === REQUESTER_POLICY ? contract : { ...contract, policy: REQUESTER_POLICY });
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
  return (Boolean(contract?.release) && !FINISHED.has(contract?.phase ?? "Pending")) || releasePending(contract);
}

/**
 * Release work this tab's requester still runs for the contract: a release in
 * progress (isReleasing), atirc token revokes Pending, or the A8 cascade
 * waiting for the guest requester. Closing the tab then would stall it.
 */
export function releaseWorkPending(contract) {
  return isReleasing(contract) || contract?.atircRevokes === "Pending" || contract?.step === "releasingGuestRequester";
}

// Release requests (socialweb r128 10558978, ephemeralinfras 11): the
// composer is the only writer of a ComputeContract's spec, so the page does
// not write a contract an EphemeralInfra owns. Every Release (the button,
// Release again, d.release and so the demo's leftover sweep) of such a
// contract sets the EphemeralInfra entry to
// {state: Released, releaseRequestedAt: now}; the infra controller copies
// spec.release and the stamp onto the ComputeContract (manager
// socialweb.computer/ephemeralinfra) and re-asks a Failed one for a newer
// stamp. The requester copies the request it answered into
// status.release.requestedAt (the feed's releaseAnsweredAt), and a newer
// request restarts a Failed contract whose ReleaseFailed condition is True.
//
// The page is its EphemeralInfra's composer (linux-kcp, rc128 option A;
// ephemeralinfras 12 makes spec.contracts a list map keyed by name). Every
// infra write (create, add an entry, a record rebind, Release,
// markFinished) is one server-side apply of the page's FULL desired spec
// (INFRA_PAGE_FIELDS: every entry with every field, the secrets, the record
// binding, description, ttl and endWhen) under INFRA_FIELD_MANAGER with
// force, built from a fresh read and re-applied whole (composeInfra). An
// apply that omits something its manager applied before removes it, so a
// partial apply would drop entries or fields; the full spec never does. The
// first apply takes over the entries earlier pages wrote by merge patch
// (Go-http-client, Update). The apply carries the resourceVersion it read,
// so a concurrent writer is a 409 and a fresh read, never a lost entry or
// stamp. All of it runs in the page's infra write queue.
//
// A contract with no ownerReferences at all (rc127 leftovers, `run request`
// from the terminal) has no composer to ask, so the page releases it as
// rc127 did (linux-kcp, rc128): a server-side apply of exactly
// {spec.release, spec.releaseRequestedAt} under
// socialweb.computer/requester-release, force, the manager socialweb's own
// release intent used for these two fields. A contract owned by anything
// else is refused.
export const RELEASE_FIELD_MANAGER = "socialweb.computer/requester-release";

/** The field manager of every page write of its EphemeralInfra. */
export const INFRA_FIELD_MANAGER = "socialweb.computer/page";

/** The EphemeralInfra spec fields the page composes (sessionInfraManifest, addContract, the secrets). */
export const INFRA_PAGE_FIELDS = ["description", "ttlSeconds", "endWhen", "record", "contracts", "secrets"];

/** The page's part of an EphemeralInfra spec: INFRA_PAGE_FIELDS that are set, deep-copied. */
export function pageInfraSpec(spec) {
  const out = {};
  for (const field of INFRA_PAGE_FIELDS) {
    if (spec?.[field] !== undefined && spec[field] !== null) out[field] = structuredClone(spec[field]);
  }
  return out;
}

/** The server-side apply of the page's full EphemeralInfra spec (resourceVersion: the one read, "" on create). */
export function infraApply(api, name, spec, resourceVersion = "") {
  const metadata = { name };
  if (resourceVersion) metadata.resourceVersion = resourceVersion;
  return {
    path: `${api}/ephemeralinfras/${name}?fieldManager=${encodeURIComponent(INFRA_FIELD_MANAGER)}&force=true`,
    contentType: "application/apply-patch+yaml",
    body: { apiVersion: "socialweb.computer/v1alpha1", kind: "EphemeralInfra", metadata, spec },
  };
}

/** spec.releaseRequestedAt for a Release now: strictly after previous, so a second Release always differs. */
export function releaseRequestedAt(previous = "", now = new Date()) {
  const before = Date.parse(previous ?? "");
  const at = Number.isNaN(before) ? now.getTime() : Math.max(now.getTime(), before + 1);
  return new Date(at).toISOString();
}

/** The server-side apply that releases ownerless ComputeContract name. */
export function releaseApply(api, name, requestedAt) {
  return {
    path: `${api}/computecontracts/${name}?fieldManager=${encodeURIComponent(RELEASE_FIELD_MANAGER)}&force=true`,
    contentType: "application/apply-patch+yaml",
    body: {
      apiVersion: "socialweb.computer/v1alpha1",
      kind: "ComputeContract",
      metadata: { name },
      spec: { release: true, releaseRequestedAt: requestedAt },
    },
  };
}

/** The later of RFC 3339 stamps ("" when none parses). */
export function latestStamp(...stamps) {
  let best = "";
  for (const stamp of stamps) {
    if (!Number.isNaN(Date.parse(stamp ?? "")) && (!best || Date.parse(stamp) > Date.parse(best))) best = stamp;
  }
  return best;
}

/** a is a stamp later than b (any parsable a is later than a missing or unparsable b). */
export function stampAfter(a, b) {
  const x = Date.parse(a ?? "");
  if (Number.isNaN(x)) return false;
  const y = Date.parse(b ?? "");
  return Number.isNaN(y) || x > y;
}

/** The infra entry for a Release: Released with releaseRequestedAt strictly after any stored request. */
export function releasedEntry(entry, ccRequestedAt = "", now = new Date()) {
  return { ...entry, state: "Released", releaseRequestedAt: releaseRequestedAt(latestStamp(entry?.releaseRequestedAt, ccRequestedAt), now) };
}

/**
 * markFinishedInfraContracts' update: a Requested entry becomes Released
 * like a Release does (releasedEntry: a fresh releaseRequestedAt strictly
 * after the entry's and the contract's, ccRequestedAt), so the infra
 * controller re-asks a Failed contract whose guest may still run; every
 * other field stays. null for anything else, so an entry a Release already
 * wrote is not written again.
 */
export function finishedEntry(entry, ccRequestedAt = "", now = new Date()) {
  return entry?.state === "Requested" ? releasedEntry(entry, ccRequestedAt, now) : null;
}

const INFRA_WRITE_ATTEMPTS = 3;

/**
 * Creates EphemeralInfra name with spec only if it is absent: a POST under
 * INFRA_FIELD_MANAGER. A 409 means it exists: it is read and the page's
 * full spec (what is there, spec's fields filling the gaps) applied with
 * the resourceVersion read, so a create never applies contracts: [] over
 * existing entries. Returns the object.
 */
export async function createInfra({ rest, api, name, spec }) {
  const manifest = { apiVersion: "socialweb.computer/v1alpha1", kind: "EphemeralInfra", metadata: { name }, spec };
  try {
    return await rest("POST", `${api}/ephemeralinfras?fieldManager=${encodeURIComponent(INFRA_FIELD_MANAGER)}`, manifest);
  } catch (err) {
    if (Number(err?.status ?? 0) !== 409) throw err;
  }
  return composeInfra({ rest, api, infra: name, update: (current) => ({ ...structuredClone(spec), ...current }) });
}

/**
 * Update-operation managedFields entries that co-own the page's spec fields:
 * the merge patches of earlier pages (Go-http-client) and the create POST
 * (INFRA_FIELD_MANAGER, operation Update). While one of them co-owns an
 * entry, the page's apply cannot remove it: a pruned secret or entry would
 * stay. Seen live on a migrated infra: Go-http-client owned every
 * spec.secrets key (a list map since before rev 12) and kept a pruned one.
 */
export function staleSpecOwners(obj) {
  return (obj?.metadata?.managedFields ?? []).filter((m) => m?.operation === "Update" &&
    (m.manager === "Go-http-client" || m.manager === INFRA_FIELD_MANAGER) &&
    INFRA_PAGE_FIELDS.some((field) => m.fieldsV1?.["f:spec"]?.[`f:${field}`] !== undefined));
}

/** The JSON patch that drops stale managedFields entries from obj, guarded by its resourceVersion. */
export function dropOwnersPatch(obj, stale) {
  const kept = (obj?.metadata?.managedFields ?? []).filter((m) => !stale.includes(m));
  return [
    { op: "test", path: "/metadata/resourceVersion", value: obj?.metadata?.resourceVersion },
    { op: "replace", path: "/metadata/managedFields", value: kept },
  ];
}

/**
 * One composer write of EphemeralInfra infra: reads it, update(spec, live)
 * returns the page's full desired spec from the fresh page spec (null: no
 * write), and that is applied whole (infraApply) with the resourceVersion
 * read (409: read again, 3 attempts). Returns the object kcp answers, or
 * null without a write.
 *
 * An infra other writers still co-own (staleSpecOwners) is first taken
 * over: the page's spec as read is applied unchanged, so the page owns all
 * of it, and the stale entries are dropped from managedFields; only then
 * the update is applied, so whatever it prunes really goes.
 */
export async function composeInfra({ rest, api, infra, update }) {
  const path = `${api}/ephemeralinfras/${infra}`;
  let tookOver = false;
  for (let attempt = 1; ; attempt++) {
    const live = await rest("GET", path);
    try {
      if (!tookOver && staleSpecOwners(live).length > 0) {
        tookOver = true;
        const take = infraApply(api, infra, pageInfraSpec(live?.spec), live?.metadata?.resourceVersion ?? "");
        const owned = await rest("PATCH", take.path, take.body, take.contentType);
        try {
          await rest("PATCH", `${path}?fieldManager=${encodeURIComponent(INFRA_FIELD_MANAGER)}`,
            dropOwnersPatch(owned, staleSpecOwners(owned)), "application/json-patch+json");
        } catch (err) {
          // 409/422 (the test op): someone wrote meanwhile; read again and take over again
          if (![409, 422].includes(Number(err?.status ?? 0))) throw err;
          tookOver = false;
          if (attempt >= INFRA_WRITE_ATTEMPTS) throw err;
          continue;
        }
        attempt--;
        continue;
      }
      const next = update(pageInfraSpec(live?.spec), live);
      if (!next) return null;
      const apply = infraApply(api, infra, next, live?.metadata?.resourceVersion ?? "");
      return await rest("PATCH", apply.path, apply.body, apply.contentType);
    } catch (err) {
      if (Number(err?.status ?? 0) !== 409 || attempt >= INFRA_WRITE_ATTEMPTS) throw err;
      tookOver = false;
    }
  }
}

/**
 * One spec.contracts entry of EphemeralInfra infra through composeInfra:
 * update(current) returns the new entry (null: no write); every other entry
 * and spec field is applied as read. Returns the entry written, or null.
 */
export async function updateInfraEntry({ rest, api, infra, match, update, what = "the contract" }) {
  let written = null;
  await composeInfra({
    rest,
    api,
    infra,
    update: (spec) => {
      const contracts = spec.contracts ?? [];
      const current = contracts.find(match);
      if (!current) throw new Error(`EphemeralInfra ${infra} has no entry for ${what}`);
      written = update(current);
      if (!written) return null;
      return { ...spec, contracts: contracts.map((entry) => entry === current ? written : entry) };
    },
  });
  return written;
}

/**
 * Request VM's update: contract entry added (or replaced) with addContract,
 * and its secrets entry (secret, or null) set; the secrets of entries
 * addContract pruned go too.
 */
export function withInfraContract(spec, entry, secret = null) {
  const { contracts, dropped } = addContract(spec?.contracts ?? [], entry);
  const next = { ...spec, contracts };
  if (secret) {
    next.secrets = [
      ...(spec?.secrets ?? []).filter((x) => !dropped.includes(x.contract) && x.contract !== secret.contract),
      secret,
    ];
  } else if (dropped.length > 0) {
    next.secrets = (spec?.secrets ?? []).filter((x) => !dropped.includes(x.contract));
  }
  return next;
}

/**
 * Releases ComputeContract name. Owned by an EphemeralInfra: through its
 * entry (state Released with a fresh releaseRequestedAt, strictly after the
 * entry's and the contract's), no write to the contract. No owner at all:
 * the rc127 apply of spec.release + a fresh releaseRequestedAt on the
 * contract (releaseApply), no infra write. Any other owner: refused.
 * rest(method, path, body, contentType) is kcpREST; serialize(run) runs run
 * in the page's infra write queue. Returns the releaseRequestedAt sent.
 */
export async function requestRelease({ rest, api, name, serialize = (run) => run(), now = new Date() }) {
  const cc = await rest("GET", `${api}/computecontracts/${name}`);
  if (!cc) throw new Error(`ComputeContract ${name} not found`);
  const owners = cc.metadata?.ownerReferences ?? [];
  if (owners.length === 0) {
    const requestedAt = releaseRequestedAt(cc.spec?.releaseRequestedAt, now);
    const apply = releaseApply(api, name, requestedAt);
    await rest("PATCH", apply.path, apply.body, apply.contentType);
    return requestedAt;
  }
  const owner = owners.find((o) => o.kind === "EphemeralInfra");
  if (!owner?.name) {
    const kinds = owners.map((o) => `${o.kind ?? "?"}/${o.name ?? "?"}`).join(", ");
    throw new Error(`ComputeContract ${name} is owned by ${kinds}, not an EphemeralInfra; its composer releases it, not the page`);
  }
  const short = cc.metadata?.labels?.["infra.socialweb.computer/contract"];
  const entry = await serialize(() => updateInfraEntry({
    rest,
    api,
    infra: owner.name,
    match: (c) => Boolean(c?.name) && (c.name === short || infraContractName(owner.name, c.name) === name),
    update: (current) => releasedEntry(current, cc.spec?.releaseRequestedAt, now),
    what: name,
  }));
  return entry.releaseRequestedAt;
}

/**
 * The contract as this tab sees it: a Release this tab sent (sentAt) that the
 * contract does not show yet (the infra controller copies the stamp a moment
 * later) counts as spec.release with that releaseRequestedAt, so the row says
 * release requested instead of the earlier outcome.
 */
export function withReleaseSent(contract, sentAt = "") {
  if (!contract || contract.phase === "Released" || !stampAfter(sentAt, contract.releaseRequestedAt)) return contract;
  return { ...contract, release: true, releaseRequestedAt: sentAt };
}

/** status.release answers the current Release (a contract without releaseRequestedAt predates rc127: always). */
export function releaseAnswered(contract) {
  const requested = contract?.releaseRequestedAt ?? "";
  return !requested || (contract?.releaseAnsweredAt ?? "") === requested;
}

/**
 * The requester's guestMayRun from the feed: an ok receipt, no termination,
 * no release the bidder acknowledged (receiptOk, terminated, releaseOk).
 */
export function guestMayRun(contract) {
  return Boolean(contract?.receiptOk) && !contract?.terminated && !contract?.releaseOk;
}

/**
 * A Failed contract with a newer, unanswered Release that the requester
 * restarts (r128 388ccee6, releaseRequestedAgain): its ReleaseFailed is
 * True, or its guest may still run. Pending, not Failed.
 */
export function releasePending(contract) {
  return Boolean(contract?.release) && contract?.phase === "Failed" &&
    (Boolean(contract?.releaseFailed) || guestMayRun(contract)) && !releaseAnswered(contract);
}

/**
 * The Release again button: a failed release whose outcome answers the
 * latest request, and nothing newer sent from this tab (sentAt).
 */
export function canReleaseAgain(contract, sentAt = "") {
  return Boolean(contract?.release) && contract?.phase === "Failed" && Boolean(contract?.releaseFailed) &&
    releaseAnswered(contract) && !stampAfter(sentAt, contract?.releaseRequestedAt);
}

/**
 * The requester gave up at its release bound (r128 e2a117e8: ReleaseFailed
 * True, reason ReleaseBound) and that answers the latest request.
 */
export function releaseTimedOut(contract) {
  return Boolean(contract?.release) && contract?.phase === "Failed" && Boolean(contract?.releaseFailed) &&
    contract?.releaseFailedReason === "ReleaseBound" && releaseAnswered(contract);
}

/**
 * The Release button: an active contract, or a Failed one the requester
 * re-releases for a newer stamp (r128 388ccee6): phase Failed, a valid
 * receipt (receiptOk), not terminated and not released/acked (releaseOk),
 * with no request pending (the Release sends the newer stamp) and no
 * Release again offered instead. ReleaseFailed is not needed.
 */
export function releaseOffered(contract) {
  if (isActive(contract)) return true;
  return contract?.phase === "Failed" && guestMayRun(contract) && releaseAnswered(contract) && !canReleaseAgain(contract);
}

/** The row's phase: a pending re-release shows as Releasing, not the earlier Failed. */
export function phaseLabel(contract) {
  const phase = contract?.phase ?? "Pending";
  if (releasePending(contract)) return { phase: "Releasing", text: ` Releasing · release requested ${contract.releaseRequestedAt}` };
  if (releaseTimedOut(contract)) return { phase, text: ` ${phase} · release timed out` };
  return { phase, text: ` ${phase}${contract?.release && phase !== "Released" && phase !== "Failed" ? " · releasing" : ""}` };
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

/**
 * The Associate DID input: "" (no association) or a did:plc:/did:web: DID
 * other than the signed-in identity's own (the requester refuses those too).
 */
export function associationSubject(text, selfDid = "") {
  const subject = String(text ?? "").trim();
  if (!subject) return { subject: "" };
  if (!/^did:(plc|web):[A-Za-z0-9._:%-]+$/.test(subject)) return { error: "Associate DID: enter a did:plc: or did:web: DID" };
  if (subject === selfDid) return { error: "Associate DID: that is your own DID" };
  return { subject };
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
export function infraContract({ infra, short, wootty = false, agent = false, sshKeys = [], association = "", guestTools = false }) {
  const cc = infraContractName(infra, short);
  const { spec } = contractManifest({ name: cc, wootty, agent, sshKeys, association, guestTools });
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
 * atirc agent, and with either the secretsRef of their bundle. association
 * (a DID, plan 0036 P1) has the requester write a contract-scoped
 * market.association for it; its ref file reaches the guest through the
 * secrets bundle, so it needs the secretsRef too.
 */
export function contractManifest({ name, ownerDid, wootty = false, agent = false, sshKeys = [], association = "", guestTools = false }) {
  // No bidderDids: the requester's discovery picks the bidders.
  const spec = { vmName: name, policy: REQUESTER_POLICY };
  spec.tunnels = [{ tag: SSH_TAG, port: 22, transport: "webrtc" }];
  if (association && guestTools) throw new Error(R2_ASSOCIATES_ITSELF);
  if (wootty || agent || association || guestTools) spec.secretsRef = { mount: SECRETS_MOUNT, path: secretsPath(name) };
  if (association) spec.association = { subject: association };
  // R2: socialweb-computer in the guest (linux/amd64 bidders only; others
  // fail their bid with GuestToolsUnserved). It associates itself: with
  // fromGuest the requester writes association-request.json into the bundle.
  if (guestTools) {
    spec.guestTools = [GUEST_TOOL];
    spec.association = { fromGuest: true };
  }
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
 * Release notes for the contract row (plan 0036 A8). The cascade runs in the
 * requester: it publishes guest.r2Release (outbound ContractEvent
 * <contract>-r2-release), waits up to R2_RELEASE_MINUTES with
 * status.execution.step releasingGuestRequester for the guest's r2Released
 * (only the time left since requestedAt after a restart), then deletes the
 * VM. atircRevokes runs before Released.
 */
// socialweb cloudinit.GuestRequesterReleaseTimeout (201937b0): stop 2 min +
// release unit (3 + 15 + 2 + 1 min) + 1 min.
export const R2_RELEASE_MINUTES = 24;

export function releaseNotes(contract, contractEvents = [], now = Date.now()) {
  const notes = [];
  // status.contract.r2Release (rc127, socialweb 34f74335): phase
  // Requested|Released|Failed|TimedOut; ok is omitted when false, so the
  // phase decides.
  const r2 = contract?.r2Release;
  if (r2) {
    const why = r2.message || "no message";
    notes.push({
      Requested: `R2 release: asked ${r2.requestedAt ?? "?"}, waiting (up to ${R2_RELEASE_MINUTES} min${minutesLeft(r2.requestedAt, now)})`,
      Released: `R2 release: guest released ${r2.answeredAt ?? "?"}`,
      Failed: `R2 release: FAILED: ${why}`,
      TimedOut: `R2 release: TIMED OUT after ${R2_RELEASE_MINUTES} min; bidder backstop releases R2's VMs after the association expires`,
    }[r2.phase] ?? `R2 release: [${r2.phase ?? "unknown"}] ${why}`);
  }
  const asked = contractEvents.find((e) => e?.contract === contract?.name && e.direction === "outbound" &&
    e.nsid === "com.publicdomainrelay.temp.compute.events.guest.r2Release");
  if (r2) {
    // the answer above replaces the wait/request lines
  } else if (contract?.step === "releasingGuestRequester") {
    notes.push(`R2 release: waiting for the guest requester to release its contracts (up to ${R2_RELEASE_MINUTES} min)${asked ? `, request ${asked.phase ?? "Pending"}` : ""}`);
  } else if (asked) {
    notes.push(`R2 release: asked (${asked.phase ?? "Pending"}); answered or ${R2_RELEASE_MINUTES} min bound passed`);
  }
  const revokes = { Pending: "tokens: revoking", Done: "tokens: revoked", Abandoned: "tokens: revoke ABANDONED, revoke them by hand" };
  if (contract?.atircRevokes) notes.push(revokes[contract.atircRevokes] ?? `tokens: ${contract.atircRevokes}`);
  if (!releasePending(contract)) return notes;
  // The outcomes above answer an earlier Release; the requester restarts it.
  return [`release: requested ${contract.releaseRequestedAt}, pending (the earlier release failed${contract.message ? `: ${contract.message}` : ""})`,
    ...notes.map((note) => `${note} (earlier attempt)`)];
}

// ", ~N min left" for the requester's remaining wait, "" without a time.
function minutesLeft(requestedAt, now) {
  const at = Date.parse(requestedAt ?? "");
  if (Number.isNaN(at)) return "";
  return `, ~${Math.max(0, Math.ceil((at + R2_RELEASE_MINUTES * 60_000 - now) / 60_000))} min left`;
}

/** The Start R2 button: a Held contract that runs R2 and lets it associate itself. */
export function r2StartShown(contract) {
  return contract?.phase === "Held" && (contract.guestTools ?? []).includes(GUEST_TOOL) && contract.fromGuest === true &&
    !contract.release;
}

/** True when an APIResourceSchema's ComputeContract has spec.guestTools (R2). */
export function schemaHasGuestTools(schema) {
  return (schema?.spec?.versions ?? []).some((v) => v.schema?.properties?.spec?.properties?.guestTools !== undefined);
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

// --- other tabs' EphemeralInfras (linux-kcp, rc129) ---------------------------
//
// Every tab of an origin shares one kcp state, so infras of tabs that are gone
// stay in it until their ttl. On boot the page collects those of its own
// origin: an infra none of whose entries points at a live ComputeContract is
// deleted once it is history older than INFRA_HISTORY_MS (a new browser
// session is a new tab id, and deleting the infra cascades to its Released
// contracts, whose rows carry the "tokens: revoked" proof); one with a live
// contract keeps running and gets its record binding repaired as sessionInfra
// repairs the tab's own. An infra already ending (deletionTimestamp or
// status.endedAt: the infra controller expires it) is left to the controller.

/** How long another tab's finished infra stays as history rows (linux-kcp, rc130). */
export const INFRA_HISTORY_MS = 24 * 3600 * 1000;

/** The newest of the RFC 3339 times given, in ms (0 when none parses). */
export function newestTime(...times) {
  let newest = 0;
  for (const t of times) {
    const ms = typeof t === "string" && t ? Date.parse(t) : NaN;
    if (Number.isFinite(ms) && ms > newest) newest = ms;
  }
  return newest;
}

/**
 * When a ComputeContract last changed toward its end: the newest condition
 * transition, release request or answer, or guest termination. A terminal
 * contract has no separate end stamp; these are set on the way there.
 */
export function contractFinishedAt(obj) {
  const status = obj?.status ?? {};
  return newestTime(
    ...(status.conditions ?? []).map((c) => c?.lastTransitionTime),
    obj?.spec?.releaseRequestedAt,
    status.release?.requestedAt,
    status.contract?.termination?.observedAt,
  );
}

/** The feed's view (statusfeed contract()) of a ComputeContract object, for the liveness rule. */
export function feedContract(obj) {
  const status = obj?.status ?? {};
  const spec = obj?.spec ?? {};
  const failed = (status.conditions ?? []).find((c) => c?.type === "ReleaseFailed" && c?.status === "True");
  const guest = status.contract ?? {};
  return {
    name: obj?.metadata?.name ?? "",
    phase: status.phase ?? "",
    release: spec.release === true,
    releaseRequestedAt: spec.releaseRequestedAt ?? "",
    releaseAnsweredAt: status.release?.requestedAt ?? "",
    releaseFailed: Boolean(failed),
    releaseFailedReason: failed?.reason ?? "",
    atircRevokes: status.atircRevokes ?? "",
    step: status.execution?.step ?? "",
    receiptOk: guest.receipt !== undefined && guest.receiptOk === true,
    terminated: guest.termination !== undefined,
    releaseOk: guest.release?.ok === true,
    finishedAt: contractFinishedAt(obj),
  };
}

/**
 * A ComputeContract that may still hold a VM or release work: not Released
 * or Failed; Failed with the guest possibly running (the Release offer's
 * guestMayRun) or a failed release; or release work still pending
 * (releaseWorkPending: a re-release, atirc revokes, the guest requester).
 */
export function contractLive(contract) {
  if (!contract) return false;
  if (!FINISHED.has(contract.phase || "Pending")) return true;
  if (contract.phase === "Failed" && (guestMayRun(contract) || contract.releaseFailed)) return true;
  return releaseWorkPending(contract);
}

/** The tab id in name when it is an infra of origin (originInfraName), else null. */
export function originInfraTab(origin, name) {
  const prefix = originInfraName(origin, "x").slice(0, -1);
  const tab = String(name ?? "").startsWith(prefix) ? String(name).slice(prefix.length) : "";
  return /^[a-z0-9]{1,6}$/.test(tab) ? tab : null;
}

/**
 * What to do with each EphemeralInfra of origin besides tabId's own:
 * {name, action: "delete"|"rebind"|"keep"|"skip", reason, live}. Other
 * origins' infras and the tab's own are not listed. contracts are
 * feedContract views; did is the signed-in identity (rebind target).
 */
export function planInfraGC({ infras, contracts, origin, tabId, did, now = Date.now(), historyMs = INFRA_HISTORY_MS }) {
  const own = originInfraName(origin, tabId);
  const byName = new Map((contracts ?? []).map((c) => [c?.name, c]));
  const plan = [];
  for (const infra of infras ?? []) {
    const name = infra?.metadata?.name ?? "";
    if (!name || name === own || originInfraTab(origin, name) === null) continue;
    if (infra.metadata?.deletionTimestamp || infra.status?.endedAt) {
      plan.push({ name, action: "skip", reason: "already ending", live: [] });
      continue;
    }
    const live = (infra.spec?.contracts ?? [])
      .map((entry) => infraContractName(name, entry?.name))
      .filter((cc) => contractLive(byName.get(cc)));
    if (live.length === 0) {
      // The newest end among its contracts (and entries' release stamps),
      // else the infra's creation: history younger than historyMs stays.
      const entries = infra.spec?.contracts ?? [];
      const ended = newestTime(
        infra.status?.endedAt,
        ...entries.map((entry) => entry?.releaseRequestedAt),
        ...entries.map((entry) => {
          const c = byName.get(infraContractName(name, entry?.name));
          return c?.finishedAt ? new Date(c.finishedAt).toISOString() : "";
        }),
      ) || newestTime(infra.metadata?.creationTimestamp);
      const hours = ended ? ((now - ended) / 3600_000).toFixed(1) : "unknown";
      if (!ended || now - ended <= historyMs) {
        plan.push({ name, action: "keep", reason: `history: finished ${hours} h ago`, live });
      } else {
        plan.push({ name, action: "delete", reason: `no live ComputeContract; finished ${hours} h ago`, live });
      }
      continue;
    }
    const binding = infra.spec?.record;
    if (binding?.uri || (binding?.repoRef?.name && binding.repoRef.name !== repoName(did))) {
      plan.push({ name, action: "keep", reason: "live contracts; its record is bound to another repo", live });
    } else if (infraRecordStale(binding)) {
      plan.push({ name, action: "rebind", reason: "live contracts; stale record binding", live });
    } else {
      plan.push({ name, action: "keep", reason: "live contracts", live });
    }
  }
  return plan;
}
