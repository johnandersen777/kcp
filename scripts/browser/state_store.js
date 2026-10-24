// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// Persistence primitives for the in-browser socialweb/local-PDS instance.
// Values are JSON strings: callers serialize their snapshots and the backend
// only moves bytes. IndexedDB is used when the platform provides it; a memory
// backend covers tests and browsers with IndexedDB disabled.

export const DEFAULT_MAX_BYTES = 8 * 1024 * 1024;
export const STORE_NAME = "state";

export class StateStoreError extends Error {
  constructor(message, code, cause) {
    super(message);
    this.name = "StateStoreError";
    this.code = code;
    if (cause !== undefined) this.cause = cause;
  }
}

const encoder = new TextEncoder();

function assertKey(key) {
  if (typeof key !== "string" || key === "") {
    throw new StateStoreError(
      "state_store: key must be a non-empty string",
      "InvalidKey",
    );
  }
}

function assertStorable(key, value, maxBytes) {
  assertKey(key);
  if (typeof value !== "string") {
    throw new StateStoreError(
      `state_store: value for key "${key}" must be a string, got ${typeof value}`,
      "InvalidValue",
    );
  }
  const bytes = encoder.encode(value).byteLength;
  if (bytes > maxBytes) {
    throw new StateStoreError(
      `state_store: value for key "${key}" is ${bytes} bytes, exceeds the ${maxBytes} byte limit`,
      "ValueTooLarge",
    );
  }
}

/** In-memory backend used by tests and as the IndexedDB fallback. */
export function createMemoryBackend({ maxBytes = DEFAULT_MAX_BYTES } = {}) {
  const values = new Map();
  return {
    kind: "memory",
    async load(key) {
      assertKey(key);
      return values.has(key) ? values.get(key) : null;
    },
    async save(key, value) {
      assertStorable(key, value, maxBytes);
      values.set(key, value);
    },
    async remove(key) {
      assertKey(key);
      values.delete(key);
    },
    async loadPrefix(prefix) {
      assertKey(prefix);
      return [...values].filter(([k]) => k.startsWith(prefix)).map(([, v]) => v);
    },
    async apply({ clearPrefix, deletes = [], puts = [] }) {
      if (clearPrefix) {
        for (const k of [...values.keys()]) if (k.startsWith(clearPrefix)) values.delete(k);
      }
      for (const k of deletes) values.delete(k);
      for (const [k, v] of puts) values.set(k, v);
    },
  };
}

// --- kcp storage snapshots ------------------------------------------------
//
// The kcp shard posts a full snapshot
//   {version, stores: {name: {resourceVersion, items, clusters, shards}}}
// once after boot (and occasionally as a compaction), then deltas
//   {version, delta: true, stores: {name: {resourceVersion, items, clusters, shards, deletes}}}
// where items hold only the keys written since the previous message. Any other
// top-level section is carried verbatim; a delta's copy replaces the saved one.

const ENVELOPE_FIELDS = new Set(["version", "delta", "stores"]);
const SEP = "\u0000";

function setOrDelete(map, key, value) {
  if (value === undefined) delete map[key];
  else map[key] = value;
}

/**
 * Applies one snapshot message to a full snapshot object (or null) and returns
 * the resulting full snapshot. Used by the Deno host, which writes the full
 * snapshot to its state file.
 */
export function applySnapshot(state, message) {
  if (!message?.delta) return message;
  const out = state ?? { version: message.version, stores: {} };
  out.stores ??= {};
  for (const [field, value] of Object.entries(message)) {
    if (!ENVELOPE_FIELDS.has(field)) out[field] = value;
  }
  for (const [name, delta] of Object.entries(message.stores ?? {})) {
    const store = (out.stores[name] ??= { resourceVersion: 0, items: {} });
    store.items ??= {};
    store.clusters ??= {};
    store.shards ??= {};
    store.resourceVersion = delta.resourceVersion;
    for (const [key, item] of Object.entries(delta.items ?? {})) {
      store.items[key] = item;
      setOrDelete(store.clusters, key, delta.clusters?.[key]);
      setOrDelete(store.shards, key, delta.shards?.[key]);
    }
    for (const key of delta.deletes ?? []) {
      delete store.items[key];
      delete store.clusters[key];
      delete store.shards[key];
    }
  }
  return out;
}

/**
 * Turns one snapshot message into backend.apply() operations for a per-key
 * layout under prefix: one record per object, per store resource version and
 * per extra section. A full snapshot replaces every record under prefix.
 */
export function snapshotRecords(prefix, message) {
  const ops = { clearPrefix: message.delta ? null : prefix, deletes: [], puts: [] };
  ops.puts.push([`${prefix}v`, { kind: "version", version: message.version }]);
  for (const [field, value] of Object.entries(message)) {
    if (!ENVELOPE_FIELDS.has(field)) {
      ops.puts.push([`${prefix}x${SEP}${field}`, { kind: "section", field, value }]);
    }
  }
  for (const [store, snap] of Object.entries(message.stores ?? {})) {
    ops.puts.push([`${prefix}s${SEP}${store}`, { kind: "store", store, resourceVersion: snap.resourceVersion }]);
    for (const [key, item] of Object.entries(snap.items ?? {})) {
      ops.puts.push([`${prefix}i${SEP}${store}${SEP}${key}`, {
        kind: "item",
        store,
        key,
        item,
        cluster: snap.clusters?.[key],
        shard: snap.shards?.[key],
      }]);
    }
    for (const key of snap.deletes ?? []) ops.deletes.push(`${prefix}i${SEP}${store}${SEP}${key}`);
  }
  return ops;
}

/** Rebuilds the full snapshot object from the records of snapshotRecords. */
export function assembleSnapshot(records) {
  const out = { version: 1, stores: {} };
  const storeOf = (name) =>
    (out.stores[name] ??= { resourceVersion: 0, items: {}, clusters: {}, shards: {} });
  for (const r of records) {
    if (r.kind === "version") out.version = r.version;
    else if (r.kind === "section") out[r.field] = r.value;
    else if (r.kind === "store") storeOf(r.store).resourceVersion = r.resourceVersion;
    else if (r.kind === "item") {
      const s = storeOf(r.store);
      s.items[r.key] = r.item;
      if (r.cluster !== undefined) s.clusters[r.key] = r.cluster;
      if (r.shard !== undefined) s.shards[r.key] = r.shard;
    }
  }
  return out;
}

function idbRequest(request) {
  return new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result);
    request.onerror = () =>
      reject(
        request.error ??
          new StateStoreError(
            "state_store: IndexedDB request failed",
            "IndexedDBError",
          ),
      );
  });
}

function transactionDone(tx) {
  return new Promise((resolve, reject) => {
    tx.oncomplete = () => resolve();
    tx.onerror = () =>
      reject(
        tx.error ??
          new StateStoreError(
            "state_store: IndexedDB transaction failed",
            "IndexedDBError",
          ),
      );
    tx.onabort = () =>
      reject(
        tx.error ??
          new StateStoreError(
            "state_store: IndexedDB transaction aborted",
            "IndexedDBError",
          ),
      );
  });
}

function openDatabase(factory, dbName) {
  return new Promise((resolve, reject) => {
    let request;
    try {
      request = factory.open(dbName, 1);
    } catch (error) {
      reject(error);
      return;
    }
    request.onupgradeneeded = () => {
      const db = request.result;
      if (!db.objectStoreNames.contains(STORE_NAME)) {
        db.createObjectStore(STORE_NAME);
      }
    };
    request.onsuccess = () => resolve(request.result);
    request.onerror = () =>
      reject(
        request.error ??
          new StateStoreError(
            `state_store: failed to open IndexedDB "${dbName}"`,
            "OpenFailed",
          ),
      );
    request.onblocked = () =>
      reject(
        new StateStoreError(
          `state_store: IndexedDB "${dbName}" upgrade is blocked by another connection`,
          "OpenBlocked",
        ),
      );
  });
}

/**
 * IndexedDB backend backed by a single out-of-line-key object store.
 * The returned promise rejects when the database cannot be opened.
 */
export async function createIndexedDBBackend(
  dbName,
  { indexedDB: factory = globalThis.indexedDB, maxBytes = DEFAULT_MAX_BYTES } = {},
) {
  if (!factory || typeof factory.open !== "function") {
    throw new StateStoreError("state_store: IndexedDB is not available", "Unavailable");
  }
  const db = await openDatabase(factory, dbName);
  return {
    kind: "indexeddb",
    async load(key) {
      assertKey(key);
      const tx = db.transaction(STORE_NAME, "readonly");
      const value = await idbRequest(tx.objectStore(STORE_NAME).get(key));
      return value === undefined ? null : value;
    },
    async save(key, value) {
      assertStorable(key, value, maxBytes);
      const tx = db.transaction(STORE_NAME, "readwrite");
      await Promise.all([
        idbRequest(tx.objectStore(STORE_NAME).put(value, key)),
        transactionDone(tx),
      ]);
    },
    async remove(key) {
      assertKey(key);
      const tx = db.transaction(STORE_NAME, "readwrite");
      await Promise.all([
        idbRequest(tx.objectStore(STORE_NAME).delete(key)),
        transactionDone(tx),
      ]);
    },
    async loadPrefix(prefix) {
      assertKey(prefix);
      const tx = db.transaction(STORE_NAME, "readonly");
      const range = IDBKeyRange.bound(prefix, `${prefix}￿`);
      return await idbRequest(tx.objectStore(STORE_NAME).getAll(range));
    },
    // One transaction per call: a snapshot message lands whole or not at all.
    async apply({ clearPrefix, deletes = [], puts = [] }) {
      const tx = db.transaction(STORE_NAME, "readwrite");
      const store = tx.objectStore(STORE_NAME);
      if (clearPrefix) store.delete(IDBKeyRange.bound(clearPrefix, `${clearPrefix}￿`));
      for (const k of deletes) store.delete(k);
      for (const [k, v] of puts) store.put(v, k);
      await transactionDone(tx);
    },
  };
}

/**
 * Picks IndexedDB when the platform exposes it and opening succeeds, otherwise
 * memory. `reason` carries the IndexedDB error that forced the fallback.
 *
 * @returns {Promise<{backend: object, kind: "indexeddb"|"memory", reason: unknown}>}
 */
export async function openBestBackend(
  { dbName = "socialweb-state", indexedDB: factory = globalThis.indexedDB, maxBytes = DEFAULT_MAX_BYTES } = {},
) {
  if (factory && typeof factory.open === "function") {
    try {
      const backend = await createIndexedDBBackend(dbName, { indexedDB: factory, maxBytes });
      return { backend, kind: "indexeddb", reason: null };
    } catch (reason) {
      return { backend: createMemoryBackend({ maxBytes }), kind: "memory", reason };
    }
  }
  return {
    backend: createMemoryBackend({ maxBytes }),
    kind: "memory",
    reason: null,
  };
}
