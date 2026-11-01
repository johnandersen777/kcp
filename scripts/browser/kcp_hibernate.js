/*
Copyright 2026 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Per-user hibernate/restore of the kcp worker's Go instance.
//
// The first visit cold boots. Once kcp and socialweb are up and the worker is
// idle, the worker captures its Go instance (wasm_snapshot.js) and stores it
// in this browser only: OPFS through a sync access handle or, where OPFS sync
// handles are missing, IndexedDB. Nothing is ever shipped with the build: a
// snapshot holds this user's keys and tokens (contrib/wasm/FUTURE_WORK.md).
//
// gzip (the default) copies memory (~0.1s) and compresses while Go keeps
// running (~4s, ~780 -> ~90 MiB) and inflates in ~0.5s on restore; raw writes
// the live memory in one ~0.3s blocking write and reads back in ~0.1-0.2s.
//
// A snapshot is used only when
//   - its key matches: SHA-256 of the module as fetched, of wasm_exec.js, of
//     the boot configuration, and the snapshot format, and
//   - its persist sequence numbers equal the persisted JSON state's: the
//     worker numbers every kcp storage message and socialweb vault message,
//     the page saves each number with the data it belongs to, and a snapshot
//     records the last number it had posted per channel. Equal means the
//     snapshot's memory holds exactly the persisted state; anything else
//     (newer JSON, or JSON that lost a message) cold boots from JSON.
//   - with the owner fence (state_store.js claimOwner), it was written by
//     the tab that owned the saved state when this load started. Two tabs
//     number their persists from the same stored base, so after a forced
//     take over a stale holder's capture could carry the very seqs the new
//     owner persisted; the owner token tells them apart. A capture also
//     checks, right before it commits, that its tab still owns the state.
// Any other problem deletes the snapshot and cold boots.
//
// Re-capture: whenever the sequence moved since the last capture, the worker
// is idle and the state has been still for QUIET_MS, at most every EVERY_MS
// (doubling up to MAX_EVERY_MS while only background writes come); right
// away when the page asks (visibilitychange to hidden).

import { busy, capture, instrument, restore, SNAPSHOT_FORMAT } from "./wasm_snapshot.js";

export { instrument };

const DIR = "kcp-snapshot";
const IDB = "kcp-snapshot";
// ponytail: fixed interval; back off if long sessions keep the state dirty.
const EVERY_MS = 60 * 1000;
// Background captures (no flush or hide in between) double the gap up to
// this: an idle tab writes every 5-10 s, and a capture a minute is ~5 GB/h
// of writes on a phone (re-check page-rc131b f360fdd27, 2).
const MAX_EVERY_MS = 10 * 60 * 1000;
// Capture once the persisted state has been still this long (boot and the
// socialweb compose write in bursts), unless the page is going away. A live
// page persists a write every 5-10 s even when idle, so a longer quiet period
// never came and an idle tab never captured; a hide capture (~5 s) does not
// finish before a reload or close unloads the page (page-rc131 dev boot
// timing: three cold boots in a row).
const QUIET_MS = 3 * 1000;
const CHUNK = 4 << 20;

/** The kcp-wasm state database's owner key (state_store.js OWNER_KEY). */
export const OWNER_KEY = "owner";

/**
 * The owner token in the page's state database dbName, or null (no database,
 * no token, any error). Never creates the database.
 */
export function readOwner(dbName, factory = globalThis.indexedDB) {
  return new Promise((resolve) => {
    if (!factory || !dbName) return resolve(null);
    let req;
    try {
      req = factory.open(dbName);
    } catch {
      return resolve(null);
    }
    req.onupgradeneeded = () => req.transaction?.abort(); // it did not exist: leave it so
    req.onerror = () => resolve(null);
    req.onsuccess = () => {
      const db = req.result;
      const done = (value) => {
        db.close();
        resolve(value);
      };
      if (!db.objectStoreNames.contains("state")) return done(null);
      try {
        const get = db.transaction("state", "readonly").objectStore("state").get(OWNER_KEY);
        get.onsuccess = () => done(get.result ?? null);
        get.onerror = () => done(null);
      } catch {
        done(null);
      }
    };
  });
}

function notOwner(message) {
  const err = new Error(message);
  err.name = "NotOwnerError";
  return err;
}

/**
 * Why the stored snapshot meta must not be restored, or "" when it may: key,
 * seqs and, when owner is not undefined (the fence is on), the owner token
 * the saved state had when this load started.
 */
// keyChanges names what differs between two snapshotKey values (module,
// wasm_exec.js, boot configuration) and, given both loads' per-field config
// hashes (confHashes), which configuration fields.
export function keyChanges(oldKey, newKey, oldConf, newConf) {
  const [oldFormat, oldWasm, oldExec, oldConfHash] = String(oldKey ?? "").split("-");
  const [newFormat, newWasm, newExec, newConfHash] = String(newKey ?? "").split("-");
  const parts = [];
  if (oldFormat !== newFormat) parts.push("snapshot format");
  if (oldWasm !== newWasm) parts.push("module");
  if (oldExec !== newExec) parts.push("wasm_exec.js");
  if (oldConfHash !== newConfHash) {
    const fields = oldConf && newConf
      ? [...new Set([...Object.keys(oldConf), ...Object.keys(newConf)])].filter((k) => oldConf[k] !== newConf[k]).sort()
      : [];
    parts.push(fields.length ? `boot configuration (${fields.join(", ")})` : "boot configuration");
  }
  return parts.length ? parts.join(", ") : "the key";
}

export function snapshotRejection(meta, { key, seqs, owner, confHashes }) {
  if (meta?.extra?.key !== key) return `changed since the capture: ${keyChanges(meta?.extra?.key, key, meta?.extra?.confHashes, confHashes)}`;
  if (!sameSeqs(meta.extra?.seqs, seqs)) {
    return `persisted state is newer (snapshot ${JSON.stringify(meta.extra?.seqs)}, state ${JSON.stringify(seqs)})`;
  }
  if (owner !== undefined && (meta.extra?.owner ?? null) !== owner) {
    return "written by a tab that no longer owned the saved state";
  }
  return "";
}

const hex = (buf) => Array.from(new Uint8Array(buf), (b) => b.toString(16).padStart(2, "0")).join("");
const ms = (t0) => Math.round(performance.now() - t0);

export async function sha256(bytes) {
  return hex(await crypto.subtle.digest("SHA-256", bytes));
}

// config is the boot configuration the Go program read once at start
// (socialweb identity, origin, ...): a different one needs a cold boot.
export async function snapshotKey(wasmHash, execUrl, config = "") {
  const exec = await (await fetch(execUrl)).arrayBuffer();
  const conf = await sha256(new TextEncoder().encode(config));
  return `f${SNAPSHOT_FORMAT}-${wasmHash}-${await sha256(exec)}-${conf.slice(0, 16)}`;
}

// Copies a (decompressed) stream into view; returns the byte count.
async function streamInto(stream, view) {
  const reader = stream.getReader();
  let at = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) return at;
    if (at + value.byteLength > view.byteLength) throw new Error("snapshot memory is larger than recorded");
    view.set(value, at);
    at += value.byteLength;
  }
}

// gzip of a stable copy; Go keeps running while the chunks compress.
function gzipStream(copy) {
  const cs = new CompressionStream("gzip");
  const writer = cs.writable.getWriter();
  (async () => {
    for (let off = 0; off < copy.byteLength; off += CHUNK) {
      await writer.write(copy.subarray(off, off + CHUNK));
    }
    await writer.close();
  })().catch(() => {});
  return cs.readable;
}

function memStream(blob, gzip) {
  return gzip ? blob.stream().pipeThrough(new DecompressionStream("gzip")) : blob.stream();
}

// --- OPFS --------------------------------------------------------------------

const opfs = {
  kind: "opfs",
  async dir(create = false) {
    const root = await navigator.storage.getDirectory();
    return root.getDirectoryHandle(DIR, { create });
  },
  async readMeta() {
    try {
      const file = await (await (await this.dir()).getFileHandle("meta.json")).getFile();
      return JSON.parse(await file.text());
    } catch (err) {
      if (err?.name === "NotFoundError") return null;
      throw err;
    }
  },
  async fill(meta, view) {
    const fh = await (await this.dir()).getFileHandle(meta.memFile);
    if (meta.gzip) return streamInto(memStream(await fh.getFile(), true), view);
    const handle = await fh.createSyncAccessHandle();
    try {
      return handle.read(view, { at: 0 });
    } finally {
      handle.close();
    }
  },
  // save captures inside, after the last await, so Go cannot run between the
  // capture and the raw write.
  async save(go, extra, gzip, beforeCommit = async () => {}) {
    const dir = await this.dir(true);
    const memFile = `mem-${Date.now()}.${gzip ? "gz" : "bin"}`;
    const handle = await (await dir.getFileHandle(memFile, { create: true })).createSyncAccessHandle();
    let meta;
    try {
      const snap = capture(go, extra());
      meta = snap.meta;
      const t0 = performance.now();
      if (gzip) {
        const copy = snap.memory.slice();
        meta.stats.copyMs = ms(t0);
        let at = 0;
        const reader = gzipStream(copy).getReader();
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          at += handle.write(value, { at });
        }
        meta.stored = at;
      } else {
        meta.stored = handle.write(snap.memory, { at: 0 });
      }
      handle.truncate(meta.stored);
      handle.flush();
      meta.stats.writeMs = ms(t0);
    } catch (err) {
      handle.close();
      await dir.removeEntry(memFile).catch(() => {});
      throw err;
    }
    handle.close();
    try {
      await beforeCommit();
    } catch (err) {
      await dir.removeEntry(memFile).catch(() => {});
      throw err;
    }
    Object.assign(meta, { memFile, gzip });
    // createWritable swaps the file in on close(): the old meta.json (and the
    // mem file it names) stays valid until the new one is complete.
    const writable = await (await dir.getFileHandle("meta.json", { create: true })).createWritable();
    await writable.write(JSON.stringify(meta));
    await writable.close();
    for await (const name of dir.keys()) {
      if (name.startsWith("mem-") && name !== memFile) await dir.removeEntry(name).catch(() => {});
    }
    return meta;
  },
  async writeMeta(meta) {
    const writable = await (await (await this.dir(true)).getFileHandle("meta.json", { create: true })).createWritable();
    await writable.write(JSON.stringify(meta));
    await writable.close();
  },
  async discard() {
    const root = await navigator.storage.getDirectory();
    await root.removeEntry(DIR, { recursive: true }).catch((err) => {
      if (err?.name !== "NotFoundError") throw err;
    });
  },
};

// --- IndexedDB (fallback) ------------------------------------------------------

function idbOpen() {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(IDB, 1);
    req.onupgradeneeded = () => req.result.createObjectStore("s");
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

async function idbDo(mode, fn) {
  const db = await idbOpen();
  try {
    return await new Promise((resolve, reject) => {
      const tx = db.transaction("s", mode);
      const out = fn(tx.objectStore("s"));
      tx.oncomplete = () => resolve(out.result);
      tx.onerror = tx.onabort = () => reject(tx.error);
    });
  } finally {
    db.close();
  }
}

const idb = {
  kind: "indexeddb",
  readMeta() {
    return idbDo("readonly", (s) => s.get("meta")).then((m) => m ?? null);
  },
  async fill(meta, view) {
    const blob = await idbDo("readonly", (s) => s.get("mem"));
    if (!blob) throw new Error("snapshot memory is missing");
    return streamInto(memStream(blob, meta.gzip), view);
  },
  async save(go, extra, gzip, beforeCommit = async () => {}) {
    const { meta, memory } = capture(go, extra());
    const t0 = performance.now();
    // new Blob() copies synchronously, so the stored memory is consistent.
    let blob = new Blob([gzip ? memory.slice() : memory]);
    meta.stats.copyMs = ms(t0);
    if (gzip) blob = await new Response(memStream(blob, false).pipeThrough(new CompressionStream("gzip"))).blob();
    Object.assign(meta, { stored: blob.size, gzip });
    await beforeCommit();
    await idbDo("readwrite", (s) => {
      s.put(blob, "mem");
      return s.put(meta, "meta");
    });
    meta.stats.writeMs = ms(t0);
    return meta;
  },
  writeMeta(meta) {
    return idbDo("readwrite", (s) => s.put(meta, "meta"));
  },
  discard() {
    return idbDo("readwrite", (s) => s.clear()).catch(() => {});
  },
};

/**
 * After a restore by a new owner (its memory is exactly the persisted state:
 * the seqs matched), stamps the stored snapshot with that owner's token so
 * the next load can use it without a fresh capture first.
 */
export async function restampOwner(store, token, restored = null) {
  const meta = await store.readMeta();
  if (!meta?.extra || meta.extra.owner === token) return false;
  // Only the snapshot this load restored (a capture may have committed since).
  if (restored && (meta.wallMs !== restored.wallMs || meta.memFile !== restored.memFile ||
    !sameSeqs(meta.extra.seqs, restored.extra?.seqs))) return false;
  meta.extra.owner = token;
  await store.writeMeta(meta);
  return true;
}

export function pickStore(kind) {
  const hasOpfs = typeof globalThis.FileSystemFileHandle?.prototype?.createSyncAccessHandle === "function" &&
    typeof navigator.storage?.getDirectory === "function";
  if (kind === "indexeddb" || !hasOpfs) return idb;
  return opfs;
}

// discardSnapshot removes every stored snapshot (both backends). Safe from the
// page as long as no worker holds a sync access handle.
export async function discardSnapshot() {
  const results = await Promise.allSettled([
    typeof navigator.storage?.getDirectory === "function" ? opfs.discard() : null,
    globalThis.indexedDB ? idb.discard() : null,
  ]);
  const failed = results.find((r) => r.status === "rejected");
  if (failed) throw failed.reason;
}

const sameSeqs = (a, b) => (a?.kcp ?? 0) === (b?.kcp ?? 0) && (a?.socialweb ?? 0) === (b?.socialweb ?? 0);

/**
 * Restores the stored snapshot into go (instrumented, never run) when it is
 * usable. Returns null to cold boot (nothing was touched yet) or
 * {meta, stats, resume}; Go runs once resume() is called. Throws after the restore started: the caller must discard
 * the snapshot and cold boot in a fresh worker.
 *
 * before(meta) runs right before the instance is rebuilt (fs state, JS state).
 */
export async function tryRestore({ go, module, key, seqs, owner = undefined, confHashes, store = pickStore(), log, before, outcome = () => {} }) {
  const t0 = performance.now();
  let meta;
  try {
    meta = await store.readMeta();
    if (!meta) {
      outcome(`not restored: no snapshot stored (${store.kind})`);
      return null;
    }
    const why = snapshotRejection(meta, { key, seqs, owner, confHashes });
    if (why) {
      log("info", `snapshot discarded: ${why}`);
      outcome(`not restored: discarded, ${why}`);
      await store.discard();
      return null;
    }
  } catch (err) {
    log("warn", `snapshot unreadable, cold boot: ${err?.message ?? err}`);
    outcome(`not restored: unreadable, ${err?.message ?? err}`);
    await store.discard().catch(() => {});
    return null;
  }
  const readMetaMs = ms(t0);
  before(meta);
  let readBytes = 0;
  const r = await restore(go, module, meta, async (view) => {
    readBytes = await store.fill(meta, view);
    if (readBytes !== view.byteLength) throw new Error(`snapshot memory is ${readBytes} bytes, want ${view.byteLength}`);
  });
  if (r.missing.length) log("warn", `snapshot: unresolved JS refs ${r.missing.join(", ")}`);
  return {
    meta,
    resume: r.resume,
    stats: {
      store: store.kind,
      gzip: meta.gzip,
      storedMiB: +(meta.stored / 1048576).toFixed(1),
      memoryMiB: +(meta.bytes / 1048576).toFixed(1),
      gapS: Math.round((Date.now() - meta.wallMs) / 1000),
      readMetaMs,
      ...r.timings,
    },
  };
}

/**
 * Starts the capture loop. ready() is true when kcp and socialweb are up and
 * no request is in flight; seqs() returns the persist sequence numbers and
 * extra() the JSON-safe worker state to keep. captured is the seqs the stored
 * snapshot already holds (after a restore), or null.
 */
// A capture asked for after a kcp flush (requestSoon) waits for the seqs to
// be still this long and for this gap since the last capture: every phase
// change and action flushes, and each capture copies the whole memory
// (~780 MiB, ~4 s gzip), an iOS tab-kill risk if they come back to back
// (review page-rc131 2e7d88ba5, 1). A hide (request) still captures at once.
export const SOON_QUIET_MS = 3 * 1000;
export const SOON_GAP_MS = 15 * 1000;
// A hide flushes and asks for a capture together; the flush can persist after
// the hide's tick found nothing new. A flush this soon after a hide still
// captures at once (re-check page-rc131 fb6899fe2): iOS freezes a hidden tab
// within seconds.
export const HIDE_FLUSH_MS = 10 * 1000;

export function startHibernation({
  go, key, ready, seqs, extra, captured = null, gzip = false, store = pickStore(), log, owner = null,
  timers = globalThis, now = Date.now,
}) {
  let lastSeqs = captured;
  // Boot grace: the first background capture waits a full EVERY_MS, so it
  // does not land during boot and the socialweb compose at peak heap
  // (re-check page-rc131b f360fdd27, 1).
  let lastAt = now();
  let everyMs = EVERY_MS;
  let want = false; // a hide: capture as soon as kcp is idle
  let soon = false; // a flush: capture once still, at most every SOON_GAP_MS
  let hideAt = -Infinity;
  // Asks made during a capture are not answered by it (its seqs are older):
  // wantAsks counts the ones that set want (a hide, or a flush right after
  // one), asks every ask. Only a hide keeps want across a capture; a flush
  // during a capture keeps soon, which stays throttled (re-check page-rc131b
  // 64ea075ce: one hide plus a flush every few seconds chained captures).
  let asks = 0;
  let wantAsks = 0;
  let running = null; // the in-flight save
  let disabled = false;
  let lastReason = "";
  let seen = seqs();
  let changedAt = now();

  const tick = async () => {
    if (disabled || running) return;
    const current = seqs();
    if (!sameSeqs(current, seen)) {
      seen = current;
      changedAt = now();
    }
    if (lastSeqs && sameSeqs(lastSeqs, current)) {
      // Nothing new since the last capture: a request is answered.
      want = false;
      soon = false;
      return;
    }
    const t = now();
    if (want) {
      // a hide: at once
    } else if (soon) {
      if (t - changedAt < SOON_QUIET_MS || (lastAt && t - lastAt < SOON_GAP_MS)) return;
    } else {
      if (t - changedAt < QUIET_MS) return;
      if (t - lastAt < everyMs) return;
    }
    const reason = !ready() ? "kcp or socialweb not ready, or a request is in flight" : busy(go);
    if (reason) {
      if (reason !== lastReason) log("debug", `snapshot deferred: ${reason}`);
      lastReason = reason;
      return;
    }
    const background = !want && !soon;
    const asked = asks;
    const wantAsked = wantAsks;
    const startedAt = now();
    let done;
    running = new Promise((resolve) => (done = resolve));
    const t0 = performance.now();
    // owner: {token, current()}; the capture is this tab's only while the
    // saved state's owner is still its token, checked before it starts and
    // again right before it commits.
    const stillOwner = async () => {
      if (owner && (await owner.current()) !== owner.token) throw notOwner("another tab owns the saved state now");
    };
    try {
      await stillOwner();
      const meta = await store.save(go, () => ({ ...extra(), seqs: seqs(), key, owner: owner?.token ?? null }), gzip, stillOwner);
      lastSeqs = meta.extra.seqs;
      lastAt = now();
      if (wantAsks === wantAsked) {
        want = false;
        hideAt = -Infinity; // one capture per hide
      } else if (hideAt <= startedAt) {
        // This capture already followed the hide; the hide's flush that
        // landed during it gets one more capture, later flushes are ordinary.
        hideAt = -Infinity;
      }
      if (asks === asked) {
        soon = false;
        everyMs = background ? Math.min(everyMs * 2, MAX_EVERY_MS) : EVERY_MS;
      }
      log("info", `snapshot saved to ${store.kind}: ${(meta.bytes / 1048576).toFixed(0)} MiB memory, ` +
        `${(meta.stored / 1048576).toFixed(0)} MiB stored${gzip ? " (gzip)" : ""}, capture ${meta.stats.captureMs}ms, ` +
        `copy ${meta.stats.copyMs ?? 0}ms, write ${meta.stats.writeMs}ms, total ${ms(t0)}ms`);
    } catch (err) {
      if (err?.name === "NotOwnerError") {
        disabled = true;
        log("warn", `snapshot capture stopped: ${err.message}`);
      } else {
        log("warn", `snapshot failed: ${err?.message ?? err}`);
      }
      if (err?.name === "QuotaExceededError") disabled = true;
      // A failed capture counts for the gaps: no retry every second.
      lastAt = now();
      if (wantAsks === wantAsked) want = false;
    } finally {
      running = null;
      done();
    }
  };
  timers.setInterval(tick, 1000);
  return {
    /** The tab is being hidden: capture as soon as kcp is idle. */
    request() {
      asks++;
      wantAsks++;
      want = true;
      hideAt = now();
      everyMs = EVERY_MS;
      void tick();
    },
    /**
     * kcp just flushed: capture once the seqs were still SOON_QUIET_MS and
     * SOON_GAP_MS after the last capture, and only if they moved since it.
     * Within HIDE_FLUSH_MS of a hide it is the hide's flush: at once.
     */
    requestSoon() {
      if (lastSeqs && sameSeqs(lastSeqs, seqs())) return;
      asks++;
      everyMs = EVERY_MS;
      if (now() - hideAt < HIDE_FLUSH_MS) {
        wantAsks++;
        want = true;
        void tick();
        return;
      }
      soon = true;
    },
    async discard() {
      disabled = true;
      await running; // its sync access handle blocks removal
      await discardSnapshot();
    },
  };
}
