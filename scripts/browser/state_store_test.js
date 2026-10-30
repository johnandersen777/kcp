// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// fake-indexeddb is not a cached npm dependency here, and CI must not fetch
// from the network, so these tests cover the memory backend and fallback
// selection. The IndexedDB path shares the same guard code and remains covered
// end to end by the browser demo once M5 wires it up.

import {
  applySnapshot,
  assembleSnapshot,
  createIndexedDBBackend,
  createMemoryBackend,
  createPersister,
  openBestBackend,
  snapshotRecords,
  StateStoreError,
} from "./state_store.js";

function assertEquals(actual, expected) {
  if (actual !== expected) {
    throw new Error(`expected:\n${expected}\nactual:\n${actual}`);
  }
}

function assert(condition, message) {
  if (!condition) throw new Error(message);
}

async function assertRejects(fn, code) {
  try {
    await fn();
  } catch (error) {
    assert(error instanceof StateStoreError, `expected StateStoreError, got ${error}`);
    if (code !== undefined) assertEquals(error.code, code);
    return error;
  }
  throw new Error("expected the call to reject");
}

Deno.test("memory backend round-trips strings and removes keys", async () => {
  const backend = createMemoryBackend();
  assertEquals(backend.kind, "memory");
  assertEquals(await backend.load("kcp"), null);

  await backend.save("kcp", '{"objects":{}}');
  assertEquals(await backend.load("kcp"), '{"objects":{}}');

  await backend.remove("kcp");
  assertEquals(await backend.load("kcp"), null);
});

Deno.test("memory backend enforces keys, strings and the byte guard", async () => {
  const backend = createMemoryBackend({ maxBytes: 4 });
  await backend.save("k", "abcd");

  const tooLarge = await assertRejects(
    () => backend.save("k", "abcde"),
    "ValueTooLarge",
  );
  assert(tooLarge.message.includes('"k"'), "error names the key");
  assert(tooLarge.message.includes("5 bytes"), "error reports the size");
  assert(tooLarge.message.includes("4 byte limit"), "error reports the limit");

  await assertRejects(() => backend.save("", "x"), "InvalidKey");
  await assertRejects(() => backend.save("k", { a: 1 }), "InvalidValue");
  await assertRejects(() => backend.load(""), "InvalidKey");
  await assertRejects(() => backend.remove(""), "InvalidKey");
});

Deno.test("openBestBackend falls back to memory without IndexedDB", async () => {
  const chosen = await openBestBackend({ indexedDB: null, dbName: "test" });
  assertEquals(chosen.kind, "memory");
  assertEquals(chosen.reason, null);
  await chosen.backend.save("fs", '{"a":"b"}');
  assertEquals(await chosen.backend.load("fs"), '{"a":"b"}');
});

Deno.test("openBestBackend falls back when IndexedDB cannot open", async () => {
  const broken = {
    open() {
      throw new Error("blocked by test");
    },
  };
  const chosen = await openBestBackend({ indexedDB: broken, dbName: "test" });
  assertEquals(chosen.kind, "memory");
  assert(chosen.reason instanceof Error, "fallback carries the open error");
  assertEquals(chosen.reason.message, "blocked by test");
});

Deno.test("createIndexedDBBackend rejects when IndexedDB is unavailable", async () => {
  await assertRejects(
    () => createIndexedDBBackend("test", { indexedDB: null }),
    "Unavailable",
  );
});

const fullSnapshot = {
  version: 1,
  files: { "sa.key": "k1" },
  stores: {
    main: {
      resourceVersion: 5,
      items: { "/registry/a": { n: "a" }, "/registry/b": { n: "b" } },
      clusters: { "/registry/a": "root", "/registry/b": "root" },
    },
  },
};
const delta = {
  version: 1,
  delta: true,
  files: { "sa.key": "k2" },
  stores: {
    main: {
      resourceVersion: 7,
      items: { "/registry/a": { n: "a2" }, "/registry/c": { n: "c" } },
      shards: { "/registry/c": "s1" },
      deletes: ["/registry/b"],
    },
  },
};
const expected = {
  version: 1,
  files: { "sa.key": "k2" },
  stores: {
    main: {
      resourceVersion: 7,
      items: { "/registry/a": { n: "a2" }, "/registry/c": { n: "c" } },
      clusters: {},
      shards: { "/registry/c": "s1" },
    },
  },
};

function canonical(value) {
  if (Array.isArray(value)) return value.map(canonical);
  if (value && typeof value === "object") {
    return Object.fromEntries(Object.keys(value).sort().map((k) => [k, canonical(value[k])]));
  }
  return value;
}

Deno.test("applySnapshot folds deltas into a full snapshot", () => {
  const state = applySnapshot(null, structuredClone(fullSnapshot));
  const out = applySnapshot(state, structuredClone(delta));
  assertEquals(JSON.stringify(canonical(out)), JSON.stringify(canonical(expected)));
  // A full snapshot replaces whatever was there.
  assertEquals(applySnapshot(out, fullSnapshot), fullSnapshot);
});

Deno.test("per-key records round-trip full snapshots and deltas", async () => {
  const backend = createMemoryBackend();
  const prefix = "kcp\u0000";
  await backend.save("kcp", "legacy");
  await backend.save("socialweb", "other");
  await backend.apply(snapshotRecords(prefix, fullSnapshot));
  await backend.apply(snapshotRecords(prefix, delta));
  const rebuilt = assembleSnapshot(await backend.loadPrefix(prefix));
  assertEquals(JSON.stringify(canonical(rebuilt)), JSON.stringify(canonical(expected)));
  assertEquals(await backend.load("kcp"), "legacy");
  assertEquals(await backend.load("socialweb"), "other");

  // The next full snapshot drops records it no longer contains.
  await backend.apply(snapshotRecords(prefix, { version: 1, stores: { main: { resourceVersion: 9, items: {} } } }));
  const compacted = assembleSnapshot(await backend.loadPrefix(prefix));
  assertEquals(Object.keys(compacted.stores.main.items).length, 0);
  assertEquals(compacted.stores.main.resourceVersion, 9);
  assertEquals(compacted.files, undefined);
});

// --- createPersister (socialweb vault snapshot durability, rc127) ---

// A memory backend whose writes wait for release(), recording what landed.
function gatedBackend() {
  const store = createMemoryBackend();
  const gates = [];
  const writes = [];
  const gate = (op) => new Promise((resolve) => gates.push(() => resolve(op())));
  return {
    store,
    writes,
    gates,
    backend: () => Promise.resolve({
      save: (key, value) => gate(() => { writes.push([key, value]); return store.save(key, value); }),
      apply: (ops) => gate(() => { writes.push(...ops.puts.map(([k, v]) => [k, String(v)])); return store.apply(ops); }),
    }),
    async releaseAll() {
      while (gates.length) {
        gates.shift()();
        await new Promise((r) => setTimeout(r, 0));
      }
    },
  };
}

// Timers that only fire when the test says so.
function manualTimers() {
  const live = new Map();
  let next = 1;
  return {
    live,
    setTimeout: (fn, ms) => { const id = next++; live.set(id, { fn, ms }); return id; },
    clearTimeout: (id) => live.delete(id),
    fireAll() {
      const all = [...live.values()];
      live.clear();
      for (const t of all) t.fn();
    },
  };
}

Deno.test("persister: an immediate key saves at once with its seq, no timer", async () => {
  const store = createMemoryBackend();
  const timers = manualTimers();
  const p = createPersister({ backend: () => Promise.resolve(store), immediate: ["socialweb"], timers });
  await p.schedule("socialweb", '{"vault":"revoke-marker"}', 7);
  assertEquals(await store.load("socialweb"), '{"vault":"revoke-marker"}');
  assertEquals(String(await store.load("socialweb-seq")), "7");
  assertEquals(timers.live.size, 0);
  assertEquals(p.pending(), 0);
});

Deno.test("persister: other keys stay debounced until the timer", async () => {
  const store = createMemoryBackend();
  const timers = manualTimers();
  const p = createPersister({ backend: () => Promise.resolve(store), immediate: ["socialweb"], timers, delayMs: 250 });
  await p.schedule("other", "a");
  await p.schedule("other", "b");
  assertEquals(timers.live.size, 1);
  assertEquals(await store.load("other"), null);
  timers.fireAll();
  await p.flushAll();
  assertEquals(await store.load("other"), "b");
});

Deno.test("persister: concurrent saves are serialized and the newest snapshot wins", async () => {
  const g = gatedBackend();
  const p = createPersister({ backend: g.backend, immediate: ["socialweb"], timers: manualTimers() });
  const first = p.schedule("socialweb", "s1", 1);
  await new Promise((r) => setTimeout(r, 0));
  // s1 is in flight; s2 and s3 arrive before it lands: s3 replaces s2.
  const second = p.schedule("socialweb", "s2", 2);
  const third = p.schedule("socialweb", "s3", 3);
  assertEquals(g.gates.length, 1);
  await g.releaseAll();
  await Promise.all([first, second, third]);
  assertEquals(await g.store.load("socialweb"), "s3");
  assertEquals(String(await g.store.load("socialweb-seq")), "3");
  const saved = g.writes.filter(([k]) => k === "socialweb").map(([, v]) => v);
  assertEquals(saved.join(","), "s1,s3");
});

Deno.test("persister: a save failure is reported and the next snapshot still lands", async () => {
  const store = createMemoryBackend();
  let fail = true;
  const errors = [];
  const backend = () => Promise.resolve({
    apply: (ops) => {
      if (fail) {
        fail = false;
        return Promise.reject(new Error("quota"));
      }
      return store.apply(ops);
    },
  });
  const p = createPersister({ backend, immediate: ["socialweb"], timers: manualTimers(), onError: (k, e) => errors.push(`${k}: ${e.message}`) });
  await p.schedule("socialweb", "s1", 1);
  await p.schedule("socialweb", "s2", 2);
  assertEquals(errors.join(), "socialweb: quota");
  assertEquals(await store.load("socialweb"), "s2");
});

Deno.test("persister: flushAll (hidden, pagehide) starts every pending save at once", async () => {
  const g = gatedBackend();
  const timers = manualTimers();
  const p = createPersister({ backend: g.backend, immediate: ["socialweb"], timers, delayMs: 250 });
  await p.schedule("a", "a1");
  await p.schedule("b", "b1", 4);
  assertEquals(timers.live.size, 2);
  const flushed = p.flushAll();
  assertEquals(timers.live.size, 0);
  await new Promise((r) => setTimeout(r, 0));
  assertEquals(g.gates.length, 2);
  await g.releaseAll();
  await flushed;
  assertEquals(await g.store.load("a"), "a1");
  assertEquals(await g.store.load("b"), "b1");
  assertEquals(p.pending(), 0);
  await p.flushAll();
});

Deno.test("persister: the page saves socialweb immediately and flushes on hidden and pagehide", async () => {
  const app = await Deno.readTextFile(new URL("./app.js", import.meta.url));
  assert(/createPersister\(\{[\s\S]*?immediate: \[socialwebStateKey\]/.test(app), "socialweb key is immediate");
  assert(/"socialweb-persist"\) \{\s*void persister\.schedule\(socialwebStateKey/.test(app), "socialweb-persist goes to the persister");
  assert(/addEventListener\("pagehide", \(\) => void persister\.flushAll\(\)\)/.test(app), "pagehide flushes");
  assert(/visibilityState !== "hidden"\) return;\s*void persister\.flushAll\(\)/.test(app), "hidden flushes");
  assert(!/schedulePersist/.test(app), "the old debounced path is gone");
});
