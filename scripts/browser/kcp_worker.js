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

globalThis.__kcpInitialState = init.initialState ?? "";
globalThis.__kcpPersist = (snapshot) => {
  self.postMessage({ type: "persist", snapshot });
};

// Side-effecting import: Go's wasm_exec.js defines globalThis.Go.
await import(init.execUrl);

const bytes = await (await fetch(init.wasmUrl)).arrayBuffer();
const go = new globalThis.Go();
const { instance } = await WebAssembly.instantiate(bytes, go.importObject);

// run() only settles when the Go program exits; the shard blocks forever.
go.run(instance).catch((err) => console.error("kcp-wasm exited:", err));

for (let i = 0; i < 400; i++) {
  if (typeof globalThis.kcpHandleRequest === "function") break;
  await new Promise((resolve) => setTimeout(resolve, 25));
}
if (typeof globalThis.kcpHandleRequest !== "function") {
  throw new Error("kcp-wasm did not register kcpHandleRequest");
}

const pending = new Map();
let nextCallbackID = 1;

globalThis.__kcpResolve = (callbackID, payload) => {
  const entry = pending.get(callbackID);
  if (!entry) {
    console.error(`kcp-wasm: response for unknown callback ${callbackID}`);
    return;
  }
  pending.delete(callbackID);
  self.postMessage({ type: "response", id: entry.id, response: JSON.parse(payload) });
};

self.onmessage = (event) => {
  const { type, id, request } = event.data ?? {};
  if (type !== "request" || typeof id !== "number") return;

  const callbackID = nextCallbackID++;
  pending.set(callbackID, { id });
  try {
    globalThis.kcpHandleRequest(JSON.stringify(request), callbackID);
  } catch (err) {
    pending.delete(callbackID);
    self.postMessage({
      type: "response",
      id,
      error: err instanceof Error ? err.message : String(err),
    });
  }
};

self.postMessage({ type: "ready" });
