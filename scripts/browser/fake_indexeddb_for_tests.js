// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// A small IndexedDB stand-in for the deno tests (fake-indexeddb is not a
// cached dependency and the tests must not fetch). It models what the owner
// fence relies on: readwrite transactions run one at a time, requests answer
// asynchronously, a transaction commits only when no request is pending
// after the last callback, and abort() drops every write of the transaction.
// Not served to the page.

export const fakeKeyRange = {
  bound: (lower, upper) => ({ lower, upper, includes: (k) => k >= lower && k <= upper }),
};

export function fakeIndexedDB() {
  const dbs = new Map();
  return {
    open(name) {
      const req = { result: null, error: null, onsuccess: null, onerror: null, onupgradeneeded: null };
      setTimeout(() => {
        let state = dbs.get(name);
        const fresh = !state;
        if (!state) {
          state = { data: new Map(), stores: new Set(), tail: Promise.resolve() };
          dbs.set(name, state);
        }
        req.result = database(state);
        if (fresh) req.onupgradeneeded?.();
        req.onsuccess?.();
      }, 0);
      return req;
    },
    /** A copy of the database's records. */
    dump(name) {
      return new Map(dbs.get(name)?.data ?? []);
    },
  };
}

function database(state) {
  return {
    objectStoreNames: { contains: (n) => state.stores.has(n) },
    createObjectStore: (n) => state.stores.add(n),
    transaction: (_name, mode = "readonly") => transaction(state, mode),
    close() {},
  };
}

function transaction(state, mode) {
  const tx = { error: null, oncomplete: null, onerror: null, onabort: null };
  let staged = null;
  let pending = 0;
  let started = false;
  let finished = false;
  let aborted = false;
  let release = null;
  const queued = [];
  const previous = state.tail;
  if (mode === "readwrite") state.tail = new Promise((resolve) => (release = resolve));
  previous.then(() => {
    started = true;
    staged = new Map(state.data);
    for (const run of queued.splice(0)) run();
    settle();
  });
  function settle() {
    setTimeout(() => {
      if (finished || !started || pending > 0) return;
      finished = true;
      if (aborted) tx.onabort?.();
      else {
        if (mode === "readwrite") state.data = staged;
        tx.oncomplete?.();
      }
      release?.();
    }, 0);
  }
  function request(op) {
    const req = { result: undefined, error: null, onsuccess: null, onerror: null };
    pending++;
    const run = () =>
      setTimeout(() => {
        if (!aborted) {
          req.result = op(staged);
          req.onsuccess?.({ target: req });
        }
        pending--;
        settle();
      }, 0);
    if (started) run();
    else queued.push(run);
    return req;
  }
  const keysOf = (data, key) => (key && typeof key.includes === "function" ? [...data.keys()].filter((k) => key.includes(k)) : [key]);
  const sortedIn = (data, range) => [...data.keys()].filter((k) => range.includes(k)).sort();
  tx.abort = () => {
    aborted = true;
  };
  tx.objectStore = () => ({
    get: (key) => request((data) => data.get(key)),
    put: (value, key) => {
      if (mode !== "readwrite") throw new Error("put in a readonly transaction");
      return request((data) => (data.set(key, value), key));
    },
    delete: (key) =>
      request((data) => {
        for (const k of keysOf(data, key)) data.delete(k);
      }),
    getAll: (range) => request((data) => sortedIn(data, range).map((k) => data.get(k))),
    getAllKeys: (range) => request((data) => sortedIn(data, range)),
  });
  return tx;
}
