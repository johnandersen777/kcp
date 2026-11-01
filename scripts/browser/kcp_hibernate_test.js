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

// rc131 cold boots, without a capture storm (review page-rc131 2e7d88ba5,
// 1): a flush asks for a capture (requestSoon) that waits until the seqs
// were still SOON_QUIET_MS and SOON_GAP_MS after the last capture; a hide
// (request) captures at once. A fake clock drives the loop's tick.
function clockedLoop({ captured = { kcp: 9, socialweb: 0 } } = {}) {
  const c = { t: 1_000_000, seqs: { ...captured }, saved: [], tick: null };
  const store = {
    kind: "fake",
    async save(_go, extra, _gzip, beforeCommit) {
      const e = extra();
      await beforeCommit();
      c.saved.push({ at: c.t, kcp: e.seqs.kcp });
      return { extra: e, bytes: 0, stored: 0, stats: {} };
    },
  };
  c.loop = startHibernation({
    go: fakeGo(),
    key: KEY,
    ready: () => true,
    seqs: () => ({ ...c.seqs }),
    extra: () => ({}),
    captured,
    store,
    log: () => {},
    timers: { setInterval: (fn) => (c.tick = fn) },
    now: () => c.t,
  });
  // advance the clock second by second, ticking like the 1 s interval
  c.run = async (seconds, each = () => {}) => {
    for (let i = 0; i < seconds; i++) {
      c.t += 1000;
      each(i);
      await c.tick();
    }
  };
  return c;
}

Deno.test("capture after a flush: once the seqs are still, never for a no-op flush", async () => {
  const c = clockedLoop();
  c.loop.requestSoon(); // a flush that wrote nothing new
  await c.run(5);
  assertEquals(c.saved, [], "no capture without new records");
  c.seqs.kcp = 10; // the flush persisted one message
  c.loop.requestSoon();
  await c.run(2);
  assertEquals(c.saved, [], "not before SOON_QUIET_MS of still seqs");
  await c.run(3);
  assertEquals(c.saved.map((x) => x.kcp), [10], "captured a few seconds after the flush, long before QUIET_MS (30 s)");
});

Deno.test("no request: moving seqs capture only after the 30 s quiet period", async () => {
  const c = clockedLoop();
  c.loop.requestSoon(); // unchanged seqs: not armed
  await c.run(1);
  c.seqs.kcp = 10; // a background write, no flush
  await c.run(20);
  assertEquals(c.saved, [], "nothing before the quiet period");
  await c.run(15);
  assertEquals(c.saved.length, 1, "the regular capture");
});

Deno.test("a typical VM lifecycle captures at most about twice", async () => {
  const c = clockedLoop();
  // Request VM to Held: ~60 s of requester status writes every 1-2 s, a
  // flush at each of 7 phase/step changes.
  await c.run(60, (i) => {
    if (i % 2 === 0) c.seqs.kcp++;
    if (i % 9 === 0) c.loop.requestSoon();
  });
  await c.run(10); // Held, settled
  // minutes of use: occasional background writes, no flushes
  await c.run(120, (i) => {
    if (i % 40 === 0) c.seqs.kcp++;
  });
  // Release: ~15 s of writes, 3 flushes
  await c.run(15, (i) => {
    c.seqs.kcp++;
    if (i % 5 === 0) c.loop.requestSoon();
  });
  await c.run(10);
  // [62 s: Held settled, 141 s: the regular 30 s-quiet capture of a
  // background write, 208 s: Released settled]: two from flushes.
  const at = c.saved.map((x) => (x.at - 1_000_000) / 1000);
  assert(c.saved.length <= 3, `captures at ${JSON.stringify(at)} s`);
  assertEquals(at.filter((s) => (s > 60 && s <= 70) || s > 195).length, 2, "one per settle point");
  // the two settle points are captured right away
  const settledHeld = c.saved.find((x) => x.at > 1_000_000 + 60_000 && x.at <= 1_000_000 + 70_000);
  assert(settledHeld, "Held captured once settled");
  const burst = c.saved.filter((x) => x.at <= 1_000_000 + 60_000);
  assertEquals(burst.length, 0, "no capture during the bidding burst (seqs never still 3 s)");
});

Deno.test("a hide captures at once; the worker asks requestSoon after a flush", async () => {
  const c = clockedLoop();
  c.seqs.kcp = 10;
  c.loop.request(); // visibilitychange to hidden
  await new Promise((r) => setTimeout(r, 10));
  assertEquals(c.saved.map((x) => x.kcp), [10]);
  const worker = await Deno.readTextFile(new URL("./kcp_worker.js", import.meta.url));
  if (!/if \(out\?\.flushed\) hibernation\?\.requestSoon\(\);/.test(worker)) throw new Error("flushes use requestSoon");
  if (!/type === "hibernate"\) \{\s*hibernation\?\.request\(\);/.test(worker)) throw new Error("hide uses request");
});

Deno.test("a hide whose flush lands after the hide's tick still captures at once", async () => {
  const c = clockedLoop();
  c.loop.request(); // hidden: the flush has not persisted yet, seqs unchanged
  await c.run(1);
  assertEquals(c.saved, [], "nothing new yet");
  c.seqs.kcp = 10; // the hide's flush persists
  c.loop.requestSoon();
  await new Promise((r) => setTimeout(r, 10));
  assertEquals(c.saved.map((x) => x.kcp), [10], "captured without the SOON_QUIET_MS/SOON_GAP_MS wait");
  // later flushes are ordinary again
  await c.run(20);
  c.seqs.kcp = 11;
  c.loop.requestSoon();
  await c.run(1);
  assertEquals(c.saved.length, 1, "a flush long after the hide waits for still seqs");
  await c.run(4);
  assertEquals(c.saved.map((x) => x.kcp), [10, 11]);
});

function assert(condition, message) {
  if (!condition) throw new Error(message);
}

Deno.test("restampOwner stamps only the snapshot this load restored", async () => {
  let stored = { ...meta("tab-a"), wallMs: 5, memFile: "mem-5.gz" };
  const store = { readMeta: async () => stored, writeMeta: async (m) => void (stored = structuredClone(m)) };
  const restored = structuredClone(stored);
  stored = { ...meta("tab-a"), wallMs: 9, memFile: "mem-9.gz" }; // a capture committed since
  assertEquals(await restampOwner(store, "tab-b", restored), false, "another snapshot");
  stored = structuredClone(restored);
  assertEquals(await restampOwner(store, "tab-b", restored), true, "the restored one");
});
