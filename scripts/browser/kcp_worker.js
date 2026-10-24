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
// back as {type: "persist", snapshot}.

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
let lastPersistLog = 0;
globalThis.__kcpPersist = (snapshot) => {
  const now = Date.now();
  if (now - lastPersistLog > 5000) {
    lastPersistLog = now;
    log("debug", `snapshot ${snapshot.length} bytes`);
  }
  self.postMessage({ type: "persist", snapshot });
};

// Side-effecting import: Go's wasm_exec.js defines globalThis.Go.
await import(init.execUrl);

// The real kcp server writes certs, keys and kubeconfigs. Give Go's syscall
// package a synchronous in-memory filesystem before the runtime starts.
const { installFsShim, makeMemoryDir } = await import("./fs_shim.js");
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
  self.postMessage({ type: "socialweb-persist", snapshot });
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
  self.postMessage({ type: "response", id: entry.id, response: JSON.parse(payload) });
};

const { fetchWasm, mib } = await import("./wasm_fetch.js");
const [wasmResponse, wasmBytes] = await fetchWasm(init.wasmUrl);
const go = new globalThis.Go();
const { instance } = await WebAssembly.instantiateStreaming(wasmResponse, go.importObject);
log("info", `kcp.wasm ${mib(wasmBytes)} MiB downloaded + compiled`);

// run() only settles when the Go program exits; the shard blocks forever.
go.run(instance).catch((err) => console.error("kcp-wasm exited:", err));

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

log("ok", `kcp ready in ${Math.round(performance.now() - started)}ms`);
self.postMessage({ type: "ready" });
