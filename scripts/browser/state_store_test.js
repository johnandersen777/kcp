// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// fake-indexeddb is not a cached npm dependency here, and CI must not fetch
// from the network, so these tests cover the memory backend and fallback
// selection. The IndexedDB path shares the same guard code and remains covered
// end to end by the browser demo once M5 wires it up.

import {
  createIndexedDBBackend,
  createMemoryBackend,
  openBestBackend,
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
