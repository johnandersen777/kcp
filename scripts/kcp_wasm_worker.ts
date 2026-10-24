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

// kcp_wasm_worker.ts runs the kcp-wasm module (GOOS=js GOARCH=wasm) inside a
// Web Worker and exposes a request/response RPC to the serving thread.
//
// The Go module registers two globals:
//   - kcpHandleRequest(requestJSON, callbackID): start handling an HTTP request
//   - __kcpResolve(callbackID, responseJSON): called when it is done
//
// That asymmetry is required because HTTP handling (notably the Fetch
// controller's outbound request) is asynchronous in Go: blocking the JS
// callback would prevent the Go runtime from delivering its fetch result.

type InitMessage = {
  type: "init";
  wasmPath: string;
  execPath: string;
  execUrl: string;
  initialState?: string;
};

type Pending = {
  resolve: (value: unknown) => void;
  reject: (reason: unknown) => void;
};

// Deno types `self` as a Window in this file; narrow it to the worker API we
// actually use.
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

// Side-effecting import: Go's wasm_exec.js defines globalThis.Go.
if (init.execUrl) {
  await import(init.execUrl);
} else {
  await import(pathToFileURL(init.execPath));
}

// The Go side reads this before starting the controller, and calls
// __kcpPersist after every mutation. Deno's main thread owns the file.
(globalThis as Record<string, unknown>).__kcpInitialState =
  init.initialState ?? "";
(globalThis as Record<string, unknown>).__kcpPersist = (
  snapshot: string,
) => {
  workerScope.postMessage({ type: "persist", snapshot });
};

const pending = new Map<number, Pending>();
let nextCallbackID = 1;

(globalThis as Record<string, unknown>).__kcpResolve = (
  id: number,
  payload: string,
) => {
  const entry = pending.get(id);
  if (!entry) {
    console.error(`kcp-wasm: response for unknown callback ${id}`);
    return;
  }
  pending.delete(id);
  try {
    entry.resolve(JSON.parse(payload));
  } catch (err) {
    entry.reject(err);
  }
};

workerScope.onmessage = async (event: MessageEvent) => {
  const { id, request } = event.data ?? {};
  if (typeof id !== "number") return;

  const callbackID = nextCallbackID++;
  const response = new Promise((resolve, reject) => {
    pending.set(callbackID, { resolve, reject });
  });

  try {
    const handle = (globalThis as Record<string, unknown>)
      .kcpHandleRequest as
        | ((requestJSON: string, callbackID: number) => void)
        | undefined;
    if (!handle) throw new Error("kcpHandleRequest is not registered yet");
    handle(JSON.stringify(request), callbackID);
    const result = await response;
    workerScope.postMessage({ id, response: result });
  } catch (err) {
    workerScope.postMessage({
      id,
      error: err instanceof Error ? err.message : String(err),
    });
  }
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

// run() only settles when the Go program exits; it runs forever by design.
go.run(instance).catch((err) => {
  console.error("kcp-wasm exited:", err);
});

// Wait until the Go program has registered its handler.
for (let i = 0; i < 400; i++) {
  if (
    typeof (globalThis as Record<string, unknown>).kcpHandleRequest ===
      "function"
  ) {
    break;
  }
  await new Promise((r) => setTimeout(r, 25));
}
if (
  typeof (globalThis as Record<string, unknown>).kcpHandleRequest !==
    "function"
) {
  throw new Error("kcp-wasm did not register kcpHandleRequest");
}

workerScope.postMessage({ type: "ready" });

function pathToFileURL(path: string): string {
  // Absolute paths only; encode characters that would confuse URL parsing.
  const normalized = path.startsWith("/") ? path : `/${path}`;
  return "file://" + encodeURI(normalized);
}
