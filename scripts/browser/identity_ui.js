// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// The requester sign-in as one explicit state, derived from the
// MarketIdentity status feed plus what this page is doing, so the panel says
// where it is, as whom, and what the next tap does.

// How long a tap waits for the controller before its old status counts again.
export const GRACE_MS = 15_000;

export function normalizeHandle(value) {
  return String(value ?? "").trim().replace(/^@/, "").toLowerCase();
}

// A sign-in URL the next tap may open: unexpired, never opened before (a
// callback spends its state even when it fails), and asked for the handle in
// the field, since the URL carries the applied handle as its login hint.
export function signInURL(identity, local = {}, now = Date.now()) {
  if (normalizeHandle(local.typed) !== normalizeHandle(local.applied)) return "";
  const action = identity?.pendingAction;
  if (!action || (action.kind !== "visitUrl" && action.kind !== "signInAgain")) return "";
  const url = String(action.url ?? "");
  if (!url || (local.used ?? []).includes(url)) return "";
  if (action.expiresAt && Date.parse(action.expiresAt) <= now) return "";
  // Builds before rc110 keep an unexpired URL after the handle changes.
  let hint = null;
  try {
    hint = new URL(url).searchParams.get("login_hint");
  } catch {
    return "";
  }
  if (hint !== null && normalizeHandle(hint) !== normalizeHandle(local.applied)) return "";
  return url;
}

export function friendlyError(message) {
  const text = String(message ?? "").trim();
  const known = [
    [/state is unknown|state .*(not found|expired)|unknown state/i, "that sign-in link was already used or has expired"],
    [/popup was blocked/i, "the browser blocked the sign-in tab; tap Try again"],
    [/popup timed out/i, "no answer from the sign-in tab within 5 minutes"],
    [/access_denied/i, "the sign-in was declined"],
    // An entryway like bsky.social serves no PDS metadata; socialweb needs the handle.
    [/oauth-protected-resource returned HTTP 404/i, "type your handle (e.g. alice.bsky.social) and tap Try again"],
  ];
  for (const [pattern, friendly] of known) if (pattern.test(text)) return friendly;
  return text.replace(/^(marketidentity|oauthclient|socialweb): /, "").replace(/ (marketidentity|oauthclient): /g, " ") ||
    "unknown error";
}

function hostOf(url) {
  try {
    return new URL(url).host;
  } catch {
    return "your PDS";
  }
}

// local: {step: idle|preparing|authorizing|completing|signingOut, since,
// error, notice, typed, applied, used: [urls], account: {did, handle}}.
// The result names the primary and secondary buttons by action:
// start (apply the identity), open (the popup), switch, signOut, cancel.
export function signInView({ requester = true, identity, local = {}, now = Date.now() }) {
  const view = (state, tone, text, detail, primary, secondary = null) =>
    ({ state, tone, text, detail: (!primary.disabled && local.notice) || detail || "", primary, secondary, busy: Boolean(primary.disabled) });
  const wait = (label) => ({ label, action: "none", disabled: true });
  if (!requester) return view("starting", "warn", "Starting kcp…", "", wait("Sign in"));
  const phase = identity?.phase ?? "Pending";
  const url = signInURL(identity, local, now);
  const waiting = now - (local.since ?? 0) < GRACE_MS;
  const step = local.step ?? "idle";
  if (step === "authorizing") {
    return view("authorizing", "warn", `Approve the sign-in on ${hostOf(local.url)} in the other tab, then come back here.`,
      "Closed that tab without approving? Tap Cancel.", wait("Waiting for approval…"), { label: "Cancel", action: "cancel" });
  }
  if (step === "completing" && phase !== "Ready") {
    if (waiting) return view("completing", "warn", "Finishing sign-in…", "", wait("Finishing…"));
    return failed(identity?.message || "the sign-in did not finish");
  }
  if (step === "signingOut" && phase === "Ready" && waiting) {
    return view("signingOut", "warn", "Signing out…", "", wait("Signing out…"));
  }
  if (local.error) return failed(local.error);
  if (step === "preparing" && !url && (waiting || (phase !== "Failed" && phase !== "Ready"))) {
    const target = normalizeHandle(local.applied) || "bsky.social";
    if (waiting) return view("preparing", "warn", `Preparing sign-in for ${target}…`, "", wait("Preparing…"));
    return view("preparing", "warn", `Still waiting for a sign-in link for ${target}.`, identity?.message ?? "",
      { label: "Try again", action: "start" });
  }
  if (phase === "Ready" && identity?.did) {
    const did = identity.did;
    const handle = local.account?.did === did ? normalizeHandle(local.account.handle) : "";
    const typed = normalizeHandle(local.typed);
    const other = typed && typed !== handle && typed !== did;
    const primary = { label: other ? `Switch to ${typed}` : "Switch account", action: "switch" };
    const signOut = { label: "Sign out", action: "signOut" };
    const detail = handle ? did : "";
    if (other && handle) {
      return view("signedIn", "warn", `Signed in as ${handle}, not ${typed}.`, did, primary, signOut);
    }
    return view("signedIn", "ok", `Signed in as ${handle || did}`, detail, primary, signOut);
  }
  if (url) {
    const host = hostOf(url);
    if (step === "preparing") {
      return view("ready", "warn", `Sign-in link ready. Tap Continue to approve on ${host}.`, "",
        { label: "Continue sign-in", action: "open" });
    }
    if (phase === "SessionExpired") {
      return view("ready", "warn", "Your session expired.", identity?.message ?? "", { label: "Sign in again", action: "open" });
    }
    return view("signedOut", "", "Not signed in.", `Sign in opens ${host} in a new tab.`, { label: "Sign in", action: "open" });
  }
  // A fresh page boots with no handle and the controller fails on it; that
  // is not a failed sign-in until someone taps.
  if (phase === "Failed" && step === "preparing") return failed(identity?.message);
  return view("signedOut", "", "Not signed in.", "", { label: "Sign in", action: "start" });

  function failed(message) {
    // The controller reissues a URL after a failed callback; then one tap opens it.
    const retry = { label: "Try again", action: url ? "open" : "start" };
    return { ...view("failed", "err", `Sign-in failed: ${friendlyError(message)}`, "", retry), error: String(message ?? "") };
  }
}
