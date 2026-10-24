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
// diagram and an activity log. Both Go modules run in this tab; the Deno
// server only serves static files.

import { Terminal } from "https://esm.sh/@xterm/xterm@5.5.0";
import { FitAddon } from "https://esm.sh/@xterm/addon-fit@0.10.0";
import { buildDiagram } from "./diagram.js";

const config = await (await fetch("/config")).json();
const apiServer = String(config.apiServer);
const intervalMs = Number(config.intervalMs ?? 1500);
const stateKey = "kcp-wasm-state";
const maxLogEntries = 500;

const statusEl = document.getElementById("status");
const serverEl = document.getElementById("server");
const diagramEl = document.getElementById("diagram");
const diagramUpdatedEl = document.getElementById("diagram-updated");
const logEl = document.getElementById("log");
const logClearEl = document.getElementById("log-clear");
const logVerboseEl = document.getElementById("log-verbose");
const leftPaneEl = document.getElementById("left-pane");
const logPaneEl = document.getElementById("log-pane");
const splitterEl = document.getElementById("splitter");

serverEl.textContent = apiServer;

// --- activity log ----------------------------------------------------------

function addLog(level, message) {
  const row = document.createElement("div");
  row.className = `log-${level}`;
  const time = new Date().toLocaleTimeString(undefined, { hour12: false });
  row.textContent = `${time} ${message}`;
  logEl.appendChild(row);
  while (logEl.childElementCount > maxLogEntries) {
    logEl.firstElementChild.remove();
  }
  logEl.scrollTop = logEl.scrollHeight;
}

logClearEl.addEventListener("click", () => logEl.replaceChildren());

const verboseKey = "kcp-log-verbose";
logVerboseEl.checked = localStorage.getItem(verboseKey) === "1";
logEl.classList.toggle("verbose", logVerboseEl.checked);
logVerboseEl.addEventListener("change", () => {
  localStorage.setItem(verboseKey, logVerboseEl.checked ? "1" : "0");
  logEl.classList.toggle("verbose", logVerboseEl.checked);
  logEl.scrollTop = logEl.scrollHeight;
});

// --- terminal --------------------------------------------------------------

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
const promptText = () => cyan("kubectl") + "\x1b[90m>\x1b[0m ";

// --- splitter --------------------------------------------------------------

let draggingSplitter = false;
splitterEl.addEventListener("pointerdown", (event) => {
  draggingSplitter = true;
  splitterEl.setPointerCapture(event.pointerId);
});
splitterEl.addEventListener("pointermove", (event) => {
  if (!draggingSplitter) return;
  const rect = leftPaneEl.getBoundingClientRect();
  const height = Math.max(60, Math.min(rect.height - 160, rect.bottom - event.clientY));
  logPaneEl.style.height = `${Math.round(height)}px`;
});
for (const type of ["pointerup", "pointercancel"]) {
  splitterEl.addEventListener(type, () => {
    draggingSplitter = false;
  });
}

// --- worker plumbing -------------------------------------------------------

let kcpWorker = null;
let kubectlWorker = null;
let kubectlGate = Promise.resolve();
let gateResolve = null;
let restarting = false;
let ready = false;
let busy = false;
const pendingRuns = new Map();
let nextRunID = 1;
let runQueue = Promise.resolve();

class CancelledError extends Error {
  constructor() {
    super("cancelled");
    this.name = "CancelledError";
  }
}

function setStatus(text, className) {
  statusEl.textContent = text;
  statusEl.className = className;
}

function waitReady(worker) {
  return new Promise((resolve, reject) => {
    const cleanup = () => {
      worker.removeEventListener("message", onMessage);
      worker.removeEventListener("error", onError);
    };
    const onMessage = (event) => {
      if (event.data?.type === "ready") {
        cleanup();
        resolve();
      }
    };
    const onError = (event) => {
      cleanup();
      reject(new Error(event.message ?? "worker error"));
    };
    worker.addEventListener("message", onMessage);
    worker.addEventListener("error", onError);
  });
}

function attachKcp(worker) {
  worker.addEventListener("message", (event) => {
    const data = event.data ?? {};
    if (data.type === "response") {
      kubectlWorker?.postMessage({
        type: "fetchResult",
        id: data.id,
        request: { response: data.response, error: data.error },
      });
    } else if (data.type === "persist") {
      schedulePersist(data.snapshot);
    } else if (data.type === "log") {
      addLog(data.level ?? "info", `kcp: ${data.message}`);
    }
  });
}

function attachKubectl(worker) {
  worker.addEventListener("message", (event) => {
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
      return;
    }
    if (data.type === "log") {
      addLog(data.level ?? "info", `kubectl: ${data.message}`);
      return;
    }
    if (data.type === "exit") {
      const message = `kubectl exited (code ${data.code ?? "?"})`;
      for (const entry of pendingRuns.values()) entry.reject(new Error(message));
      pendingRuns.clear();
      void restartKubectl(message);
    }
  });

  worker.addEventListener("error", (event) => {
    const message = event.message ?? "kubectl worker error";
    addLog("error", `kubectl worker crashed: ${message}`);
    for (const entry of pendingRuns.values()) entry.reject(new Error(message));
    pendingRuns.clear();
  });
}

async function spawnKubectl() {
  const worker = new Worker("/kubectl_worker.js", { type: "module" });
  attachKubectl(worker);
  const readyPromise = waitReady(worker);
  worker.postMessage({
    type: "init",
    wasmUrl: "/kubectl.wasm",
    execUrl: "/wasm_exec.js",
    apiServer,
  });
  await readyPromise;
  kubectlWorker = worker;
}

async function restartKubectl(reason) {
  if (restarting) return;
  restarting = true;
  setStatus("restarting kubectl…", "running");
  addLog("warn", `restarting kubectl worker: ${reason}`);
  kubectlGate = new Promise((resolve) => {
    gateResolve = resolve;
  });
  const old = kubectlWorker;
  kubectlWorker = null;
  for (const entry of pendingRuns.values()) entry.reject(new CancelledError());
  pendingRuns.clear();
  old?.terminate();
  try {
    await spawnKubectl();
    addLog("ok", "kubectl worker restarted");
  } catch (err) {
    addLog("error", `kubectl restart failed: ${err instanceof Error ? err.message : err}`);
  } finally {
    restarting = false;
    gateResolve?.();
    gateResolve = null;
    if (kubectlWorker) setStatus("connected", "connected");
    else setStatus("kubectl failed", "disconnected");
  }
}

function runKubectl(args, stdin = "") {
  const task = runQueue.then(async () => {
    await kubectlGate;
    const worker = kubectlWorker;
    if (!worker) throw new Error("kubectl worker is not running");
    return await new Promise((resolve, reject) => {
      const id = nextRunID++;
      pendingRuns.set(id, { resolve, reject });
      worker.postMessage({ type: "run", id, request: { args, stdin } });
    });
  });
  runQueue = task.catch(() => {});
  return task;
}

let persistTimer = null;
function schedulePersist(snapshot) {
  if (persistTimer) clearTimeout(persistTimer);
  persistTimer = setTimeout(() => {
    try {
      localStorage.setItem(stateKey, snapshot);
    } catch {
      addLog("warn", `snapshot too large for localStorage (${snapshot.length} bytes)`);
    }
  }, 250);
}

// --- helpers ---------------------------------------------------------------

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

function normalizeNewlines(text) {
  return String(text).replace(/\r\n/g, "\n").replace(/\n/g, "\r\n");
}

// --- startup ---------------------------------------------------------------

async function start() {
  const initial = localStorage.getItem(stateKey) ?? "";
  kcpWorker = new Worker("/kcp_worker.js", { type: "module" });
  attachKcp(kcpWorker);
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

  term.writeln(cyan("kubectl-wasm") + dim(" loading kubectl.wasm (122 MB)…"));
  setStatus("loading kubectl…", "running");
  await spawnKubectl();

  ready = true;
  setStatus("connected", "connected");
  term.writeln(dim(`kcp + kubectl running in this browser tab (api ${apiServer})`));
  term.writeln(dim("type 'help' for examples; Ctrl-C cancels a running command"));
  writePromptHint();
  prompt();

  void poll();
  setInterval(() => void poll(), intervalMs);
}

function writePromptHint() {
  term.writeln(dim("try: get crds   |   get fetches"));
  term.writeln(dim(`create a Fetch: create -f ${location.origin}/demo.yaml`));
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
  let mermaid = null;
  let crds = null;
  let fetches = null;
  try {
    [crds, fetches] = await Promise.all([
      kubectlJSON(["get", "crds"]),
      kubectlJSON(["get", "fetches"]),
    ]);
    mermaid = buildDiagram(apiServer, crds.items ?? [], fetches.items ?? []);
  } catch (err) {
    if (err instanceof CancelledError) return;
    const message = String(err instanceof Error ? err.message : err);
    addLog("error", `poll failed: ${message}`);
    mermaid = [
      "flowchart LR",
      `  api["kcp-wasm<br/>${apiServer}"]`,
      `  err["kubectl error<br/>${message.replace(/"/g, "'").slice(0, 120)}"]`,
      "  api --> err",
    ].join("\n");
  } finally {
    polling = false;
  }

  if (mermaid !== lastDiagram) {
    lastDiagram = mermaid;
    renderDiagram(mermaid);
    const crdCount = crds?.items?.length ?? 0;
    const items = fetches?.items ?? [];
    const summary = items
      .map((item) => {
        const name = item?.metadata?.name ?? "?";
        const phase = item?.status?.phase ?? "Unresolved";
        const code = item?.status?.statusCode ? `/${item.status.statusCode}` : "";
        return `${name}=${phase}${code}`;
      })
      .join(", ");
    addLog(
      crds ? "info" : "warn",
      `state: ${crdCount} CRD(s)${summary ? ` · ${summary}` : " · no Fetch resources"}`,
    );
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

// --- terminal state and editing --------------------------------------------

let line = "";
let cursor = 0;
let history = [];
let historyIndex = 0;

function prompt() {
  term.write(`\r\n${promptText()}`);
}

function redraw() {
  term.write(`\r\x1b[2K${promptText()}${line}`);
  const back = line.length - cursor;
  if (back > 0) term.write(`\x1b[${back}D`);
}

function replaceLine(next) {
  line = next;
  cursor = next.length;
  redraw();
}

function insertText(text) {
  if (!text) return;
  if (cursor === line.length) {
    line += text;
    cursor = line.length;
    term.write(text);
  } else {
    line = line.slice(0, cursor) + text + line.slice(cursor);
    cursor += text.length;
    redraw();
  }
}

function deleteWordBeforeCursor() {
  if (cursor === 0) return;
  let index = cursor;
  while (index > 0 && line[index - 1] === " ") index--;
  while (index > 0 && line[index - 1] !== " ") index--;
  line = line.slice(0, index) + line.slice(cursor);
  cursor = index;
  redraw();
}

function printHelp() {
  term.writeln(dim("This terminal runs upstream kubectl compiled to WebAssembly."));
  term.writeln(dim("kcp runs in another Web Worker in this tab; --server is added for you."));
  term.writeln(dim("examples:"));
  term.writeln("  get crds");
  term.writeln("  get fetches");
  term.writeln("  get fetch demo -o yaml");
  term.writeln("  describe fetch demo");
  term.writeln(`  create -f ${location.origin}/demo.yaml`);
  term.writeln("  delete fetch demo");
  term.writeln(dim("keys: Ctrl-C cancel · Ctrl-L clear · ↑/↓ history · ←/→ cursor · Ctrl-A/E/U/W"));
  term.writeln(dim("no local files in WASM: use URLs with -f (e.g. the demo.yaml above)"));
}

function submit() {
  const command = line.trim();
  line = "";
  cursor = 0;
  term.write("\r\n");
  if (!command) {
    prompt();
    return;
  }
  history.push(command);
  historyIndex = history.length;

  const args = splitArgs(command);
  if (args[0] === "kubectl") args.shift();

  if (args[0] === "help") {
    printHelp();
    prompt();
    return;
  }
  if (args.length === 0) {
    prompt();
    return;
  }
  if (!ready) {
    term.writeln(red("still loading WASM modules…"));
    prompt();
    return;
  }

  busy = true;
  setStatus("running…", "running");
  addLog("info", `> ${args.join(" ")}`);
  const started = performance.now();
  runKubectl(withServer(args))
    .then((result) => {
      const elapsed = Math.round(performance.now() - started);
      if (result.stdout) term.write(normalizeNewlines(result.stdout));
      if (result.stderr) term.write(red(normalizeNewlines(result.stderr)));
      term.write(dim(`[exit ${result.code} · ${elapsed}ms]`));
      addLog(
        result.code === 0 ? "ok" : "warn",
        `exit ${result.code} · ${elapsed}ms · ${args[0]}`,
      );
    })
    .catch((err) => {
      if (err instanceof CancelledError) {
        term.writeln(dim("[cancelled]"));
        addLog("warn", "command cancelled");
      } else {
        const message = err instanceof Error ? err.message : String(err);
        term.writeln(red(`error: ${message}`));
        addLog("error", `command failed: ${message}`);
      }
    })
    .finally(() => {
      busy = false;
      if (!restarting) setStatus("connected", "connected");
      prompt();
      void poll();
    });
}

function cancelRunning() {
  line = "";
  cursor = 0;
  if (!busy) {
    prompt();
    return;
  }
  term.write("^C");
  addLog("warn", "Ctrl-C: restarting the kubectl worker to cancel");
  void restartKubectl("Ctrl-C");
}

term.onData((data) => {
  if (busy && data !== "\u0003") return;

  if (data === "\r" || data === "\n") {
    if (!busy) submit();
    return;
  }
  if (data === "\u0003") {
    cancelRunning();
    return;
  }
  if (busy) return;
  if (data === "\u007f") {
    if (cursor > 0) {
      line = line.slice(0, cursor - 1) + line.slice(cursor);
      cursor--;
      redraw();
    }
    return;
  }
  if (data === "\u001b[3~") {
    if (cursor < line.length) {
      line = line.slice(0, cursor) + line.slice(cursor + 1);
      redraw();
    }
    return;
  }
  if (data === "\u001b[D") {
    if (cursor > 0) {
      cursor--;
      term.write("\u001b[D");
    }
    return;
  }
  if (data === "\u001b[C") {
    if (cursor < line.length) {
      cursor++;
      term.write("\u001b[C");
    }
    return;
  }
  if (data === "\u001b[H" || data === "\u001b[1~") {
    if (cursor > 0) {
      cursor = 0;
      redraw();
    }
    return;
  }
  if (data === "\u001b[F" || data === "\u001b[4~") {
    if (cursor < line.length) {
      cursor = line.length;
      redraw();
    }
    return;
  }
  if (data === "\u000c") {
    term.clear();
    prompt();
    return;
  }
  if (data === "\u0001") {
    cursor = 0;
    redraw();
    return;
  }
  if (data === "\u0005") {
    cursor = line.length;
    redraw();
    return;
  }
  if (data === "\u0015") {
    line = line.slice(cursor);
    cursor = 0;
    redraw();
    return;
  }
  if (data === "\u0017") {
    deleteWordBeforeCursor();
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

  let text = "";
  for (const ch of data) {
    if (ch === "\r" || ch === "\n") {
      insertText(text);
      text = "";
      submit();
      return;
    }
    if (ch >= " ") text += ch;
  }
  insertText(text);
});

setStatus("starting…", "running");
start().catch((err) => {
  setStatus("failed", "disconnected");
  addLog("error", `startup failed: ${err instanceof Error ? err.message : err}`);
  term.writeln(red(`startup failed: ${err instanceof Error ? err.message : err}`));
});
