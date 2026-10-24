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

// cli_worker.js runs stock Go CLI binaries (goat, yq) compiled to js/wasm.
// Each invocation gets a fresh Go runtime instance, exactly like starting the
// native binary: argv/env/cwd come from the just-bash shell, stdin is fed to
// fd 0, and fd 1/2 are captured by the fs shim. Upstream sources are
// unmodified.
//
// The worker compiles all modules once and instantiates per run. goat's HTTP
// runs through the browser's normal fetch (PDS/PLC/AppView enable CORS,
// including preflight for Authorization).

import {
  clearMemoryFs,
  installFsShim,
  makeMemoryDir,
  setCwd,
  setOutputSink,
  setStdin,
  snapshotFiles,
  writeMemoryFile,
} from "./fs_shim.js";

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

await import(init.execUrl);
installFsShim();

// Go's os.Getwd/os.Chdir go through process.cwd/chdir; wasm_exec's shim
// throws ENOSYS, so back them with the just-bash working directory.
let processCwd = init.cwd ?? "/home/user";
globalThis.process.cwd = () => processCwd;
globalThis.process.chdir = (path) => {
  processCwd = String(path);
};
setCwd(processCwd);

const nativeFetch = globalThis.fetch.bind(globalThis);
globalThis.fetch = async (input, options = {}) => {
  const url = typeof input === "string" ? input : String(input?.url ?? input);
  if (/^https?:/i.test(url)) {
    log("info", `${options.method ?? "GET"} ${url}`);
  }
  return nativeFetch(input, options);
};

const compiled = new Map();
const unavailable = new Map();
for (const [tool, url] of Object.entries(init.modules ?? {})) {
  try {
    const bytes = await (await nativeFetch(url)).arrayBuffer();
    compiled.set(tool, await WebAssembly.compile(bytes));
    log("info", `${tool}.wasm ${(bytes.byteLength / 1048576).toFixed(1)} MiB compiled`);
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    unavailable.set(tool, message);
    log("error", `${tool}.wasm unavailable: ${message}`);
  }
}

self.onmessage = (event) => {
  const { type, id, request } = event.data ?? {};
  if (type === "run" && typeof id === "number") {
    void runTool(id, request ?? {});
  }
};

async function runTool(id, request) {
  const tool = String(request.tool ?? "");
  const module = compiled.get(tool);
  if (!module) {
    self.postMessage({
      type: "result",
      id,
      result: {
        stdout: "",
        stderr: `${tool}: not available${unavailable.get(tool) ? `: ${unavailable.get(tool)}` : ""}\n`,
        code: 127,
      },
    });
    return;
  }

  clearMemoryFs();
  const cwd = request.cwd ?? "/home/user";
  processCwd = cwd;
  setCwd(cwd);
  for (const dir of request.dirs ?? []) makeMemoryDir(dir);
  for (const [path, content] of Object.entries(request.files ?? {})) {
    writeMemoryFile(path, content);
  }
  setStdin(request.stdin ?? "");

  const capture = { stdout: "", stderr: "" };
  setOutputSink(capture);

  let exitCode = 0;
  try {
    const go = new globalThis.Go();
    go.argv = [tool, ...(request.args ?? [])];
    go.env = request.env ?? {};
    go.exit = (code) => {
      // runtime.wasmExit passes -1 for os.Exit(-1); shell status wraps to 255.
      exitCode = code < 0 ? 256 + (code % 256) : code;
    };
    const instance = await WebAssembly.instantiate(module, go.importObject);
    await go.run(instance);
  } catch (err) {
    capture.stderr += `${err instanceof Error ? err.message : String(err)}\n`;
    exitCode = exitCode || 1;
  } finally {
    setOutputSink(null);
  }

  self.postMessage({
    type: "result",
    id,
    result: {
      stdout: capture.stdout,
      stderr: capture.stderr,
      code: exitCode,
      files: snapshotFiles(),
    },
  });
}

log(
  "ok",
  `CLI worker ready in ${Math.round(performance.now() - started)}ms ` +
    `(${[...compiled.keys()].join(", ") || "no modules"})`,
);
self.postMessage({ type: "ready" });
