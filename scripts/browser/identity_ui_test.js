// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import { friendlyError, GRACE_MS, signInURL, signInView } from "./identity_ui.js";

function assertEquals(actual, expected, label = "value") {
  const a = JSON.stringify(actual);
  const e = JSON.stringify(expected);
  if (a !== e) throw new Error(`${label}: expected ${e}, got ${a}`);
}

const now = Date.parse("2026-10-24T12:00:00Z");
const URL1 = "https://bsky.social/oauth/authorize?client_id=x&state=s1";
const awaiting = {
  phase: "AwaitingAuthorization",
  pendingAction: { kind: "visitUrl", url: URL1, expiresAt: "2026-10-24T12:05:00Z" },
};
const ALICE = "did:plc:lpfuqerea3deuoyrn7ojser4";
const ready = { phase: "Ready", did: ALICE, handle: "johnandersen777.bsky.social" };
const at = (local, identity = awaiting) => signInView({ identity, local, now });

Deno.test("signInURL refuses expired, opened, and other-handle URLs", () => {
  assertEquals(signInURL(awaiting, {}, now), URL1);
  assertEquals(signInURL(awaiting, { used: [URL1] }, now), "", "used");
  assertEquals(signInURL(awaiting, {}, Date.parse("2026-10-24T12:06:00Z")), "", "expired");
  assertEquals(signInURL(awaiting, { typed: "bob.test", applied: "alice.test" }, now), "", "handle changed");
  assertEquals(signInURL(awaiting, { typed: "@Alice.test ", applied: "alice.test" }, now), URL1, "normalized");
  assertEquals(signInURL({ phase: "Ready" }, {}, now), "");
});

Deno.test("signInView walks signed out to signed in", () => {
  assertEquals(signInView({ requester: false }).state, "starting");
  const out = at({}, { phase: "Pending" });
  assertEquals([out.state, out.primary.action, out.primary.disabled], ["signedOut", "start", undefined]);
  // A URL already issued opens on the first tap: iOS allows a popup only in the tap.
  assertEquals(at({}).primary.action, "open");
  const preparing = at({ step: "preparing", since: now - 1000, used: [URL1] });
  assertEquals([preparing.state, preparing.busy], ["preparing", true]);
  const continueTap = at({ step: "preparing", since: now - 1000 });
  assertEquals([continueTap.state, continueTap.primary.label], ["ready", "Continue sign-in"]);
  const authorizing = at({ step: "authorizing", url: URL1 });
  assertEquals([authorizing.state, authorizing.busy, authorizing.secondary.action], ["authorizing", true, "cancel"]);
  assertEquals(at({ step: "completing", since: now, used: [URL1] }).state, "completing");
  const done = at({ step: "completing", since: now, account: { did: ALICE, handle: "alice.bsky.social" } }, ready);
  assertEquals([done.state, done.text, done.secondary.action], ["signedIn", "Signed in as alice.bsky.social", "signOut"]);
});

Deno.test("signInView shows the session's account, not the typed handle", () => {
  const unresolved = at({ typed: "" }, ready);
  assertEquals(unresolved.text, `Signed in as ${ALICE}`, "falls back to the DID");
  const mismatch = at({ typed: "johnandersen777.bsky.social", account: { did: ALICE, handle: "alice.bsky.social" } }, ready);
  assertEquals([mismatch.tone, mismatch.text, mismatch.primary.label],
    ["warn", "Signed in as alice.bsky.social, not johnandersen777.bsky.social.", "Switch to johnandersen777.bsky.social"]);
  const stale = at({ account: { did: "did:plc:other", handle: "bob.test" } }, ready);
  assertEquals(stale.text, `Signed in as ${ALICE}`, "a handle resolved for another DID is ignored");
});

Deno.test("signInView waits out the old status after a tap, then trusts it", () => {
  const failedStatus = { phase: "Failed", message: "oauthclient: the oauth callback state is unknown" };
  assertEquals(at({ step: "preparing", since: now - 1000 }, failedStatus).state, "preparing");
  const late = at({ step: "preparing", since: now - GRACE_MS - 1 }, failedStatus);
  assertEquals([late.state, late.text, late.primary.action],
    ["failed", "Sign-in failed: that sign-in link was already used or has expired", "start"]);
  assertEquals(at({ step: "preparing", since: now - 1000 }, ready).state, "preparing", "switching away from Ready");
  assertEquals(at({ step: "completing", since: now - GRACE_MS - 1, used: [URL1] }).state, "failed");
  assertEquals(at({ step: "signingOut", since: now }, ready).state, "signingOut");
  assertEquals(at({}, { ...awaiting, phase: "SessionExpired" }).primary.label, "Sign in again");
});

Deno.test("friendlyError hides package prefixes", () => {
  assertEquals(friendlyError("marketidentity: resolving the configured handle x: not found"),
    "resolving the configured handle x: not found");
  assertEquals(friendlyError("OAuth popup was blocked"), "the browser blocked the sign-in tab; tap Try again");
});
