// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// Owner fence for hibernation snapshots (rc130). Two tabs number their
// persists from the same stored base, so seqs alone cannot tell a stale
// holder's capture from the owner's: both outcomes are tested here.

import { readOwner, restampOwner, snapshotRejection, startHibernation, tryRestore } from "./kcp_hibernate.js";
import { createIndexedDBBackend } from "./state_store.js";
import { fakeIndexedDB, fakeKeyRange } from "./fake_indexeddb_for_tests.js";

function assertEquals(actual, expected, label = "value") {
  const a = JSON.stringify(actual);
  const e = JSON.stringify(expected);
  if (a !== e) throw new Error(`${label}: expected ${e}, got ${a}`);
}

const KEY = "k1";
const SEQS = { kcp: 12, socialweb: 7 };
const meta = (owner) => ({ extra: { key: KEY, seqs: { ...SEQS }, owner }, bytes: 0, stored: 0, wallMs: 0, stats: {} });

Deno.test("snapshotRejection: same seqs from a stale holder are refused by the owner token", () => {
  assertEquals(snapshotRejection(meta("tab-b"), { key: KEY, seqs: SEQS, owner: "tab-b" }), "", "the owner's snapshot");
  assertEquals(
    snapshotRejection(meta("tab-a"), { key: KEY, seqs: SEQS, owner: "tab-b" }),
    "written by a tab that no longer owned the saved state",
    "a stale holder's capture with colliding seqs",
  );
  assertEquals(snapshotRejection(meta(undefined), { key: KEY, seqs: SEQS, owner: null }), "", "no owner yet (first fenced load)");
  assertEquals(snapshotRejection(meta("tab-a"), { key: KEY, seqs: SEQS, owner: undefined }), "", "fence off (memory state)");
  assertEquals(snapshotRejection(meta("tab-b"), { key: KEY, seqs: { kcp: 13, socialweb: 7 }, owner: "tab-b" }).startsWith("persisted state is newer"), true);
});

Deno.test("tryRestore discards a stale holder's snapshot and cold boots", async () => {
  let discarded = 0;
  const store = { kind: "fake", readMeta: async () => meta("tab-a"), discard: async () => void discarded++ };
  const logs = [];
  const r = await tryRestore({ go: {}, module: null, key: KEY, seqs: SEQS, owner: "tab-b", store, log: (l, m) => logs.push(m), before: () => {} });
  assertEquals([r, discarded], [null, 1]);
  assertEquals(logs, ["snapshot discarded: written by a tab that no longer owned the saved state"]);
});

const fakeGo = () => ({ exited: false, _inst: { exports: { __snap_g0: 1 } } });
function capturing(currentOwners) {
  const saved = [];
  const logs = [];
  const store = {
    kind: "fake",
    async save(_go, extra, _gzip, beforeCommit) {
      const e = extra();
      await beforeCommit();
      saved.push(e);
      return { extra: e, bytes: 0, stored: 0, stats: {} };
    },
  };
  let i = 0;
  const loop = startHibernation({
    go: fakeGo(),
    key: KEY,
    ready: () => true,
    seqs: () => ({ ...SEQS }),
    extra: () => ({}),
    store,
    log: (_l, m) => logs.push(m),
    owner: { token: "tab-a", current: async () => currentOwners[Math.min(i++, currentOwners.length - 1)] },
    timers: { setInterval: () => 0 },
  });
  return { loop, saved, logs };
}
const settle = () => new Promise((r) => setTimeout(r, 10));

Deno.test("capture: the owner's snapshot carries its token", async () => {
  const { loop, saved } = capturing(["tab-a"]);
  loop.request();
  await settle();
  assertEquals(saved.map((e) => e.owner), ["tab-a"]);
});

Deno.test("capture: a tab that lost the state writes no snapshot, before or at commit", async () => {
  const before = capturing(["tab-b"]);
  before.loop.request();
  await settle();
  before.loop.request();
  await settle();
  assertEquals(before.saved, [], "refused before capturing, then stopped");
  assertEquals(before.logs.filter((m) => m.startsWith("snapshot capture stopped")).length, 1);
  const atCommit = capturing(["tab-a", "tab-b"]); // taken over during the capture
  atCommit.loop.request();
  await settle();
  assertEquals(atCommit.saved, [], "refused right before the commit");
});

Deno.test("readOwner reads the state database's owner and creates nothing", async () => {
  const factory = fakeIndexedDB();
  const backend = await createIndexedDBBackend("kcp-wasm", { indexedDB: factory, keyRange: fakeKeyRange });
  assertEquals(await readOwner("kcp-wasm", factory), null, "unclaimed");
  await backend.claimOwner("tab-b");
  assertEquals(await readOwner("kcp-wasm", factory), "tab-b");
  assertEquals(await readOwner("missing", factory), null);
  assertEquals(await readOwner("kcp-wasm", undefined), null, "no IndexedDB");
});

Deno.test("restampOwner hands a restored snapshot to the new owner", async () => {
  let stored = meta("tab-a");
  const store = { readMeta: async () => stored, writeMeta: async (m) => void (stored = structuredClone(m)) };
  assertEquals(await restampOwner(store, "tab-b"), true);
  assertEquals(stored.extra.owner, "tab-b");
  assertEquals(await restampOwner(store, "tab-b"), false, "already stamped");
});

Deno.test("page wiring: snapshots get this tab's token and the previous owner's for restores", async () => {
  const app = await Deno.readTextFile(new URL("./app.js", import.meta.url));
  const worker = await Deno.readTextFile(new URL("./kcp_worker.js", import.meta.url));
  const claim = app.slice(app.indexOf("async function claimStateOwner"), app.indexOf("function stateWriteFailed"));
  if (!(claim.indexOf("backend.load(OWNER_KEY)") >= 0 && claim.indexOf("backend.load(OWNER_KEY)") < claim.indexOf("claimOwner("))) {
    throw new Error("previous owner read before the claim");
  }
  if (!/owner: stateOwnerPrevious === undefined \? null : \{ token: stateOwnerToken, previous: stateOwnerPrevious, db: stateDB \}/.test(app)) {
    throw new Error("hibernate init carries the owner");
  }
  for (const needle of ["init.hibernate.owner.previous", "hibernate.readOwner(init.hibernate.owner.db)", "hibernate.restampOwner("]) {
    if (!worker.includes(needle)) throw new Error(`worker lacks ${needle}`);
  }
});

// rc131 cold boots: a flush writes records newer than the last snapshot, so
// the worker asks for a capture right after it; it happens only when the
// seqs moved, without waiting for the 30 s quiet period.
Deno.test("capture after a flush: only when the seqs moved, at once", async () => {
  let seqs = { kcp: 9, socialweb: 0 };
  const saved = [];
  const store = {
    kind: "fake",
    async save(_go, extra, _gzip, beforeCommit) {
      const e = extra();
      await beforeCommit();
      saved.push(e.seqs.kcp);
      return { extra: e, bytes: 0, stored: 0, stats: {} };
    },
  };
  const loop = startHibernation({
    go: fakeGo(),
    key: KEY,
    ready: () => true,
    seqs: () => ({ ...seqs }),
    extra: () => ({}),
    captured: { kcp: 9, socialweb: 0 }, // the snapshot this load restored
    store,
    log: () => {},
    timers: { setInterval: () => 0 },
  });
  loop.request(); // a flush that wrote nothing new
  await settle();
  assertEquals(saved, [], "no capture without new records");
  seqs = { kcp: 10, socialweb: 0 }; // the flush persisted one message
  loop.request();
  await settle();
  assertEquals(saved, [10], "captured right after the flush, inside the quiet period");
  const worker = await Deno.readTextFile(new URL("./kcp_worker.js", import.meta.url));
  if (!/flushed: Boolean\(out\?\.flushed\) \}\);[\s\S]{0,400}if \(out\?\.flushed\) hibernation\?\.request\(\);/.test(worker)) {
    throw new Error("the worker asks for a capture after each flush");
  }
});
