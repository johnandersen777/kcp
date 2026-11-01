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

/** The banner the page shows while state lives only in memory. */
export const MEMORY_FALLBACK_MESSAGE =
  "This browser is not saving state: a reload loses your sign-in, VMs and tokens";

/**
 * What the page says about the backend openBestBackend picked: the #log line
 * (at "ok"/"warn", never the filtered debug level, so probes always see it)
 * and, for memory, the banner and console warning text.
 */
export function backendNotice({ kind, reason }) {
  const line = `kcp state backend: ${kind}`;
  if (kind !== "memory") return { level: "ok", line, banner: null, warn: null };
  const why = reason ? ` (IndexedDB unavailable: ${reason?.message ?? reason})` : " (no IndexedDB)";
  return { level: "warn", line: `${line}${why}`, banner: MEMORY_FALLBACK_MESSAGE, warn: `${MEMORY_FALLBACK_MESSAGE}${why}` };
}

/** The visible error for saved state of channel ("kcp" or "socialweb") that could not be read. */
export function unreadableMessage(channel) {
  return `saved ${channel} state could not be read; not saving over it`;
}

/**
 * Copies a saved value the running build could not read to
 * `<channel>-unreadable-<time>` before anything else happens, so a later
 * build (or a person) can still recover it. A value already backed up under
 * that channel is not copied again. Returns the backup key (or the existing
 * one's marker "" when it was a duplicate).
 */
export async function backupUnreadable(backend, channel, raw, now = new Date()) {
  if (typeof raw !== "string" || raw === "") return null;
  const prefix = `${channel}-unreadable-`;
  const existing = await backend.loadPrefix(prefix).catch(() => []);
  if (existing.includes(raw)) return "";
  const key = `${prefix}${now.toISOString()}`;
  // apply, not save: the backup may exceed the per-value limit save enforces.
  await backend.apply({ puts: [[key, raw]] });
  return key;
}

/**
 * Per-key snapshot saver for the page's IndexedDB state. Each key's saves run
 * one at a time, in arrival order, and the newest snapshot wins: a save
 * queued while another is in flight replaces any older queued one, so an
 * older snapshot never overwrites a newer one. (No seq comparison: a kcp
 * worker restarted after a snapshot discard counts seq from 0 again.) Keys in `immediate` save at once (the socialweb
 * vault: its revoke marker and atircMinted must survive a reload right after
 * the write); the others are debounced by delayMs. flushAll() starts every
 * pending save now (visibilitychange to hidden, pagehide).
 *
 * @param {{backend: () => Promise<object>, delayMs?: number, immediate?: string[],
 *   onError?: (key: string, err: unknown) => void, timers?: {setTimeout: Function, clearTimeout: Function}}} options
 */
export function createPersister(
  { backend, delayMs = 250, immediate = [], onError = () => {}, timers = globalThis },
) {
  const now = new Set(immediate);
  const queued = new Map();
  const debounce = new Map();
  const running = new Map();
  let stopped = false;

  async function save(key, { snapshot, seq }) {
    const store = await backend();
    if (seq) await store.apply({ puts: [[key, snapshot], [`${key}-seq`, seq]] });
    else await store.save(key, snapshot);
  }

  function flush(key) {
    if (debounce.has(key)) {
      timers.clearTimeout(debounce.get(key));
      debounce.delete(key);
    }
    if (running.has(key)) return running.get(key);
    if (!queued.has(key)) return Promise.resolve();
    const run = (async () => {
      while (queued.has(key)) {
        const next = queued.get(key);
        queued.delete(key);
        try {
          await save(key, next);
        } catch (err) {
          onError(key, err);
        }
      }
    })().finally(() => running.delete(key));
    running.set(key, run);
    return run;
  }

  return {
    schedule(key, snapshot, seq = 0) {
      if (stopped) return Promise.resolve();
      queued.set(key, { snapshot, seq });
      if (now.has(key)) {
        // Behind an in-flight save: its loop picks this one up next.
        const inFlight = running.get(key);
        return inFlight ? inFlight : flush(key);
      }
      if (debounce.has(key)) timers.clearTimeout(debounce.get(key));
      debounce.set(key, timers.setTimeout(() => {
        debounce.delete(key);
        void flush(key);
      }, delayMs));
      return Promise.resolve();
    },
    flushAll() {
      const keys = new Set([...queued.keys(), ...debounce.keys(), ...running.keys()]);
      return Promise.all([...keys].map(flush));
    },
    pending() {
      return queued.size + running.size;
    },
    /**
     * Drops every save not started yet and ignores later ones: another tab
     * took this tab's state by force and owns the store now. A save already
     * running finishes (it cannot be recalled).
     */
    stop() {
      stopped = true;
      for (const timer of debounce.values()) timers.clearTimeout(timer);
      debounce.clear();
      queued.clear();
    },
  };
}
