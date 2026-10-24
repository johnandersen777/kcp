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

// app.js wires the browser UI: xterm.js terminal -> kubectl WASM worker ->
// fetch bridge -> kcp WASM worker, plus a poller that renders the live Pele
// diagram. Both Go modules run in this tab; the Deno server only serves
// static files.

import { Terminal } from "https://esm.sh/@xterm/xterm@5.5.0";
import { FitAddon } from "https://esm.sh/@xterm/addon-fit@0.10.0";
import { buildDiagram } from "./diagram.js";

const config = await (await fetch("/config")).json();
const apiServer = String(config.apiServer);
const intervalMs = Number(config.intervalMs ?? 1500);
const stateKey = "kcp-wasm-state";

const statusEl = document.getElementById("status");
const serverEl = document.getElementById("server");
const diagramEl = document.getElementById("diagram");
const diagramUpdatedEl = document.getElementById("diagram-updated");

serverEl.textContent = apiServer;

const term = new Terminal({
  cursorBlink: true,
  convertEol: true,
  fontSize: 13,
  fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
  scrollback: 5000,
  theme: {
    background: "#0b0e14",
    foreground: "#d5dbe5",
    cursor: "#7aa2f7",
    selectionBackground: "#2b3a55",
  },
});
const fitAddon = new FitAddon();
term.loadAddon(fitAddon);
term.open(document.getElementById("terminal"));
fitAddon.fit();
new ResizeObserver(() => {
  try { fitAddon.fit(); } catch { /* layout not ready */ }
}).observe(document.getElementById("terminal-pane"));
term.focus();
document.getElementById("terminal-pane").addEventListener("click", () => term.focus());

const dim = (text) => `\x1b[90m${text}\x1b[0m`;
const red = (text) => `\x1b[31m${text}\x1b[0m`;
const cyan = (text) => `\x1b[36m${text}\x1b[0m`;

function setStatus(text, className) {
  statusEl.textContent = text;
  statusEl.className = className;
}

let line = "";
let busy = false;
let ready = false;
let history = [];
let historyIndex = 0;

function normalizeNewlines(text) {
  return String(text).replace(/\r\n/g, "\n").replace(/\n/g, "\r\n");
}

function prompt() {
  term.write(`\r\n${cyan("kubectl")}\x1b[90m>\x1b[0m `);
}

function writePromptHint() {
  term.writeln(dim("try: get crds   |   get fetches"));
  term.writeln(
    dim(`create a Fetch: create -f ${location.origin}/demo.yaml`),
  );
}

// --- worker plumbing -------------------------------------------------------

let kcpWorker;
let kubectlWorker;
const pendingRuns = new Map();
let nextRunID = 1;
let runQueue = Promise.resolve();

function waitReady(worker) {
  return new Promise((resolve, reject) => {
    const onMessage = (event) => {
      if (event.data?.type === "ready") {
        worker.removeEventListener("message", onMessage);
        resolve();
      }
    };
    worker.addEventListener("message", onMessage);
    worker.addEventListener("error", (event) => {
      worker.removeEventListener("message", onMessage);
      reject(new Error(event.message ?? "worker error"));
    }, { once: true });
  });
}

let persistTimer = null;
function schedulePersist(snapshot) {
  if (persistTimer) clearTimeout(persistTimer);
  persistTimer = setTimeout(() => {
    try {
      localStorage.setItem(stateKey, snapshot);
    } catch {
      // Quota exceeded; the WASM state stays in memory only.
    }
  }, 250);
}

function splitArgs(text) {
  const args = [];
  let current = "";
  let quote = null;
  for (const ch of text) {
    if (quote) {
      if (ch === quote) quote = null;
      else current += ch;
    } else if (ch === '"' || ch === "'") {
      quote = ch;
    } else if (ch === " " || ch === "\t") {
      if (current) {
        args.push(current);
        current = "";
      }
    } else {
      current += ch;
    }
  }
  if (current) args.push(current);
  return args;
}

function withServer(args) {
  if (args.some((arg) => arg === "--server" || arg.startsWith("--server="))) {
    return args;
  }
  return [...args, `--server=${apiServer}`];
}

function runKubectl(args, stdin = "") {
  const task = runQueue.then(() => {
    return new Promise((resolve, reject) => {
      const id = nextRunID++;
      pendingRuns.set(id, { resolve, reject });
      kubectlWorker.postMessage({
        type: "run",
        id,
        request: { args, stdin },
      });
    });
  });
  runQueue = task.catch(() => {});
  return task;
}

async function start() {
  const initial = localStorage.getItem(stateKey) ?? "";

  kcpWorker = new Worker("/kcp_worker.js", { type: "module" });
  kcpWorker.addEventListener("message", (event) => {
    const data = event.data ?? {};
    if (data.type === "response") {
      kubectlWorker.postMessage({
        type: "fetchResult",
        id: data.id,
        request: { response: data.response, error: data.error },
      });
    } else if (data.type === "persist") {
      schedulePersist(data.snapshot);
    }
  });

  term.writeln(cyan("kcp-wasm") + dim(" loading kcp.wasm…"));
  setStatus("loading kcp…", "running");
  const kcpReady = waitReady(kcpWorker);
  kcpWorker.postMessage({
    type: "init",
    wasmUrl: "/kcp.wasm",
    execUrl: "/wasm_exec.js",
    initialState: initial,
  });
  await kcpReady;

  kubectlWorker = new Worker("/kubectl_worker.js", { type: "module" });
  kubectlWorker.addEventListener("message", (event) => {
    const data = event.data ?? {};
    if (data.type === "fetch") {
      kcpWorker.postMessage({
        type: "request",
        id: data.id,
        request: data.request,
      });
      return;
    }
    if (data.type === "result" && typeof data.id === "number") {
      const entry = pendingRuns.get(data.id);
      if (!entry) return;
      pendingRuns.delete(data.id);
      entry.resolve(data.result ?? {});
    }
  });

  term.writeln(cyan("kubectl-wasm") + dim(" loading kubectl.wasm (122 MB)…"));
  setStatus("loading kubectl…", "running");
  const kubectlReady = waitReady(kubectlWorker);
  kubectlWorker.postMessage({
    type: "init",
    wasmUrl: "/kubectl.wasm",
    execUrl: "/wasm_exec.js",
    apiServer,
  });
  await kubectlReady;

  ready = true;
  setStatus("connected", "connected");
  term.writeln(dim(`kcp + kubectl running in this browser tab (api ${apiServer})`));
  writePromptHint();
  prompt();

  void poll();
  setInterval(() => void poll(), intervalMs);
}

// --- live Pele diagram -----------------------------------------------------

let lastDiagram = "";
let polling = false;

async function kubectlJSON(args) {
  const result = await runKubectl(withServer([...args, "-o", "json"]));
  if (result.code !== 0) {
    throw new Error(String(result.stderr ?? "kubectl failed").trim());
  }
  return JSON.parse(result.stdout ?? "{}");
}

async function poll() {
  if (!ready || polling) return;
  polling = true;
  let mermaid;
  try {
    const [crds, fetches] = await Promise.all([
      kubectlJSON(["get", "crds"]),
      kubectlJSON(["get", "fetches"]),
    ]);
    mermaid = buildDiagram(apiServer, crds.items ?? [], fetches.items ?? []);
  } catch (err) {
    mermaid = [
      "flowchart LR",
      `  api["kcp-wasm<br/>${apiServer}"]`,
      `  err["kubectl error<br/>${String(err.message ?? err).replace(/"/g, "'").slice(0, 120)}"]`,
      "  api --> err",
    ].join("\n");
  } finally {
    polling = false;
  }
  if (mermaid !== lastDiagram) {
    lastDiagram = mermaid;
    renderDiagram(mermaid);
  }
}

function renderDiagram(mermaid) {
  diagramEl.replaceChildren();
  diagramUpdatedEl.textContent = `updated ${new Date().toLocaleTimeString()}`;
  try {
    pele.mount(diagramEl, mermaid);
  } catch (err) {
    const pre = document.createElement("pre");
    pre.className = "error";
    pre.textContent = String(err);
    diagramEl.appendChild(pre);
  }
}

// --- terminal --------------------------------------------------------------

function submit() {
  const command = line.trim();
  line = "";
  term.write("\r\n");
  if (!command) {
    prompt();
    return;
  }
  history.push(command);
  historyIndex = history.length;

  if (!ready) {
    term.writeln(red("still loading WASM modules…"));
    prompt();
    return;
  }

  const args = splitArgs(command);
  if (args[0] === "kubectl") args.shift();
  if (args.length === 0) {
    prompt();
    return;
  }

  busy = true;
  setStatus("running…", "running");
  const started = performance.now();
  runKubectl(withServer(args))
    .then((result) => {
      if (result.stdout) term.write(normalizeNewlines(result.stdout));
      if (result.stderr) term.write(red(normalizeNewlines(result.stderr)));
      term.write(dim(`[exit ${result.code} · ${Math.round(performance.now() - started)}ms]`));
    })
    .catch((err) => {
      term.writeln(red(`error: ${err instanceof Error ? err.message : err}`));
    })
    .finally(() => {
      busy = false;
      setStatus("connected", "connected");
      prompt();
      void poll();
    });
}

function replaceLine(next) {
  term.write("\r\x1b[2K" + cyan("kubectl") + "\x1b[90m>\x1b[0m " + next);
  line = next;
}

term.onData((data) => {
  if (busy) return;
  if (data === "\r") {
    submit();
    return;
  }
  if (data === "\u007f") {
    if (line.length > 0) {
      line = line.slice(0, -1);
      term.write("\b \b");
    }
    return;
  }
  if (data === "\u0003") {
    term.write("^C");
    line = "";
    prompt();
    return;
  }
  if (data === "\u000c") {
    term.clear();
    prompt();
    return;
  }
  if (data === "\u001b[A") {
    if (history.length === 0) return;
    historyIndex = Math.max(0, historyIndex - 1);
    replaceLine(history[historyIndex] ?? "");
    return;
  }
  if (data === "\u001b[B") {
    if (history.length === 0) return;
    historyIndex = Math.min(history.length, historyIndex + 1);
    replaceLine(history[historyIndex] ?? "");
    return;
  }
  if (data.startsWith("\u001b")) return;
  for (const ch of data) {
    if (ch === "\r") submit();
    else if (ch >= " ") {
      line += ch;
      term.write(ch);
    }
  }
});

setStatus("starting…", "running");
start().catch((err) => {
  setStatus("failed", "disconnected");
  term.writeln(red(`startup failed: ${err instanceof Error ? err.message : err}`));
});
