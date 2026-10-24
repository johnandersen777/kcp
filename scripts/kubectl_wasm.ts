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

/**
 * Runs upstream kubectl compiled to WebAssembly in a Web Worker, against any
 * Kubernetes API server reachable from the Deno host (including the kcp WASM
 * shard served by scripts/kcp_wasm.ts).
 *
 *   deno run --allow-read --allow-net \
 *     scripts/kubectl_wasm.ts \
 *     --wasm=bin/wasm/kubectl.wasm \
 *     --wasm-exec=bin/wasm/wasm_exec.js \
 *     -- get fetches --server=http://127.0.0.1:18080
 *
 * Everything after `--` is passed to kubectl. Like the native client, it uses
 * Go's js/wasm net/http transport, which performs each request with fetch().
 * There is no kubeconfig file access in the WASM runtime: pass connection
 * details as flags (--server, --token, --namespace, ...) or use
 * --kubeconfig only if the host installs an fs shim.
 */

const options = new Map<string, string>();
const kubectlArgs: string[] = [];
let afterDoubleDash = false;

for (const arg of Deno.args) {
  if (afterDoubleDash) {
    kubectlArgs.push(arg);
    continue;
  }
  if (arg === "--") {
    afterDoubleDash = true;
    continue;
  }
  const match = arg.match(/^--([^=]+)=(.*)$/);
  if (match) {
    options.set(match[1], match[2]);
  } else {
    kubectlArgs.push(arg);
  }
}

const wasmPath = Deno.realPathSync(
  options.get("wasm") ?? "bin/wasm/kubectl.wasm",
);
const wasmExecPath = Deno.realPathSync(
  options.get("wasm-exec") ?? "bin/wasm/wasm_exec.js",
);
const stdinFile = options.get("stdin-file");
const stdin = stdinFile ? await Deno.readTextFile(stdinFile) : "";

type RunResult = {
  stdout?: string;
  stderr?: string;
  code?: number;
};

const workerURL = new URL("./kubectl_wasm_worker.ts", import.meta.url);
const worker = new Worker(workerURL, { type: "module" });

const ready = new Promise<void>((resolve, reject) => {
  worker.onmessage = (event: MessageEvent) => {
    const data = event.data ?? {};
    if (data.type === "ready") {
      resolve();
      return;
    }
    if (data.type === "error") {
      reject(new Error(String(data.error)));
    }
  };
  worker.onerror = (event: ErrorEvent) => {
    reject(new Error(event.message ?? "kubectl-wasm worker error"));
  };
});

const encoder = new TextEncoder();

try {
  worker.postMessage({
    type: "init",
    wasmPath,
    execPath: wasmExecPath,
    execUrl: "file://" + encodeURI(wasmExecPath),
  });
  await ready;

  const resultPromise = new Promise<RunResult>((resolve, reject) => {
    worker.onmessage = (event: MessageEvent) => {
      const data = event.data ?? {};
      if (typeof data.id !== "number") return;
      if (data.error) {
        reject(new Error(String(data.error)));
      } else {
        resolve((data.result ?? {}) as RunResult);
      }
    };
    worker.onerror = (event: ErrorEvent) => {
      reject(new Error(event.message ?? "kubectl-wasm worker error"));
    };
  });

  worker.postMessage({ id: 1, args: kubectlArgs, stdin });
  const result = await resultPromise;

  if (result.stdout) {
    Deno.stdout.writeSync(encoder.encode(result.stdout));
  }
  if (result.stderr) {
    Deno.stderr.writeSync(encoder.encode(result.stderr));
  }
  Deno.exit(result.code ?? 0);
} catch (err) {
  console.error("kubectl-wasm:", err instanceof Error ? err.message : err);
  Deno.exit(1);
} finally {
  worker.terminate();
}
