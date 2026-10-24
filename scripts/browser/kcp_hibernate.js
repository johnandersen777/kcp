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
// Any other problem deletes the snapshot and cold boots.
//
// Re-capture: whenever the sequence moved since the last capture, the worker
// is idle and the state has been still for QUIET_MS, at most every EVERY_MS;
// right away when the page asks (visibilitychange to hidden).

import { busy, capture, instrument, restore, SNAPSHOT_FORMAT } from "./wasm_snapshot.js";

export { instrument };

const DIR = "kcp-snapshot";
const IDB = "kcp-snapshot";
// ponytail: fixed interval; back off if long sessions keep the state dirty.
const EVERY_MS = 60 * 1000;
// Capture once the persisted state has been still this long (boot and the
// socialweb compose write in bursts), unless the page is going away.
const QUIET_MS = 30 * 1000;
const CHUNK = 4 << 20;

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
  async save(go, extra, gzip) {
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
  async save(go, extra, gzip) {
    const { meta, memory } = capture(go, extra());
    const t0 = performance.now();
    // new Blob() copies synchronously, so the stored memory is consistent.
    let blob = new Blob([gzip ? memory.slice() : memory]);
    meta.stats.copyMs = ms(t0);
    if (gzip) blob = await new Response(memStream(blob, false).pipeThrough(new CompressionStream("gzip"))).blob();
    Object.assign(meta, { stored: blob.size, gzip });
    await idbDo("readwrite", (s) => {
      s.put(blob, "mem");
      return s.put(meta, "meta");
    });
    meta.stats.writeMs = ms(t0);
    return meta;
  },
  discard() {
    return idbDo("readwrite", (s) => s.clear()).catch(() => {});
  },
};

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
export async function tryRestore({ go, module, key, seqs, store = pickStore(), log, before }) {
  const t0 = performance.now();
  let meta;
  try {
    meta = await store.readMeta();
    if (!meta) return null;
    const why = meta.extra?.key !== key
      ? "the module, wasm_exec.js or the boot configuration changed"
      : !sameSeqs(meta.extra?.seqs, seqs)
      ? `persisted state is newer (snapshot ${JSON.stringify(meta.extra?.seqs)}, state ${JSON.stringify(seqs)})`
      : "";
    if (why) {
      log("info", `snapshot discarded: ${why}`);
      await store.discard();
      return null;
    }
  } catch (err) {
    log("warn", `snapshot unreadable, cold boot: ${err?.message ?? err}`);
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
export function startHibernation({ go, key, ready, seqs, extra, captured = null, gzip = false, store = pickStore(), log }) {
  let lastSeqs = captured;
  let lastAt = 0;
  let want = false;
  let running = false;
  let disabled = false;
  let lastReason = "";
  let seen = seqs();
  let changedAt = Date.now();

  const tick = async () => {
    if (disabled || running) return;
    const current = seqs();
    if (!sameSeqs(current, seen)) {
      seen = current;
      changedAt = Date.now();
    }
    if (lastSeqs && sameSeqs(lastSeqs, current)) return;
    if (!want && Date.now() - changedAt < QUIET_MS) return;
    if (!want && lastAt && Date.now() - lastAt < EVERY_MS) return;
    const reason = !ready() ? "kcp or socialweb not ready, or a request is in flight" : busy(go);
    if (reason) {
      if (reason !== lastReason) log("debug", `snapshot deferred: ${reason}`);
      lastReason = reason;
      return;
    }
    running = true;
    const t0 = performance.now();
    try {
      const meta = await store.save(go, () => ({ ...extra(), seqs: seqs(), key }), gzip);
      lastSeqs = meta.extra.seqs;
      lastAt = Date.now();
      want = false;
      log("info", `snapshot saved to ${store.kind}: ${(meta.bytes / 1048576).toFixed(0)} MiB memory, ` +
        `${(meta.stored / 1048576).toFixed(0)} MiB stored${gzip ? " (gzip)" : ""}, capture ${meta.stats.captureMs}ms, ` +
        `copy ${meta.stats.copyMs ?? 0}ms, write ${meta.stats.writeMs}ms, total ${ms(t0)}ms`);
    } catch (err) {
      log("warn", `snapshot failed: ${err?.message ?? err}`);
      if (err?.name === "QuotaExceededError") disabled = true;
    } finally {
      running = false;
    }
  };
  setInterval(tick, 1000);
  return {
    request() {
      want = true;
      void tick();
    },
    async discard() {
      disabled = true;
      await discardSnapshot();
    },
  };
}
