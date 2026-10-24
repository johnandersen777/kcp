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
  };
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
