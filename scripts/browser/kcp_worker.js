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

// kcp_worker.js runs the kcp WASM shard (kcp.wasm, GOOS=js GOARCH=wasm) in a
// browser Web Worker. The module registers kcpHandleRequest(requestJSON,
// callbackID) and calls __kcpResolve(callbackID, responseJSON) when done.
//
// The page sends {type: "request", id, request}; the worker answers with
// {type: "response", id, response}. Snapshots from __kcpPersist are posted
// back as {type: "persist", snapshot, seq}.
//
// Hibernation (kcp_hibernate.js): init.hibernate = {enabled, restore,
// persistSeq, gzip, store} restores a stored memory snapshot instead of booting when it
// matches; {type: "hibernate"} asks for a capture soon, {type:
// "snapshot-discard"} deletes it. A failed restore answers {type: "ready",
// restart} and the page starts a fresh worker that cold boots.

const init = await new Promise((resolve) => {
  const listener = (event) => {
    if (event.data?.type === "init") {
      self.removeEventListener("message", listener);
      resolve(event.data);
    }
  };
  self.addEventListener("message", listener);
});

const log = (level, message) => self.postMessage({ type: "log", level, message });
const started = performance.now();

globalThis.__kcpInitialState = init.initialState ?? "";
if (globalThis.__kcpInitialState) {
  log("info", `restoring snapshot (${globalThis.__kcpInitialState.length} bytes)`);
}
// Read by the Go entrypoint before the socialweb orchestrator starts.
if (init.socialweb) globalThis.__socialwebConfig = JSON.stringify(init.socialweb);

// Every persisted message is numbered (per channel, from one counter); the
// page stores the number with the data and a snapshot records the last ones
// it had posted, so a restore can tell which of the two is newer.
const seqs = { kcp: init.hibernate?.persistSeq?.kcp ?? 0, socialweb: init.hibernate?.persistSeq?.socialweb ?? 0 };
let nextSeq = Math.max(seqs.kcp, seqs.socialweb) + 1;
const takeSeq = (channel) => (seqs[channel] = nextSeq++);

let lastPersistLog = 0;
globalThis.__kcpPersist = (snapshot) => {
  const now = Date.now();
  if (now - lastPersistLog > 5000) {
    lastPersistLog = now;
    log("debug", `snapshot ${snapshot.length} bytes`);
  }
  self.postMessage({ type: "persist", snapshot, seq: takeSeq("kcp") });
};

// Side-effecting import: Go's wasm_exec.js defines globalThis.Go.
await import(init.execUrl);

// The real kcp server writes certs, keys and kubeconfigs. Give Go's syscall
// package a synchronous in-memory filesystem before the runtime starts.
const { installFsShim, makeMemoryDir, exportFsState, importFsState } = await import("./fs_shim.js");
if (!globalThis.process) globalThis.process = {};
try {
  globalThis.process.cwd = () => "/";
  globalThis.process.chdir = () => {};
} catch {
  // wasm_exec may expose a read-only process object; the absolute rootDir
  // keeps the boot working without cwd.
}
installFsShim();
for (const dir of ["/kcp", "/tmp", "/home", "/home/user"]) makeMemoryDir(dir);

// Go writes its stdout/stderr (klog included) to this worker's console, which
// browser automation that only sees the page (Safari's MCP) cannot read.
// localStorage kcp.debugGo=1 forwards it to the activity log at debug level.
if (init.debugGo) {
  for (const level of ["log", "error"]) {
    const original = console[level].bind(console);
    console[level] = (...args) => {
      original(...args);
      const text = args.map((a) => (a instanceof Error ? `${a}\n${a.stack ?? ""}` : String(a))).join(" ").trimEnd();
      if (text) log("debug", `go: ${text.slice(0, 4000)}`);
    };
  }
}

// Everything Go calls into is defined before it first runs: the status feed
// and the persist hooks fire as soon as a hot boot (or a restore) is up.
const pending = new Map();
let nextCallbackID = 1;

// callGo calls a (requestJSON, callbackID) Go export and resolves with its
// __kcpResolve answer; an answer carrying error rejects.
function callGo(fn, payload, timeoutMs = 30_000) {
  return new Promise((resolve, reject) => {
    if (typeof globalThis[fn] !== "function") return reject(new Error(`${fn} is not available`));
    const callbackID = nextCallbackID++;
    const timer = setTimeout(() => {
      if (pending.delete(callbackID)) reject(new Error(`${fn} timed out`));
    }, timeoutMs);
    pending.set(callbackID, {
      resolve(out) {
        clearTimeout(timer);
        if (out?.error) reject(new Error(String(out.error)));
        else resolve(out);
      },
    });
    globalThis[fn](JSON.stringify(payload), callbackID);
  });
}

// Relay ingress (ingress_manager.js): this worker holds one
// com.fedproxy.temp.xrpc.subscribe WebSocket per ingress Exposure host and
// answers its request frames through socialwebIngressRequest, which serves
// only that host's routes. Go owns the did:key; the page only shows status.
const { register: registerIngress, encodeBase64, decodeBase64 } = await import("./ingress_ws.js");
const { createIngressManager } = await import("./ingress_manager.js");
const ingress = createIngressManager({
  log,
  onStatus: (entries) => self.postMessage({ type: "socialweb-ingress-status", entries }),
  connect: (entry, hooks) => {
    const host = entry.host;
    return registerIngress({
      proxyHost: entry.proxyHost,
      did: entry.did,
      getServiceAuthToken: async (nsid) => (await callGo("socialwebIngressToken", { host, nsid })).token,
      signer: async (nonce) =>
        decodeBase64((await callGo("socialwebIngressSign", { host, nonce: encodeBase64(nonce) })).signature),
      handler: async (req) => {
        const res = await callGo("socialwebIngressRequest", {
          host, method: req.method, path: req.path, params: req.params,
          headers: req.headers, body: req.body, rawBody: req.rawBody,
        }, 60_000);
        return { status: res.status, body: res.body, contentType: res.contentType || "application/json", headers: res.headers };
      },
      ...hooks,
    });
  },
});
// Go pushes the ingress Exposures on every change. Applied once Go's call
// returns: connecting calls back into Go exports.
globalThis.__socialwebIngress = (json) => {
  const list = JSON.parse(json);
  queueMicrotask(() => ingress.apply(list));
};

// Socialweb vault state (OAuth session, pending PKCE, credentials) is small;
// the page persists it under the "socialweb" IndexedDB key.
globalThis.__socialwebPersist = (snapshot) => {
  if (typeof snapshot !== "string" || snapshot === "") return;
  self.postMessage({ type: "socialweb-persist", snapshot, seq: takeSeq("socialweb") });
};

let latestState = null;
globalThis.__kcpState = (json) => {
  try {
    latestState = JSON.parse(json);
    self.postMessage({ type: "state", state: latestState });
  } catch (err) {
    console.error("kcp-wasm: bad state snapshot", err);
  }
};

globalThis.__kcpResolve = (callbackID, payload) => {
  const entry = pending.get(callbackID);
  if (!entry) {
    console.error(`kcp-wasm: response for unknown callback ${callbackID}`);
    return;
  }
  pending.delete(callbackID);
  if (entry.resolve) return entry.resolve(JSON.parse(payload));
  self.postMessage({ type: "response", id: entry.id, response: JSON.parse(payload) });
};

const { fetchWasm, mib } = await import("./wasm_fetch.js");
const hibernate = await import("./kcp_hibernate.js");
const go = hibernate.instrument(new globalThis.Go());
// Low-memory mode (iOS): collect four times as often to keep the heap small. No
// GOMEMLIMIT: kcp's live heap is near 400 MiB, and a soft limit below it makes
// the single-threaded runtime collect continuously and never yield to JS.
// KCP_LOW_MEMORY turns off the apiserver watch cache and shortens the storage
// watch history (contrib/wasm/real). KCP_GOGC_READY collects harder once kcp is
// up, while socialweb composes: settled memory ~417 -> ~395 MiB in Deno, at
// the cost of a slower compose (~38 -> ~56 s). 10 starved the iPhone once the
// event streams decode records (heap ~280 MiB, ~100 collections a minute, kcp
// GETs taking 4 s and PDS calls timing out), so it is 25 unless the page
// passes gogcReady (localStorage kcp.gogcReady). KCP_SNAPSHOT_COMPACT_SECONDS=0
// drops the periodic full state snapshot (a 6 MB encode every 10 min, each
// raising the wasm memory peak a step); deltas still persist.
if (init.lowMemory) {
  go.env = {
    ...go.env, GOGC: "25", KCP_LOW_MEMORY: "1", KCP_GOGC_READY: String(init.gogcReady ?? 25),
    KCP_SNAPSHOT_COMPACT_SECONDS: "0",
  };
}
// With the Go console forwarded (debug), log Go's memory stats every minute.
if (init.debugGo) go.env = { ...go.env, KCP_MEMSTATS: "60" };
// Debug only (localStorage kcp.heapProfile=<seconds>, dev server only): Go
// hands heap and goroutine profiles to __kcpHeapDump on that period; the
// page uploads them.
if (init.heapProfile) {
  go.env = { ...go.env, KCP_HEAP_PROFILE: String(init.heapProfile) };
  globalThis.__kcpHeapDump = (_stats, heapBase64, goroutineBase64) =>
    postMessage({ type: "heap-profile", heapBase64, goroutineBase64 });
}
const fetchStarted = performance.now();
// The module hash only keys the snapshot; skip it (and its buffered copy)
// when hibernation is off.
const [wasmResponse, wasmBytes, wasmHash] = await fetchWasm(init.wasmUrl, fetch, { hash: Boolean(init.hibernate?.enabled) });
const module = await WebAssembly.compileStreaming(wasmResponse);
const compileMs = Math.round(performance.now() - fetchStarted);
log("info", `kcp.wasm ${mib(wasmBytes)} MiB downloaded + compiled in ${compileMs}ms`);

const snapshotKey = await hibernate.snapshotKey(
  await wasmHash,
  init.execUrl,
  JSON.stringify({ ...init.socialweb, restore: undefined }),
);
const restored = await restoreSnapshot();
if (init.debugMemory) {
  // Linear memory only grows; its size is the best crash predictor on iOS.
  setInterval(() => {
    const mem = globalThis.__kcpMemory?.buffer?.byteLength;
    if (mem) log("debug", `kcp wasm memory ${mib(mem)} MiB`);
  }, 2000);
}
if (!restored) {
  const instance = await WebAssembly.instantiate(module, go.importObject);
  globalThis.__kcpMemory = instance.exports.mem;
  // run() only settles when the Go program exits; the shard blocks forever.
  go.run(instance).catch((err) => console.error("kcp-wasm exited:", err));
}

for (let i = 0; i < 7200; i++) {
  if (typeof globalThis.kcpHandleRequest === "function") break;
  if (globalThis.__kcpBootError) break;
  if (i % 200 === 0 && i > 0) log("info", `waiting for real kcp readiness (${i / 40}s)`);
  await new Promise((resolve) => setTimeout(resolve, 25));
}
if (globalThis.__kcpBootError) {
  throw new Error("kcp-wasm boot failed: " + globalThis.__kcpBootError);
}
if (typeof globalThis.kcpHandleRequest !== "function") {
  throw new Error("kcp-wasm did not register kcpHandleRequest");
}
log("ok", "real kcp bridge registered");

self.onmessage = (event) => {
  const { type, id, request } = event.data ?? {};
  if (handleHibernateMessage(type)) return;
  if (type === "state-subscribe") {
    if (latestState) self.postMessage({ type: "state", state: latestState });
    return;
  }
  // Page-only Go exports (vault writes, sign-out). Answered like "request"
  // through __kcpResolve. The relay ingress exports stay in this worker.
  if (type === "socialweb-call" && typeof id === "number") {
    const { fn, payload } = event.data;
    const allowed = ["socialwebVaultPut", "socialwebSignOut"];
    if (!allowed.includes(fn) || typeof globalThis[fn] !== "function") {
      self.postMessage({ type: "response", id, error: `${fn} is not available` });
      return;
    }
    const callbackID = nextCallbackID++;
    pending.set(callbackID, { id });
    globalThis[fn](String(payload ?? ""), callbackID);
    return;
  }
  if (type !== "request" || typeof id !== "number") return;

  const callbackID = nextCallbackID++;
  pending.set(callbackID, { id });
  try {
    // The socialweb handler owns exposure routes and falls back to kcp for
    // everything else; it is registered only after the app composes.
    const handler = typeof globalThis.socialwebHandleRequest === "function"
      ? globalThis.socialwebHandleRequest
      : globalThis.kcpHandleRequest;
    handler(JSON.stringify(request), callbackID);
  } catch (err) {
    pending.delete(callbackID);
    self.postMessage({
      type: "response",
      id,
      error: err instanceof Error ? err.message : String(err),
    });
  }
};

let hibernation = null;
if (restored) await finishRestore();
// The restored program announced its ingress Exposures to a worker that is
// gone; ask for the current list to reopen the relay subscriptions.
if (restored && typeof globalThis.socialwebIngressList === "function") {
  callGo("socialwebIngressList", {})
    .then((out) => ingress.apply(out?.ingress ?? []))
    .catch((err) => log("warn", `ingress list after restore: ${err?.message ?? err}`));
}
const readyMs = Math.round(performance.now() - started);
log("ok", `kcp ready in ${readyMs}ms`);
self.postMessage({ type: "ready", restored: restored?.stats ?? null, compileMs, readyMs });
hibernation = startHibernationLoop();

// --- hibernation ---------------------------------------------------------------

// restoreSnapshot resumes a stored snapshot into go when it is usable, or
// returns null to cold boot. A failure after the restore started restarts
// the worker (finishRestore covers failures once Go runs).
async function restoreSnapshot() {
  if (!init.hibernate?.enabled || init.hibernate.restore === false) return null;
  try {
    const r = await hibernate.tryRestore({
      go,
      module,
      key: snapshotKey,
      seqs,
      store: hibernate.pickStore(init.hibernate.store),
      log,
      before(meta) {
        importFsState(meta.extra.fs);
        latestState = meta.extra.latestState ?? null;
      },
    });
    if (r) log("info", `snapshot restored: ${JSON.stringify(r.stats)}`);
    return r;
  } catch (err) {
    await restartCold(err);
  }
}

// finishRestore lets Go run and checks that the restored kcp answers.
// A trap or exit in the restored program fails it at once instead of
// reaching the page as a worker crash.
async function finishRestore() {
  const t0 = performance.now();
  let onError;
  try {
    const failed = new Promise((_, reject) => {
      onError = (event) => {
        event.preventDefault();
        reject(event.error ?? new Error(event.message));
      };
      self.addEventListener("error", onError);
      go._exitPromise.then(() => reject(new Error(`Go exited (code ${go.exitCode})`)));
    });
    restored.resume();
    const readyz = new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error("/readyz timed out")), 30_000);
      const callbackID = nextCallbackID++;
      pending.set(callbackID, {
        resolve(r) {
          clearTimeout(timer);
          resolve(r);
        },
      });
      globalThis.kcpHandleRequest(JSON.stringify({ method: "GET", path: "/readyz" }), callbackID);
    });
    const response = await Promise.race([readyz, failed]);
    if (response.status !== 200) throw new Error(`/readyz ${response.status}: ${response.body ?? ""}`);
    restored.stats.firstReadyzMs = Math.round(performance.now() - t0);
  } catch (err) {
    await restartCold(err);
  } finally {
    self.removeEventListener("error", onError);
  }
}

async function restartCold(err) {
  console.error("kcp-wasm: snapshot restore failed", err);
  log("warn", `snapshot restore failed, cold booting: ${err?.message ?? err}`);
  await hibernate.discardSnapshot().catch(() => {});
  self.postMessage({ type: "ready", restart: String(err?.message ?? err) });
  await new Promise(() => {}); // the page terminates this worker
}

function startHibernationLoop() {
  if (!init.hibernate?.enabled) return null;
  return hibernate.startHibernation({
    go,
    key: snapshotKey,
    captured: restored ? { ...seqs } : null,
    gzip: init.hibernate.gzip === true,
    store: hibernate.pickStore(init.hibernate.store),
    log,
    ready: () => typeof globalThis.socialwebHandleRequest === "function" && pending.size === 0,
    seqs: () => ({ ...seqs }),
    extra: () => ({ fs: exportFsState(), latestState }),
  });
}

function handleHibernateMessage(type) {
  if (type === "hibernate") {
    hibernation?.request();
    return true;
  }
  if (type === "snapshot-discard") {
    (hibernation?.discard() ?? hibernate.discardSnapshot())
      .then(() => log("info", "snapshot discarded; capture is off until reload"))
      .catch((err) => log("warn", `snapshot discard failed: ${err?.message ?? err}`));
    return true;
  }
  return false;
}
