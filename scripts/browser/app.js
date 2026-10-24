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

// app.js runs a just-bash shell in the browser. kubectl and goat are custom
// just-bash commands that bridge to their own WebAssembly workers; kcp runs
// in a third worker. The Deno server serves files and proxies goat's
// cross-origin HTTP.

import { Terminal } from "https://esm.sh/@xterm/xterm@5.5.0";
import { FitAddon } from "https://esm.sh/@xterm/addon-fit@0.10.0";
import { WebLinksAddon } from "https://esm.sh/@xterm/addon-web-links@0.11.0";
import {
  Bash,
  defineCommand,
  getCommandNames,
} from "https://esm.sh/just-bash@3.6.0/browser";
import { buildDiagram, nodeID } from "./diagram.js";

const config = await (await fetch("/config")).json();
const apiServer = String(config.apiServer);
const intervalMs = Number(config.intervalMs ?? 1500);
const stateKey = "kcp-wasm-state";
const shellStateKey = "kcp-shell-state";
const maxLogEntries = 500;
const homeDir = "/home/user";

const statusEl = document.getElementById("status");
const retryEl = document.getElementById("retry");
retryEl.addEventListener("click", () => location.reload());
const serverEl = document.getElementById("server");
const diagramEl = document.getElementById("diagram");
const diagramUpdatedEl = document.getElementById("diagram-updated");
const logEl = document.getElementById("log");
const logClearEl = document.getElementById("log-clear");
const logVerboseEl = document.getElementById("log-verbose");
const leftPaneEl = document.getElementById("left-pane");
const logPaneEl = document.getElementById("log-pane");
const splitterEl = document.getElementById("splitter");
const logToggleEl = document.getElementById("log-toggle");

serverEl.textContent = apiServer;

// --- activity log ----------------------------------------------------------

function addLog(level, message) {
  const nearBottom =
    logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 40;
  const row = document.createElement("div");
  row.className = `log-${level}`;
  const time = new Date().toLocaleTimeString(undefined, { hour12: false });
  row.textContent = `${time} ${message}`;
  logEl.appendChild(row);
  while (logEl.childElementCount > maxLogEntries) {
    logEl.firstElementChild.remove();
  }
  if (nearBottom) logEl.scrollTop = logEl.scrollHeight;
}

logClearEl.addEventListener("click", () => logEl.replaceChildren());

document.getElementById("log-save").addEventListener("click", () => {
  const text = [...logEl.children].map((row) => row.textContent).join("\n");
  const blob = new Blob([`${text}\n`], { type: "text/plain" });
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = `kcp-browser-log-${Date.now()}.txt`;
  anchor.click();
  URL.revokeObjectURL(url);
});

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
term.loadAddon(new WebLinksAddon());
term.open(document.getElementById("terminal"));
const isNarrow = () => window.innerWidth < 700;

fitAddon.fit();
new ResizeObserver(() => {
  try {
    const size = isNarrow() ? 12 : 13;
    if (term.options.fontSize !== size) term.options.fontSize = size;
    fitAddon.fit();
  } catch { /* layout not ready */ }
}).observe(document.getElementById("terminal-pane"));
term.focus();
document.getElementById("terminal-pane").addEventListener("click", () => term.focus());

// Ctrl+Shift+C/V copy and paste terminal selections without stealing Ctrl-C
// (which cancels) or plain Ctrl-V (browser paste into xterm's textarea).
term.attachCustomKeyEventHandler((event) => {
  if (event.type !== "keydown") return true;
  if (event.ctrlKey && event.shiftKey && event.code === "KeyC") {
    const selection = term.getSelection();
    if (selection) navigator.clipboard?.writeText(selection).catch(() => {});
    return false;
  }
  if (event.ctrlKey && event.shiftKey && event.code === "KeyV") {
    navigator.clipboard?.readText()
      .then((text) => {
        if (text) term.paste(text);
      })
      .catch(() => {});
    return false;
  }
  return true;
});

const dim = (text) => `\x1b[90m${text}\x1b[0m`;
const red = (text) => `\x1b[31m${text}\x1b[0m`;
const cyan = (text) => `\x1b[36m${text}\x1b[0m`;

// --- splitter --------------------------------------------------------------

const splitKey = "kcp-log-height";
const logExpandedKey = "kcp-log-expanded";
const savedSplit = localStorage.getItem(splitKey);
if (savedSplit) logPaneEl.style.height = savedSplit;

let logExpanded = localStorage.getItem(logExpandedKey) === "1";

function setLogExpanded(expanded) {
  logExpanded = expanded;
  logPaneEl.classList.toggle("collapsed", !expanded);
  leftPaneEl.classList.toggle("log-collapsed", !expanded);
  logPaneEl.style.height = expanded ? (savedSplit ?? "190px") : "";
  logToggleEl.textContent = expanded ? "hide" : "show";
  localStorage.setItem(logExpandedKey, expanded ? "1" : "0");
  if (expanded) logEl.scrollTop = logEl.scrollHeight;
}

logPaneEl.classList.toggle("collapsed", !logExpanded);
leftPaneEl.classList.toggle("log-collapsed", !logExpanded);
logPaneEl.style.height = logExpanded ? logPaneEl.style.height : "";
logToggleEl.textContent = logExpanded ? "hide" : "show";
logToggleEl.addEventListener("click", () => setLogExpanded(!logExpanded));

let draggingSplitter = false;
splitterEl.addEventListener("pointerdown", (event) => {
  draggingSplitter = true;
  if (!logExpanded) setLogExpanded(true);
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
    localStorage.setItem(splitKey, logPaneEl.style.height || "190px");
  });
}

// --- shared worker helpers -------------------------------------------------

let ready = false;
let busy = false;

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

class CancelledError extends Error {
  constructor() {
    super("cancelled");
    this.name = "CancelledError";
  }
}

function serializeQueue() {
  let queue = Promise.resolve();
  return (task) => {
    const result = queue.then(task);
    queue = result.catch(() => {});
    return result;
  };
}

// --- kcp worker ------------------------------------------------------------

let kcpWorker = null;
const pendingBridges = new Map();

function attachKcp(worker) {
  worker.addEventListener("error", (event) => {
    const message = event.message ?? "worker error";
    addLog("error", `kcp worker crashed: ${message}`);
    for (const id of pendingBridges.keys()) {
      kubectlWorker?.postMessage({
        type: "fetchResult",
        id,
        request: { error: `kcp worker crashed: ${message}` },
      });
    }
    pendingBridges.clear();
  });
  worker.addEventListener("message", (event) => {
    const data = event.data ?? {};
    if (data.type === "response") {
      pendingBridges.delete(data.id);
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

async function spawnKcp() {
  const worker = new Worker("/kcp_worker.js", { type: "module" });
  attachKcp(worker);
  const readyPromise = waitReady(worker);
  worker.postMessage({
    type: "init",
    wasmUrl: "/kcp.wasm",
    execUrl: "/wasm_exec.js",
    initialState: localStorage.getItem(stateKey) ?? "",
  });
  await readyPromise;
  kcpWorker = worker;
}

// --- kubectl worker --------------------------------------------------------

let kubectlWorker = null;
let kubectlGate = Promise.resolve();
let kubectlGateResolve = null;
let kubectlRestarting = false;
let kubectlInFlight = 0;
const pendingRuns = new Map();
let nextRunID = 1;
const runKubectlQueued = serializeQueue();

function attachKubectl(worker) {
  worker.addEventListener("message", (event) => {
    const data = event.data ?? {};
    if (data.type === "fetch") {
      pendingBridges.set(data.id, true);
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
      pendingBridges.clear();
      if (!kubectlRestarting) void restartKubectl(message);
    }
  });

  worker.addEventListener("error", (event) => {
    const message = event.message ?? "kubectl worker error";
    addLog("error", `kubectl worker crashed: ${message}`);
    for (const entry of pendingRuns.values()) entry.reject(new Error(message));
    pendingRuns.clear();
    pendingBridges.clear();
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
  if (kubectlRestarting) return;
  kubectlRestarting = true;
  setStatus("restarting kubectl…", "running");
  addLog("warn", `restarting kubectl worker: ${reason}`);
  kubectlGate = new Promise((resolve) => {
    kubectlGateResolve = resolve;
  });
  const old = kubectlWorker;
  kubectlWorker = null;
  for (const entry of pendingRuns.values()) entry.reject(new CancelledError());
  pendingRuns.clear();
  pendingBridges.clear();
  old?.terminate();
  try {
    await spawnKubectl();
    addLog("ok", "kubectl worker restarted");
  } catch (err) {
    addLog("error", `kubectl restart failed: ${err instanceof Error ? err.message : err}`);
  } finally {
    kubectlRestarting = false;
    kubectlGateResolve?.();
    kubectlGateResolve = null;
    if (kubectlWorker && !cliRestarting) setStatus("connected", "connected");
  }
}

function runKubectl(args, stdin = "") {
  return runKubectlQueued(async () => {
    await kubectlGate;
    const worker = kubectlWorker;
    if (!worker) throw new Error("kubectl worker is not running");
    kubectlInFlight++;
    try {
      return await new Promise((resolve, reject) => {
        const id = nextRunID++;
        pendingRuns.set(id, { resolve, reject });
        worker.postMessage({ type: "run", id, request: { args, stdin } });
      });
    } finally {
      kubectlInFlight--;
    }
  });
}

function withServer(args) {
  if (args.some((arg) => arg === "--server" || arg.startsWith("--server="))) {
    return args;
  }
  return [...args, `--server=${apiServer}`];
}

// --- CLI worker (goat, yq) -------------------------------------------------

const cliBaseEnv = {
  HOME: homeDir,
  XDG_STATE_HOME: `${homeDir}/.local/state`,
  XDG_DATA_HOME: `${homeDir}/.local/share`,
  XDG_CONFIG_HOME: `${homeDir}/.config`,
  XDG_CACHE_HOME: `${homeDir}/.cache`,
  TMPDIR: "/tmp",
  PATH: "/usr/local/bin:/usr/bin:/bin",
  USER: "user",
  LOGNAME: "user",
  LANG: "C.UTF-8",
};

let cliWorker = null;
let cliGate = Promise.resolve();
let cliGateResolve = null;
let cliRestarting = false;
let cliInFlight = 0;
const pendingCli = new Map();
let nextCliID = 1;
const runCliQueued = serializeQueue();

function attachCli(worker) {
  worker.addEventListener("message", (event) => {
    const data = event.data ?? {};
    if (data.type === "log") {
      addLog(data.level ?? "info", `cli: ${data.message}`);
      return;
    }
    if (data.type === "result" && typeof data.id === "number") {
      const entry = pendingCli.get(data.id);
      if (!entry) return;
      pendingCli.delete(data.id);
      entry.resolve(data.result ?? {});
    }
  });

  worker.addEventListener("error", (event) => {
    const message = event.message ?? "cli worker error";
    addLog("error", `cli worker crashed: ${message}`);
    for (const entry of pendingCli.values()) entry.reject(new Error(message));
    pendingCli.clear();
  });
}

async function spawnCli() {
  const worker = new Worker("/cli_worker.js", { type: "module" });
  attachCli(worker);
  const readyPromise = waitReady(worker);
  worker.postMessage({
    type: "init",
    execUrl: "/wasm_exec_cli.js",
    modules: { goat: "/goat.wasm", yq: "/yq.wasm" },
    env: cliBaseEnv,
    cwd: homeDir,
  });
  await readyPromise;
  cliWorker = worker;
}

async function restartCli(reason) {
  if (cliRestarting) return;
  cliRestarting = true;
  addLog("warn", `restarting CLI worker: ${reason}`);
  cliGate = new Promise((resolve) => {
    cliGateResolve = resolve;
  });
  const old = cliWorker;
  cliWorker = null;
  for (const entry of pendingCli.values()) entry.reject(new CancelledError());
  pendingCli.clear();
  old?.terminate();
  try {
    await spawnCli();
    addLog("ok", "CLI worker restarted");
  } catch (err) {
    addLog("error", `CLI restart failed: ${err instanceof Error ? err.message : err}`);
  } finally {
    cliRestarting = false;
    cliGateResolve?.();
    cliGateResolve = null;
  }
}

function runCli(tool, args, env, cwd, stdin, files, dirs) {
  return runCliQueued(async () => {
    await cliGate;
    const worker = cliWorker;
    if (!worker) throw new Error(`${tool} worker is not running`);
    cliInFlight++;
    try {
      return await new Promise((resolve, reject) => {
        const id = nextCliID++;
        pendingCli.set(id, { resolve, reject });
        worker.postMessage({
          type: "run",
          id,
          request: { tool, args, env, cwd, stdin, files, dirs },
        });
      });
    } finally {
      cliInFlight--;
    }
  });
}

// --- just-bash shell and filesystem sharing --------------------------------

let bash = null;
let shellAbort = null;
let ttyTool = null;

function decodeStdin(value) {
  if (!value) return "";
  if (typeof value === "string") {
    const bytes = new Uint8Array(value.length);
    for (let i = 0; i < value.length; i++) bytes[i] = value.charCodeAt(i) & 0xff;
    return new TextDecoder().decode(bytes);
  }
  return new TextDecoder().decode(value);
}

async function walkBashFs(fs, dir = "/", files = {}, dirs = []) {
  let names = [];
  try {
    names = await fs.readdir(dir);
  } catch {
    return { files, dirs };
  }
  if (dir !== "/") dirs.push(dir);
  for (const name of names) {
    const path = dir === "/" ? `/${name}` : `${dir}/${name}`;
    if (path === "/dev" || path === "/proc") continue;
    let stat;
    try {
      stat = await fs.stat(path);
    } catch {
      continue;
    }
    if (stat.isDirectory) {
      await walkBashFs(fs, path, files, dirs);
    } else if (stat.isFile && stat.size < 1_000_000) {
      try {
        files[path] = await fs.readFile(path);
      } catch {
        // Binary or unreadable file; not shareable with goat.
      }
    }
  }
  return { files, dirs };
}

async function applyCliFiles(files, sent) {
  for (const path of Object.keys(sent)) {
    if (!(path in files)) {
      await bash.fs.rm(path, { force: true }).catch(() => {});
    }
  }
  for (const [path, content] of Object.entries(files)) {
    if (sent[path] === content) continue;
    const directory = path.slice(0, path.lastIndexOf("/")) || "/";
    await bash.fs.mkdir(directory, { recursive: true }).catch(() => {});
    await bash.fs.writeFile(path, content).catch((err) => {
      addLog("warn", `cli fs: ${path}: ${err instanceof Error ? err.message : err}`);
    });
  }
}

function shellHelp() {
  return [
    "just-bash shell. kubectl and goat run as WebAssembly workers in this tab.",
    "",
    "examples:",
    "  get crds",
    "  get fetches | grep demo",
    "  kubectl get fetch demo -o yaml > demo.yaml",
    "  kubectl apply -f - <<'EOF' ... EOF",
    "  goat resolve bsky.app",
    "  goat account login -u handle -p app-password",
    "  goat xrpc query https://public.api.bsky.app app.bsky.actor.getProfile actor==atproto.com",
    "  yq '.items[].metadata.name' fetches.json",
    "  echo '{\"a\":1}' | jq .a    (jq is built into just-bash)",
    "  export GOAT_USERNAME=... GOAT_PASSWORD=...",
    "  ls /tmp; cat demo.yaml",
    "",
    "builtins: help · clear · reset (wipe kcp state and reload)",
    "keys: Ctrl-C cancel · Ctrl-D finish stdin · Tab complete · ↑/↓ history",
    "",
  ].join("\n");
}

function lastPipelineSegment(script) {
  const source = stripHeredocBodies(script);
  const segments = [];
  let current = "";
  let quote = null;
  for (let i = 0; i < source.length; i++) {
    const ch = source[i];
    if (quote) {
      current += ch;
      if (ch === quote) quote = null;
      continue;
    }
    if (ch === "'" || ch === '"') {
      quote = ch;
      current += ch;
      continue;
    }
    if (ch === "|" && source[i + 1] !== "|") {
      segments.push(current);
      current = "";
      continue;
    }
    current += ch;
  }
  segments.push(current);
  return segments[segments.length - 1].trim();
}

// A native shell gives the last pipeline command a terminal stdout. Use that
// to set KCP_TTY for CLI tools that colorize on isatty (yq).
function detectTtyTool(script) {
  const segment = lastPipelineSegment(script);
  if (/[<>]/.test(segment)) return null;
  const first = splitArgs(segment)[0];
  return ["kubectl", "goat", "yq"].includes(first) ? first : null;
}

function makeCommands() {
  const kubectl = defineCommand("kubectl", async (args, ctx) => {
    const result = await runKubectl(withServer(args), decodeStdin(ctx.stdin));
    return {
      stdout: result.stdout ?? "",
      stderr: result.stderr ?? "",
      exitCode: result.code ?? 0,
    };
  });

  const runCliCommand = async (tool, args, ctx) => {
    if (!cliWorker && !cliRestarting) {
      return { stdout: "", stderr: `${tool} worker is not available\n`, exitCode: 1 };
    }
    if (cliRestarting) await cliGate;
    const env = { ...cliBaseEnv, ...Object.fromEntries(ctx.env) };
    if (tool === ttyTool) env.KCP_TTY = "1";
    const { files, dirs } = await walkBashFs(bash.fs);
    const result = await runCli(
      tool,
      args,
      env,
      ctx.cwd,
      decodeStdin(ctx.stdin),
      files,
      dirs,
    );
    await applyCliFiles(result.files ?? {}, files);
    return {
      stdout: result.stdout ?? "",
      stderr: result.stderr ?? "",
      exitCode: result.code ?? 0,
    };
  };

  const goat = defineCommand("goat", (args, ctx) => runCliCommand("goat", args, ctx));
  const yq = defineCommand("yq", (args, ctx) => runCliCommand("yq", args, ctx));

  const help = defineCommand("help", async () => ({
    stdout: shellHelp(),
    stderr: "",
    exitCode: 0,
  }));

  const clear = defineCommand("clear", async () => {
    term.clear();
    return { stdout: "", stderr: "", exitCode: 0 };
  });

  const reset = defineCommand("reset", async () => {
    localStorage.removeItem(stateKey);
    localStorage.removeItem(shellStateKey);
    addLog("warn", "shell and kcp state cleared; reloading");
    setTimeout(() => location.reload(), 300);
    return { stdout: "cleared shell and kcp state; reloading…\n", stderr: "", exitCode: 0 };
  });

  return [kubectl, goat, yq, help, clear, reset];
}

async function restoreShell() {
  let saved = {};
  try {
    saved = JSON.parse(localStorage.getItem(shellStateKey) ?? "{}");
  } catch {
    saved = {};
  }

  bash = new Bash({
    cwd: homeDir,
    env: saved.env ?? {},
    customCommands: makeCommands(),
    executionLimits: { maxExecutionTimeMs: 60 * 60 * 1000 },
  });

  for (const directory of ["/home", homeDir, "/tmp", "/etc", "/bin", "/usr/bin"]) {
    await bash.fs.mkdir(directory, { recursive: true }).catch(() => {});
  }
  for (const directory of saved.dirs ?? []) {
    await bash.fs.mkdir(directory, { recursive: true }).catch(() => {});
  }
  for (const [path, content] of Object.entries(saved.files ?? {})) {
    await bash.fs.writeFile(path, content).catch(() => {});
  }
  if (saved.cwd && saved.cwd !== bash.getCwd()) {
    await bash.exec(`cd '${saved.cwd.replace(/'/g, "'\\''")}'`).catch(() => {});
  }
}

let persistShellTimer = null;
function scheduleShellPersist(env) {
  if (!bash) return;
  if (persistShellTimer) clearTimeout(persistShellTimer);
  persistShellTimer = setTimeout(async () => {
    try {
      const { files, dirs } = await walkBashFs(bash.fs);
      const payload = JSON.stringify({
        files,
        dirs,
        env: env ?? bash.getEnv(),
        cwd: bash.getCwd(),
      });
      localStorage.setItem(shellStateKey, payload);
    } catch (err) {
      addLog(
        "warn",
        `could not persist shell filesystem: ${err instanceof Error ? err.message : err}`,
      );
    }
  }, 250);
}

// --- shell prompt and execution --------------------------------------------

let line = "";
let cursor = 0;
let history = [];
let historyIndex = 0;
let continuation = [];
let stdinMode = false;
let stdinScript = "";
let stdinLines = [];
let stdinCurrent = "";
let completing = false;

try {
  const saved = JSON.parse(localStorage.getItem("kcp-cmd-history") ?? "[]");
  if (Array.isArray(saved)) {
    history = saved.filter((entry) => typeof entry === "string");
  }
} catch {
  // Corrupt history is not worth surfacing.
}
historyIndex = history.length;

function promptText() {
  const cwd = bash?.getCwd() ?? homeDir;
  return `${cyan(cwd)} $ `;
}

const stdinPrompt = () => "\x1b[90m...\x1b[0m ";
const continuationPrompt = () => "\x1b[90m> \x1b[0m ";

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

function saveHistory() {
  try {
    localStorage.setItem("kcp-cmd-history", JSON.stringify(history.slice(-100)));
  } catch {
    // History is best-effort.
  }
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

function needsStdin(args) {
  for (let index = 0; index < args.length; index++) {
    const arg = args[index];
    if ((arg === "-f" || arg === "--filename") && args[index + 1] === "-") {
      return true;
    }
    if (arg === "--filename=-") return true;
  }
  return false;
}

function normalizeNewlines(text) {
  return String(text).replace(/\r\n/g, "\n").replace(/\n/g, "\r\n");
}

function heredocDelimiter(script) {
  const lines = script.split("\n");
  for (let i = 0; i < lines.length; i++) {
    const match = lines[i].match(/<<-?\s*(['"]?)([A-Za-z0-9_]+)\1/);
    if (!match) continue;
    const delimiter = match[2];
    const closes = lines.slice(i + 1).some((entry) => entry.trim() === delimiter);
    if (!closes) return delimiter;
  }
  return null;
}

function stripHeredocBodies(script) {
  const lines = script.split("\n");
  const kept = [];
  let active = null;
  for (const current of lines) {
    if (active !== null) {
      if (current.trim() === active) active = null;
      continue;
    }
    kept.push(current);
    const match = current.match(/<<-?\s*(['"]?)([A-Za-z0-9_]+)\1/);
    if (match) active = match[2];
  }
  return kept.join("\n");
}

function unbalancedQuote(script) {
  let quote = null;
  for (let i = 0; i < script.length; i++) {
    const ch = script[i];
    if (ch === "\\" && quote !== "'") {
      i++;
      continue;
    }
    if (quote) {
      if (ch === quote) quote = null;
    } else if (ch === "'" || ch === '"') {
      quote = ch;
    }
  }
  return quote;
}

function needsContinuation(script) {
  if (heredocDelimiter(script)) return true;
  if (unbalancedQuote(stripHeredocBodies(script))) return true;
  const trimmed = script.replace(/\s+$/, "");
  if (trimmed.endsWith("\\")) return true;
  if (/(\||&&|\|\||\()$/.test(trimmed)) return true;
  return false;
}

async function runScript(script, stdin = "") {
  busy = true;
  const label = script.split("\n")[0].slice(0, 80);
  setStatus(`running ${label}…`, "running");
  addLog("info", `$ ${label.replace(/\s+/g, " ")}${script.includes("\n") ? " …" : ""}`);
  const started = performance.now();
  ttyTool = detectTtyTool(script);
  shellAbort = new AbortController();

  try {
    const result = await bash.exec(script, {
      signal: shellAbort.signal,
      stdin,
      rawScript: script.includes("\n"),
    });
    const elapsed = Math.round(performance.now() - started);
    if (result.stdout) term.write(normalizeNewlines(result.stdout));
    if (result.stderr) term.write(red(normalizeNewlines(result.stderr)));
    term.write(dim(`[exit ${result.exitCode} · ${elapsed}ms]`));
    addLog(
      result.exitCode === 0 ? "ok" : "warn",
      `exit ${result.exitCode} · ${elapsed}ms`,
    );
    scheduleShellPersist(result.env);
  } catch (err) {
    if (err instanceof CancelledError || err?.name === "AbortError") {
      term.writeln(dim("[cancelled]"));
      addLog("warn", "script cancelled");
    } else {
      const message = err instanceof Error ? err.message : String(err);
      term.writeln(red(`error: ${message}`));
      addLog("error", `script failed: ${message}`);
    }
    scheduleShellPersist();
  } finally {
    shellAbort = null;
    ttyTool = null;
    busy = false;
    if (!kubectlRestarting && !cliRestarting) setStatus("connected", "connected");
    prompt();
    void poll();
  }
}

function submit() {
  const current = line.trimEnd();
  line = "";
  cursor = 0;
  term.write("\r\n");

  if (!current && continuation.length === 0) {
    prompt();
    return;
  }

  continuation.push(current);
  const script = continuation.join("\n");
  if (needsContinuation(script)) {
    term.write(continuationPrompt());
    return;
  }
  continuation = [];

  if (!current) {
    prompt();
    return;
  }

  history.push(current);
  saveHistory();
  historyIndex = history.length;

  if (!ready) {
    term.writeln(red("still loading WASM modules…"));
    prompt();
    return;
  }

  const words = splitArgs(current);
  if (needsStdin(words) && !script.includes("\n") && !/[|<>&]/.test(current)) {
    stdinMode = true;
    stdinScript = script;
    stdinLines = [];
    stdinCurrent = "";
    term.writeln(
      dim("stdin mode: paste or type the manifest. Ctrl-D runs, Ctrl-C aborts."),
    );
    term.write(stdinPrompt());
    return;
  }

  void runScript(script);
}

function abortStdin() {
  stdinMode = false;
  stdinScript = "";
  stdinLines = [];
  stdinCurrent = "";
  term.write("^C\r\n");
  addLog("warn", "stdin aborted");
  prompt();
}

function finishStdin() {
  if (stdinCurrent) {
    stdinLines.push(stdinCurrent);
    stdinCurrent = "";
  }
  const stdin = stdinLines.length > 0 ? `${stdinLines.join("\n")}\n` : "";
  const script = stdinScript;
  stdinMode = false;
  stdinScript = "";
  stdinLines = [];
  term.write("\r\n");
  term.writeln(dim(`[stdin ${stdin.split("\n").length - 1} line(s), ${stdin.length} bytes]`));
  void runScript(script, stdin);
}

function handleStdinData(data) {
  if (data.startsWith("\u001b")) return;
  for (const ch of data) {
    if (ch === "\r" || ch === "\n") {
      stdinLines.push(stdinCurrent);
      stdinCurrent = "";
      term.write("\r\n" + stdinPrompt());
    } else if (ch === "\u007f") {
      if (stdinCurrent.length > 0) {
        stdinCurrent = stdinCurrent.slice(0, -1);
        term.write("\b \b");
      }
    } else if (ch === "\t") {
      stdinCurrent += "  ";
      term.write("  ");
    } else if (ch >= " ") {
      stdinCurrent += ch;
      term.write(ch);
    }
  }
}

function cancelRunning() {
  line = "";
  cursor = 0;
  if (continuation.length > 0) {
    continuation = [];
    term.write("\r\x1b[2K^C");
    prompt();
    return;
  }
  if (!busy) {
    term.write("\r\x1b[2K");
    prompt();
    return;
  }
  term.write("^C");
  addLog("warn", "Ctrl-C: cancelling");
  shellAbort?.abort();
  if (kubectlInFlight > 0) void restartKubectl("Ctrl-C");
  if (cliInFlight > 0) void restartCli("Ctrl-C");
}

// --- completion ------------------------------------------------------------

function commonPrefix(values) {
  if (values.length === 0) return "";
  let prefix = values[0];
  for (const value of values.slice(1)) {
    let index = 0;
    while (
      index < prefix.length &&
      index < value.length &&
      prefix[index] === value[index]
    ) {
      index++;
    }
    prefix = prefix.slice(0, index);
  }
  return prefix;
}

function shellCommandNames() {
  const builtins = [
    "cd", "export", "unset", "alias", "source", ".", "history",
    "kubectl", "goat", "yq", "help", "clear", "reset",
  ];
  return [...new Set([...getCommandNames(), ...builtins])].sort();
}

async function pathCompletions(token) {
  const slash = token.lastIndexOf("/");
  const directory = slash === -1 ? "." : (token.slice(0, slash) || "/");
  const prefix = slash === -1 ? token : token.slice(slash + 1);
  const absolute = directory.startsWith("/")
    ? directory
    : `${bash.getCwd().replace(/\/$/, "")}/${directory}`;
  const names = await bash.fs.readdir(absolute).catch(() => []);
  const results = [];
  for (const name of names) {
    if (!name.startsWith(prefix)) continue;
    const parent = slash === -1 ? token.slice(0, token.length - prefix.length) : token.slice(0, slash + 1);
    let isDir = false;
    try {
      isDir = (await bash.fs.stat(`${absolute.replace(/\/$/, "")}/${name}`)).isDirectory;
    } catch {
      isDir = false;
    }
    results.push({ value: `${parent}${name}${isDir ? "/" : ""}`, description: isDir ? "dir" : "" });
  }
  return results;
}

async function kubectlCompletions(parts, token) {
  const result = await runKubectl([
    "__complete",
    `--server=${apiServer}`,
    ...parts,
    token,
  ]);
  return String(result.stdout ?? "")
    .split("\n")
    .filter((entry) => entry && !entry.startsWith(":") && !entry.startsWith("_activeHelp_"))
    .map((entry) => {
      const [value, ...description] = entry.split("\t");
      return { value, description: description.join(" ").trim() };
    });
}

async function completeLine() {
  if (completing || busy || stdinMode || !ready || continuation.length > 0) return;
  const original = line;
  const endsWithSpace = /\s$/.test(original);
  const parts = splitArgs(original);
  const token = endsWithSpace ? "" : (parts.pop() ?? "");

  completing = true;
  try {
    let candidates;
    if (parts[0] === "kubectl") {
      candidates = await kubectlCompletions(parts.slice(1), token);
    } else if (parts.length === 0) {
      candidates = shellCommandNames()
        .filter((name) => name.startsWith(token))
        .map((name) => ({ value: name, description: "" }));
    } else {
      candidates = await pathCompletions(token);
    }

    if (line !== original || busy) return;
    if (candidates.length === 0) {
      term.write("\x07");
      return;
    }

    const base = endsWithSpace ? line : line.slice(0, line.length - token.length);
    if (candidates.length === 1) {
      line = `${base}${candidates[0].value}${candidates[0].value.endsWith("/") ? "" : " "}`;
      cursor = line.length;
      redraw();
      return;
    }

    const prefix = commonPrefix(candidates.map((candidate) => candidate.value));
    if (prefix.length > token.length) {
      line = `${base}${prefix}`;
      cursor = line.length;
    }

    term.write("\r\n");
    for (const candidate of candidates.slice(0, 40)) {
      term.writeln(
        candidate.description
          ? `${cyan(candidate.value)}  ${dim(candidate.description)}`
          : candidate.value,
      );
    }
    if (candidates.length > 40) {
      term.writeln(dim(`... ${candidates.length - 40} more`));
    }
    term.write("\r\n");
    redraw();
  } catch (err) {
    addLog("warn", `completion failed: ${err instanceof Error ? err.message : err}`);
  } finally {
    completing = false;
  }
}

// --- live Pele diagram -----------------------------------------------------

let lastDiagram = "";
let polling = false;
let diagramTargets = new Map();

diagramEl.addEventListener("click", (event) => {
  const node = event.target.closest?.("[data-id]");
  if (!node) return;
  const command = diagramTargets.get(node.getAttribute("data-id"));
  if (!command) return;
  if (busy || stdinMode) {
    addLog("warn", "busy; cannot describe from the diagram right now");
    return;
  }
  term.focus();
  term.write(`\r\n${promptText()}${command}`);
  line = command;
  cursor = line.length;
  submit();
});

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
    mermaid = buildDiagram(
      apiServer,
      crds.items ?? [],
      fetches.items ?? [],
      isNarrow() ? "TD" : "LR",
    );

    const targets = new Map();
    for (const crd of crds.items ?? []) {
      const name = crd?.metadata?.name;
      if (name) targets.set(nodeID("crd", name), `kubectl describe crd ${name}`);
    }
    for (const fetch of fetches.items ?? []) {
      const name = fetch?.metadata?.name;
      if (name) targets.set(nodeID("fetch", name), `kubectl describe fetch ${name}`);
    }
    diagramTargets = targets;
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
    const crdCount = crds?.items?.length ?? 0;
    const items = fetches?.items ?? [];
    renderDiagram(
      mermaid,
      crds
        ? `${crdCount} CRD${crdCount === 1 ? "" : "s"} · ${items.length} Fetch${items.length === 1 ? "" : "es"}`
        : "error",
    );
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

function renderDiagram(mermaid, summary = "") {
  diagramEl.replaceChildren();
  const stamp = new Date().toLocaleTimeString();
  diagramUpdatedEl.textContent = summary ? `updated ${stamp} · ${summary}` : `updated ${stamp}`;
  try {
    pele.mount(diagramEl, mermaid);
  } catch (err) {
    const pre = document.createElement("pre");
    pre.className = "error";
    pre.textContent = String(err);
    diagramEl.appendChild(pre);
  }
}

// --- startup ---------------------------------------------------------------

async function start() {
  term.writeln(cyan("kcp-wasm") + dim(" loading kcp.wasm…"));
  setStatus("loading kcp…", "running");
  await spawnKcp();

  term.writeln(cyan("kubectl-wasm") + dim(" loading kubectl.wasm (122 MB)…"));
  setStatus("loading kubectl…", "running");
  await spawnKubectl();

  term.writeln(cyan("cli-wasm") + dim(" loading goat.wasm + yq.wasm…"));
  setStatus("loading goat + yq…", "running");
  try {
    await spawnCli();
  } catch (err) {
    addLog("warn", `goat/yq unavailable: ${err instanceof Error ? err.message : err}`);
  }

  term.writeln(dim("loading just-bash shell…"));
  await restoreShell();

  ready = true;
  retryEl.hidden = true;
  setStatus("connected", "connected");
  term.writeln(dim(`kcp + kubectl + goat + yq running in this browser tab (api ${apiServer})`));
  term.writeln(dim("type 'help' for examples; Ctrl-C cancels a running command"));
  prompt();
  term.focus();

  void poll();
  setInterval(() => void poll(), intervalMs);
}

// Dropping a manifest runs `apply -f -` with the file contents as stdin.
const terminalPaneEl = document.getElementById("terminal-pane");
terminalPaneEl.addEventListener("dragover", (event) => {
  event.preventDefault();
});
terminalPaneEl.addEventListener("drop", async (event) => {
  event.preventDefault();
  const file = event.dataTransfer?.files?.[0];
  if (!file) return;
  if (busy || stdinMode || !ready) {
    addLog("warn", "busy; cannot apply the dropped manifest right now");
    return;
  }
  const text = await file.text();
  term.focus();
  term.write(
    `\r\n${promptText()}kubectl apply -f - ${dim(`(dropped ${file.name}, ${text.length} bytes)`)}\r\n`,
  );
  void runScript("kubectl apply -f -", text);
});

// --- input -----------------------------------------------------------------

term.onData((data) => {
  if (stdinMode) {
    if (data === "\u0003") {
      abortStdin();
      return;
    }
    if (data === "\u0004") {
      finishStdin();
      return;
    }
    handleStdinData(data);
    return;
  }

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
  if (data === "\t") {
    void completeLine();
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
  for (let index = 0; index < data.length; index++) {
    const ch = data[index];
    if (ch === "\r" || ch === "\n") {
      insertText(text);
      text = "";
      submit();
      const rest = data.slice(index + 1);
      if (rest) {
        if (stdinMode) handleStdinData(rest);
        else insertText(rest);
      }
      return;
    }
    if (ch >= " ") text += ch;
  }
  insertText(text);
});

// Touch devices have no Ctrl key: expose the control actions the terminal
// needs on a small toolbar.
document.getElementById("mobile-keys").addEventListener("click", (event) => {
  const action = event.target.closest("button")?.dataset.action;
  if (!action) return;
  term.focus();
  if (action === "ctrl-c") {
    if (stdinMode) abortStdin();
    else cancelRunning();
    return;
  }
  if (action === "ctrl-d") {
    if (stdinMode) finishStdin();
    return;
  }
  if (busy) return;
  if (stdinMode) {
    if (action === "enter") handleStdinData("\r");
    else if (action === "tab") handleStdinData("\t");
    return;
  }
  if (action === "enter") {
    submit();
  } else if (action === "tab") {
    void completeLine();
  } else if (action === "up") {
    if (history.length > 0) {
      historyIndex = Math.max(0, historyIndex - 1);
      replaceLine(history[historyIndex] ?? "");
    }
  }
});

let lastNarrow = isNarrow();
window.addEventListener("resize", () => {
  if (isNarrow() === lastNarrow) return;
  lastNarrow = isNarrow();
  // Different Mermaid direction changes the diagram text; force a repaint.
  lastDiagram = "";
  void poll();
});

setStatus("starting…", "running");
start().catch((err) => {
  setStatus("failed", "disconnected");
  retryEl.hidden = false;
  addLog("error", `startup failed: ${err instanceof Error ? err.message : err}`);
  term.writeln(red(`startup failed: ${err instanceof Error ? err.message : err}`));
});
