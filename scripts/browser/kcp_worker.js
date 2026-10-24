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
// Go pushes the ingress Exposures the page must hold relay subscriptions for.
globalThis.__socialwebIngress = (json) => {
  self.postMessage({ type: "socialweb-ingress", ingress: JSON.parse(json) });
};

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

// Everything Go calls into is defined before it first runs: the status feed
// and the persist hooks fire as soon as a hot boot (or a restore) is up.
const pending = new Map();
let nextCallbackID = 1;

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
const fetchStarted = performance.now();
const [wasmResponse, wasmBytes, wasmHash] = await fetchWasm(init.wasmUrl, fetch, { hash: true });
const module = await WebAssembly.compileStreaming(wasmResponse);
const compileMs = Math.round(performance.now() - fetchStarted);
log("info", `kcp.wasm ${mib(wasmBytes)} MiB downloaded + compiled in ${compileMs}ms`);

const snapshotKey = await hibernate.snapshotKey(
  await wasmHash,
  init.execUrl,
  JSON.stringify({ ...init.socialweb, restore: undefined }),
);
const restored = await restoreSnapshot();
if (!restored) {
  const instance = await WebAssembly.instantiate(module, go.importObject);
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
  // Page-only Go exports (vault writes, relay signing and request frames).
  // Answered like "request" through __kcpResolve.
  if (type === "socialweb-call" && typeof id === "number") {
    const { fn, payload } = event.data;
    const allowed = ["socialwebVaultPut", "socialwebIngressSign",
      "socialwebIngressToken", "socialwebIngressRequest"];
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
async function finishRestore() {
  restored.resume();
  const t0 = performance.now();
  try {
    const response = await new Promise((resolve, reject) => {
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
    if (response.status !== 200) throw new Error(`/readyz ${response.status}: ${response.body ?? ""}`);
    restored.stats.firstReadyzMs = Math.round(performance.now() - t0);
  } catch (err) {
    await restartCold(err);
  }
}

async function restartCold(err) {
  console.error("kcp-wasm: snapshot restore failed", err);
  log("warn", `snapshot restore failed, cold booting: ${err?.message ?? err}`);
  await hibernate.pickStore(init.hibernate?.store).discard().catch(() => {});
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
    (hibernation?.discard() ?? hibernate.pickStore(init.hibernate?.store).discard())
      .then(() => log("info", "snapshot discarded; capture is off until reload"))
      .catch((err) => log("warn", `snapshot discard failed: ${err?.message ?? err}`));
    return true;
  }
  return false;
}
