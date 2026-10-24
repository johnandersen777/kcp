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
  socialweb?: Record<string, unknown>;
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

// Real kcp writes certs/keys/kubeconfigs. Give Go's syscall package a
// synchronous filesystem before the runtime starts (Deno's real FS for reads,
// an in-memory overlay for writes).
const { installFsShim } = await import("./kubectl_wasm_fs.ts");
const proc = (globalThis as Record<string, unknown>).process as {
  cwd: () => string;
  chdir: (path: string) => void;
} | undefined;
if (proc) {
  try {
    proc.cwd = () => "/";
    proc.chdir = () => {};
  } catch {
    // absolute rootDir keeps the boot working without cwd.
  }
}
installFsShim();

// The Go side reads this before starting the controller, and calls
// __kcpPersist after every mutation. Deno's main thread owns the file.
(globalThis as Record<string, unknown>).__kcpInitialState =
  init.initialState ?? "";
if (init.socialweb) {
  (globalThis as Record<string, unknown>).__socialwebConfig = JSON.stringify(
    init.socialweb,
  );
}
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
    const globals = globalThis as Record<string, unknown>;
    const handle = (globals.socialwebHandleRequest ?? globals.kcpHandleRequest) as
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
for (let i = 0; i < 7200; i++) {
  if (
    typeof (globalThis as Record<string, unknown>).kcpHandleRequest ===
      "function"
  ) {
    break;
  }
  if ((globalThis as Record<string, unknown>).__kcpBootError) break;
  if (i > 0 && i % 400 === 0) {
    console.error(`kcp-wasm: waiting for real kcp readiness (${i / 40}s)`);
  }
  await new Promise((r) => setTimeout(r, 25));
}
if ((globalThis as Record<string, unknown>).__kcpBootError) {
  throw new Error(
    "kcp-wasm boot failed: " +
      String((globalThis as Record<string, unknown>).__kcpBootError),
  );
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
