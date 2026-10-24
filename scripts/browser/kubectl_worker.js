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

// kubectl_worker.js runs upstream kubectl (kubectl.wasm) in a browser Web
// Worker. Go's js/wasm net/http transport calls the global fetch() for every
// request; here fetch is intercepted for the virtual API server origin and
// forwarded to the kcp worker through the page:
//
//   kubectl.wasm -> fetch(apiServer/...) -> {type:"fetch"} -> page
//     page -> kcp worker {type:"request"} -> kcp.wasm
//     page <- {type:"fetchResult"} <- page <- {type:"response"}
//
// The browser has no filesystem, so fs_shim.js provides an in-memory
// globalThis.fs (discovery cache, probed kubeconfig paths, stdin).

import { installFsShim, setStdin } from "./fs_shim.js";

const init = await new Promise((resolve) => {
  const listener = (event) => {
    if (event.data?.type === "init") {
      self.removeEventListener("message", listener);
      resolve(event.data);
    }
  };
  self.addEventListener("message", listener);
});

// Side-effecting import: Go's wasm_exec.js defines globalThis.Go and a
// fallback globalThis.fs.
await import(init.execUrl);
installFsShim();

const apiServer = String(init.apiServer).replace(/\/$/, "");
const nativeFetch = globalThis.fetch.bind(globalThis);
const pendingFetch = new Map();
let nextFetchID = 1;

function base64ToBytes(encoded) {
  const binary = atob(encoded);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return bytes;
}

globalThis.fetch = async (input, options = {}) => {
  const url = typeof input === "string" ? input : String(input?.url ?? input);
  if (!url.startsWith(apiServer)) {
    return nativeFetch(input, options);
  }

  const parsed = new URL(url);
  const headers = {};
  if (options.headers) {
    for (const [key, value] of options.headers.entries()) headers[key] = value;
  }
  let body = "";
  if (options.body) {
    body = new TextDecoder().decode(
      options.body instanceof Uint8Array
        ? options.body
        : new Uint8Array(options.body),
    );
  }

  const id = nextFetchID++;
  return await new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      if (pendingFetch.delete(id)) reject(new TypeError("fetch bridge timed out"));
    }, 120_000);
    pendingFetch.set(id, { resolve, reject, timer });
    self.postMessage({
      type: "fetch",
      id,
      request: {
        method: options.method ?? "GET",
        path: parsed.pathname + parsed.search,
        headers,
        body,
      },
    });
  });
};

const bytes = await (await nativeFetch(init.wasmUrl)).arrayBuffer();
const go = new globalThis.Go();
const { instance } = await WebAssembly.instantiate(bytes, go.importObject);

go.run(instance).catch((err) => console.error("kubectl-wasm exited:", err));

for (let i = 0; i < 800; i++) {
  if (typeof globalThis.kubectlRun === "function") break;
  await new Promise((resolve) => setTimeout(resolve, 25));
}
if (typeof globalThis.kubectlRun !== "function") {
  throw new Error("kubectl-wasm did not register kubectlRun");
}

globalThis.__kubectlResolve = (id, payload) => {
  self.postMessage({ type: "result", id, result: JSON.parse(payload) });
};

self.onmessage = (event) => {
  const { type, id, request } = event.data ?? {};

  if (type === "fetchResult") {
    const entry = pendingFetch.get(id);
    if (!entry) return;
    pendingFetch.delete(id);
    clearTimeout(entry.timer);
    if (request?.error) {
      entry.reject(new TypeError(String(request.error)));
      return;
    }
    const response = request?.response ?? {};
    const body = response.bodyBase64
      ? base64ToBytes(response.bodyBase64)
      : new TextEncoder().encode(response.body ?? "");
    entry.resolve(
      new Response(body, {
        status: response.status ?? 500,
        headers: response.headers ?? {},
      }),
    );
    return;
  }

  if (type === "run" && typeof id === "number") {
    const args = request?.args ?? [];
    setStdin(request?.stdin ?? "");
    globalThis.kubectlRun(
      JSON.stringify({ args, stdin: request?.stdin ?? "" }),
      id,
    );
  }
};

self.postMessage({ type: "ready" });
