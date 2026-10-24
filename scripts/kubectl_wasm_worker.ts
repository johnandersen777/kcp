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

// kubectl_wasm_worker.ts runs upstream kubectl (k8s.io/kubectl/pkg/cmd)
// compiled to GOOS=js GOARCH=wasm inside a Web Worker.
//
// The Go module registers:
//   - kubectlRun(requestJSON, callbackID)
//   - __kubectlResolve(callbackID, resultJSON)
//
// kubectl's outbound HTTP goes through Go's js/wasm net/http transport, which
// uses this worker's fetch(). A distinct worker therefore talks to the kcp
// WASM shard over its Deno HTTP endpoint (or directly to any Kubernetes API
// server reachable from the host).

import { installFsShim, setStdin } from "./kubectl_wasm_fs.ts";

type InitMessage = {
  type: "init";
  wasmPath: string;
  execPath: string;
  execUrl: string;
};

type Pending = {
  resolve: (value: unknown) => void;
  reject: (reason: unknown) => void;
};

const workerScope = self as unknown as {
  postMessage: (message: unknown) => void;
  onmessage: ((event: MessageEvent) => void) | null;
  addEventListener: (
    type: "message",
    listener: (event: MessageEvent) => void,
  ) => void;
  removeEventListener: (
    type: "message",
    listener: (event: MessageEvent) => void,
  ) => void;
};

const init = await new Promise<InitMessage>((resolve) => {
  const listener = (event: MessageEvent) => {
    if (event.data?.type === "init") {
      workerScope.removeEventListener("message", listener);
      resolve(event.data as InitMessage);
    }
  };
  workerScope.addEventListener("message", listener);
});

// Side-effecting import: Go's wasm_exec.js defines globalThis.Go and a
// fallback globalThis.fs (no real filesystem under Deno). kubectl needs to
// stat/read kubeconfig and `-f` files, so install a synchronous Deno-backed
// fs before the Go runtime starts.
if (init.execUrl) {
  await import(init.execUrl);
} else {
  await import(pathToFileURL(init.execPath));
}
installFsShim();

const pending = new Map<number, Pending>();

(globalThis as Record<string, unknown>).__kubectlResolve = (
  id: number,
  payload: string,
) => {
  const entry = pending.get(id);
  if (!entry) {
    console.error(`kubectl-wasm: result for unknown callback ${id}`);
    return;
  }
  pending.delete(id);
  try {
    entry.resolve(JSON.parse(payload));
  } catch (err) {
    entry.reject(err);
  }
};

workerScope.onmessage = (event: MessageEvent) => {
  const { id, args, stdin } = event.data ?? {};
  if (typeof id !== "number") return;

  const run = (globalThis as Record<string, unknown>).kubectlRun as
    | ((requestJSON: string, callbackID: number) => void)
    | undefined;
  if (!run) {
    workerScope.postMessage({ id, error: "kubectlRun is not registered yet" });
    return;
  }

  const result = new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
  });
  result.then(
    (value) => workerScope.postMessage({ id, result: value }),
    (err) =>
      workerScope.postMessage({
        id,
        error: err instanceof Error ? err.message : String(err),
      }),
  );

  setStdin(typeof stdin === "string" ? stdin : "");
  run(JSON.stringify({ args: args ?? [], stdin: stdin ?? "" }), id);
};

const Go = (globalThis as Record<string, unknown>).Go as {
  new (): {
    importObject: WebAssembly.Imports;
    run(instance: WebAssembly.Instance): Promise<void>;
  };
};

const go = new Go();
const bytes = await Deno.readFile(init.wasmPath);
const { instance } = await WebAssembly.instantiate(bytes, go.importObject);

// run() only settles when the Go program exits; kubectl-wasm blocks forever
// after registering kubectlRun.
go.run(instance).catch((err) => {
  console.error("kubectl-wasm exited:", err);
});

for (let i = 0; i < 400; i++) {
  if (
    typeof (globalThis as Record<string, unknown>).kubectlRun === "function"
  ) {
    break;
  }
  await new Promise((r) => setTimeout(r, 25));
}
if (
  typeof (globalThis as Record<string, unknown>).kubectlRun !== "function"
) {
  throw new Error("kubectl-wasm did not register kubectlRun");
}

workerScope.postMessage({ type: "ready" });

function pathToFileURL(path: string): string {
  // Absolute paths only; encode characters that would confuse URL parsing.
  const normalized = path.startsWith("/") ? path : `/${path}`;
  return "file://" + encodeURI(normalized);
}
