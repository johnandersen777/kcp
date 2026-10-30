// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import {
  contractFinishedAt,
  contractLive,
  feedContract,
  infraContractName,
  infraRecordBinding,
  originInfraName,
  originInfraTab,
  planInfraGC,
} from "./vmshell.js";

function assertEquals(actual, expected, label = "value") {
  const a = JSON.stringify(actual);
  const e = JSON.stringify(expected);
  if (a !== e) throw new Error(`${label}: expected ${e}, got ${a}`);
}

const ORIGIN = "https://kcpwasm--johnandersen777-bsky-social.fedproxy.com";
const OTHER_ORIGIN = "https://shell.socialweb.computer";
const ALICE = "did:plc:lpfuqerea3deuoyrn7ojser4";
const BOB = "did:plc:bobbobbobbobbobbobbobbob";
const ME = "mytab1";

const NOW = Date.parse("2026-10-30T12:00:00Z");
const hoursAgo = (h) => new Date(NOW - h * 3600_000).toISOString();
const infra = (origin, tab, entries, extra = {}) => ({
  metadata: { name: originInfraName(origin, tab), creationTimestamp: "2026-10-01T00:00:00Z", ...extra.metadata },
  spec: { contracts: entries.map((name) => ({ name, state: "Requested" })), record: infraRecordBinding(ALICE), ...extra.spec },
  status: extra.status ?? {},
});
// A ComputeContract object as kcp serves it.
const cc = (infraObj, short, status = {}, spec = {}) => ({
  metadata: { name: infraContractName(infraObj.metadata.name, short) },
  spec,
  status,
});
const receipt = { receipt: { cid: "bafy" }, receiptOk: true };

Deno.test("originInfraTab recognises only this origin's infra names", () => {
  assertEquals(originInfraTab(ORIGIN, originInfraName(ORIGIN, "abc123")), "abc123");
  assertEquals(originInfraTab(ORIGIN, originInfraName(OTHER_ORIGIN, "abc123")), null, "other origin");
  assertEquals(originInfraTab(ORIGIN, "einfra-something"), null, "not a page infra");
  assertEquals(originInfraTab(ORIGIN, `${originInfraName(ORIGIN, "abc123")}-x`), null, "suffix");
});

Deno.test("feedContract mirrors the status feed's guest parts", () => {
  const obj = {
    metadata: { name: "c" },
    spec: { release: true, releaseRequestedAt: "t2" },
    status: {
      phase: "Failed",
      release: { requestedAt: "t1" },
      conditions: [{ type: "ReleaseFailed", status: "True", reason: "ReleaseBound" }],
      contract: { ...receipt, release: { ok: false } },
      atircRevokes: "Done",
    },
  };
  assertEquals(feedContract(obj), {
    name: "c", phase: "Failed", release: true, releaseRequestedAt: "t2", releaseAnsweredAt: "t1",
    releaseFailed: true, releaseFailedReason: "ReleaseBound", atircRevokes: "Done", step: "",
    receiptOk: true, terminated: false, releaseOk: false, finishedAt: 0,
  });
  assertEquals(feedContract({ status: { contract: { receiptOk: true } } }).receiptOk, false, "receiptOk without a receipt");
});

Deno.test("contractLive: running, failed with a guest, and pending release work are live", () => {
  const live = (status, spec) => contractLive(feedContract({ metadata: { name: "c" }, status, spec }));
  assertEquals(live({ phase: "Held" }), true, "Held");
  assertEquals(live({}), true, "Pending (no phase yet)");
  assertEquals(live({ phase: "Released" }), false, "Released");
  assertEquals(live({ phase: "Failed" }), false, "Failed, never ran");
  assertEquals(live({ phase: "Failed", contract: receipt }), true, "Failed, guest may run");
  assertEquals(live({ phase: "Failed", contract: { ...receipt, termination: { reason: "exit" } } }), false, "Failed, terminated");
  assertEquals(live({ phase: "Failed", contract: { ...receipt, release: { ok: true } } }), false, "Failed, release acked");
  assertEquals(live({ phase: "Failed", conditions: [{ type: "ReleaseFailed", status: "True" }] }), true, "failed release");
  assertEquals(live({ phase: "Released", atircRevokes: "Pending" }), true, "atirc revokes pending");
});

Deno.test("planInfraGC deletes dead infras, rebinds live ones, leaves the rest alone", () => {
  const mine = infra(ORIGIN, ME, ["vmshell-a"]);
  const dead = infra(ORIGIN, "old001", ["vmshell-r", "vmshell-f", "vmshell-gone"]);
  const empty = infra(ORIGIN, "old002", []);
  const held = infra(ORIGIN, "old003", ["vmshell-h"], { spec: { record: { repoRef: infraRecordBinding(ALICE).repoRef } } });
  const heldOk = infra(ORIGIN, "old004", ["vmshell-h"]);
  const failedGuest = infra(ORIGIN, "old005", ["vmshell-g"], { spec: { record: undefined } });
  const otherRepo = infra(ORIGIN, "old006", ["vmshell-h"], { spec: { record: infraRecordBinding(BOB) } });
  const legacyUri = infra(ORIGIN, "old007", ["vmshell-h"], { spec: { record: { uri: "at://x/y/z" } } });
  const ending = infra(ORIGIN, "old008", [], { metadata: { deletionTimestamp: "2026-10-30T00:00:00Z" } });
  const ended = infra(ORIGIN, "old009", [], { status: { endedAt: "2026-10-30T00:00:00Z" } });
  const foreign = infra(OTHER_ORIGIN, "old001", []);
  const objects = [
    cc(mine, "vmshell-a", { phase: "Released" }),
    cc(dead, "vmshell-r", { phase: "Released" }),
    cc(dead, "vmshell-f", { phase: "Failed" }),
    cc(held, "vmshell-h", { phase: "Held" }),
    cc(heldOk, "vmshell-h", { phase: "Held" }),
    cc(failedGuest, "vmshell-g", { phase: "Failed", contract: receipt }),
    cc(otherRepo, "vmshell-h", { phase: "Bidding" }),
    cc(legacyUri, "vmshell-h", { phase: "Held" }),
  ];
  const plan = planInfraGC({
    infras: [mine, dead, empty, held, heldOk, failedGuest, otherRepo, legacyUri, ending, ended, foreign],
    contracts: objects.map(feedContract),
    origin: ORIGIN,
    tabId: ME,
    did: ALICE,
    now: NOW,
  });
  const name = (tab) => originInfraName(ORIGIN, tab);
  assertEquals(plan.map((step) => [step.name, step.action]), [
    [name("old001"), "delete"],
    [name("old002"), "delete"],
    [name("old003"), "rebind"],
    [name("old004"), "keep"],
    [name("old005"), "rebind"],
    [name("old006"), "keep"],
    [name("old007"), "keep"],
    [name("old008"), "skip"],
    [name("old009"), "skip"],
  ]);
  assertEquals(plan[2].live, [infraContractName(name("old003"), "vmshell-h")], "live contracts listed");
  assertEquals(plan.some((step) => step.name === mine.metadata.name), false, "own infra untouched");
  assertEquals(plan.some((step) => step.name === foreign.metadata.name), false, "other origin untouched");
});

Deno.test("page wiring: GC runs once the identity is Ready and rebinds through composeInfra", async () => {
  const app = await Deno.readTextFile(new URL("./app.js", import.meta.url));
  if (!/identityState\?\.phase === "Ready" && identityState\.did\) void collectOtherTabInfras\(identityState\.did\)/.test(app)) {
    throw new Error("GC not started on Ready");
  }
  const gc = app.slice(app.indexOf("async function collectOtherTabInfras"), app.indexOf("// Sets the state of contract short"));
  for (const needle of ["vmshell.planInfraGC(", "tabId: infraTabId()", "origin: location.origin", "vmshell.composeInfra(", "infraQueued(", "\"DELETE\""]) {
    if (!gc.includes(needle)) throw new Error(`GC lacks ${needle}`);
  }
});

Deno.test("planInfraGC keeps 24 h of finished history (rc130)", () => {
  const released = (status) => ({ phase: "Released", conditions: [{ type: "Ready", status: "False", lastTransitionTime: status }] });
  const recent = infra(ORIGIN, "rec001", ["vmshell-r"]);
  const old = infra(ORIGIN, "old001", ["vmshell-r"]);
  const mixed = infra(ORIGIN, "mix001", ["vmshell-live", "vmshell-old"]);
  const fresh = infra(ORIGIN, "new001", ["vmshell-x"], { metadata: { creationTimestamp: hoursAgo(2) } });
  const stamped = infra(ORIGIN, "stp001", [], { spec: { contracts: [{ name: "vmshell-s", state: "Released", releaseRequestedAt: hoursAgo(3) }] } });
  const objects = [
    cc(recent, "vmshell-r", released(hoursAgo(1))),
    cc(old, "vmshell-r", released(hoursAgo(25))),
    cc(mixed, "vmshell-live", { phase: "Held" }),
    cc(mixed, "vmshell-old", released(hoursAgo(48))),
  ];
  const plan = planInfraGC({
    infras: [recent, old, mixed, fresh, stamped],
    contracts: objects.map(feedContract),
    origin: ORIGIN,
    tabId: ME,
    did: ALICE,
    now: NOW,
  });
  const by = Object.fromEntries(plan.map((step) => [step.name, step]));
  const n = (tab) => originInfraName(ORIGIN, tab);
  assertEquals(by[n("rec001")].action, "keep", "Released 1 h ago stays");
  assertEquals(by[n("rec001")].reason, "history: finished 1.0 h ago");
  assertEquals(by[n("old001")].action, "delete", "Released 25 h ago goes");
  assertEquals(by[n("mix001")].action, "keep", "one live, one old terminal: kept");
  assertEquals(by[n("new001")].action, "keep", "no contract times: the infra's creation (2 h)");
  assertEquals(by[n("stp001")].action, "keep", "the entry's release stamp counts");
  assertEquals(contractFinishedAt({ status: { contract: { termination: { observedAt: hoursAgo(5) } } } }), NOW - 5 * 3600_000);
});
