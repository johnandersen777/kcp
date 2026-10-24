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
import { OAUTH_CALLBACK_MESSAGE, openOAuthPopup } from "./oauth.js";
import { assembleSnapshot, openBestBackend, snapshotRecords } from "./state_store.js";
import { decodeBase64, encodeBase64, register as registerIngress } from "./ingress_ws.js";
import * as vmshell from "./vmshell.js";
import * as webrtcGuest from "./webrtc_guest.js";
import * as sshCmd from "./ssh_cmd.js";
import { discardSnapshot } from "./kcp_hibernate.js";

const config = await (await fetch("/config")).json();
const apiServer = String(config.apiServer);
const intervalMs = Number(config.intervalMs ?? 1500);
const stateKey = "kcp";
const socialwebStateKey = "socialweb";
const legacyStateKey = "kcp-wasm-state";
// kcp storage is saved as one IndexedDB record per object under this prefix;
// the single-string snapshot under stateKey is the pre-delta format.
const kcpRecordPrefix = `${stateKey}\u0000`;
const stateDB = "kcp-wasm";
const stateMaxBytes = 256 * 1024 * 1024;
const shellStateKey = "kcp-shell-state";
const maxLogEntries = 500;
const homeDir = "/home/user";

const statusEl = document.getElementById("status");
const retryEl = document.getElementById("retry");
retryEl.addEventListener("click", () => location.reload());
const serverEl = document.getElementById("server");
const diagramEl = document.getElementById("diagram");
const diagramUpdatedEl = document.getElementById("diagram-updated");
const diagramToggleEl = document.getElementById("diagram-toggle");
let diagramVisible = localStorage.getItem("kcp-diagram-open") === "1";
const logEl = document.getElementById("log");
const logClearEl = document.getElementById("log-clear");
const logVerboseEl = document.getElementById("log-verbose");
const leftPaneEl = document.getElementById("left-pane");
const logPaneEl = document.getElementById("log-pane");
const splitterEl = document.getElementById("splitter");
const logToggleEl = document.getElementById("log-toggle");
const identityModeEl = document.getElementById("identity-mode");
const identityHandleEl = document.getElementById("identity-handle");
const identitySignInEl = document.getElementById("identity-signin");
const identityStatusEl = document.getElementById("identity-status");

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
    const size = isNarrow() ? 11 : 13;
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

// xterm.js has no native touch scrolling (xtermjs/xterm.js#5377). Translate
// single-finger vertical drags into scrollLines(), with sub-row remainder and
// momentum. touch-action:none on the host keeps iOS page rubber-banding out.
const touchHost = document.getElementById("terminal");
let touchLastY = null;
let touchAccum = 0;
let touchVelocity = 0;
let touchLastTime = 0;
let momentumFrame = null;

function stopTouchMomentum() {
  if (momentumFrame !== null) {
    cancelAnimationFrame(momentumFrame);
    momentumFrame = null;
  }
}

function scrollPixels(dy) {
  const rowHeight = Math.max(
    1,
    (term.element?.clientHeight ?? 300) / Math.max(1, term.rows),
  );
  touchAccum += dy;
  const lines = Math.trunc(touchAccum / rowHeight);
  if (lines !== 0) {
    touchAccum -= lines * rowHeight;
    // Finger drag down (dy > 0) reveals older output: scroll up.
    term.scrollLines(-lines);
  }
}

touchHost.addEventListener("touchstart", (event) => {
  if (event.touches.length !== 1) {
    touchLastY = null;
    return;
  }
  stopTouchMomentum();
  touchLastY = event.touches[0].clientY;
  touchAccum = 0;
  touchVelocity = 0;
  touchLastTime = performance.now();
}, { passive: true, capture: true });

touchHost.addEventListener("touchmove", (event) => {
  if (touchLastY === null || event.touches.length !== 1) return;
  const y = event.touches[0].clientY;
  const dy = y - touchLastY;
  touchLastY = y;
  if (dy === 0) return;
  event.preventDefault();
  event.stopPropagation();
  const now = performance.now();
  const dt = Math.max(1, now - touchLastTime);
  touchVelocity = 0.8 * (dy / dt) + 0.2 * touchVelocity;
  touchLastTime = now;
  scrollPixels(dy);
}, { passive: false, capture: true });

function finishTouchScroll() {
  touchLastY = null;
  const velocity = touchVelocity;
  touchVelocity = 0;
  if (Math.abs(velocity) < 0.05) return;
  let v = velocity;
  const step = () => {
    v *= 0.94;
    if (Math.abs(v) < 0.02) {
      momentumFrame = null;
      return;
    }
    scrollPixels(v * 16);
    momentumFrame = requestAnimationFrame(step);
  };
  momentumFrame = requestAnimationFrame(step);
}

touchHost.addEventListener("touchend", finishTouchScroll, {
  passive: true,
  capture: true,
});
touchHost.addEventListener("touchcancel", () => {
  touchLastY = null;
  touchVelocity = 0;
}, { passive: true, capture: true });

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

// term.write parses asynchronously; scroll after the rows exist so the new
// prompt is the last thing in view.
function scrollToBottomSettled() {
  term.scrollToBottom();
  requestAnimationFrame(() => term.scrollToBottom());
  setTimeout(() => term.scrollToBottom(), 120);
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
        resolve(event.data);
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
const pendingPageRequests = new Map();
let nextPageRequestID = -1;

// Send a request straight to the kcp/socialweb bridge. kubectl has its own
// worker; the identity callback needs this page-origin path.
function kcpPageRequest(request, timeoutMs = 60_000) {
  const worker = kcpWorker;
  if (!worker) return Promise.reject(new Error("kcp worker is not running"));
  const id = nextPageRequestID--;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      if (pendingPageRequests.delete(id)) {
        reject(new Error("kcp request timed out"));
      }
    }, timeoutMs);
    pendingPageRequests.set(id, { resolve, reject, timer });
    worker.postMessage({ type: "request", id, request });
  });
}

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
    if (data.type === "state") {
      applyState(data.state);
      return;
    }
    if (data.type === "socialweb-ingress") {
      applyIngress(data.ingress ?? []);
      return;
    }
    if (data.type === "response") {
      const page = pendingPageRequests.get(data.id);
      if (page) {
        pendingPageRequests.delete(data.id);
        clearTimeout(page.timer);
        if (data.error) page.reject(new Error(String(data.error)));
        else page.resolve(data.response);
        return;
      }
      pendingBridges.delete(data.id);
      noteKcpAnswer();
      kubectlWorker?.postMessage({
        type: "fetchResult",
        id: data.id,
        request: { response: data.response, error: data.error },
      });
    } else if (data.type === "persist") {
      persistKcp(data.snapshot, data.seq);
    } else if (data.type === "socialweb-persist") {
      schedulePersist(data.snapshot, socialwebStateKey, data.seq);
    } else if (data.type === "log") {
      addLog(data.level ?? "info", `kcp: ${data.message}`);
      const message = String(data.message);
      if (message.includes("MiB downloaded")) {
        setStatus(message.replace("kcp.wasm ", "kcp "), "running");
      } else if (message.includes("waiting for real kcp readiness")) {
        const match = message.match(/\((\d+)s\)/);
        setStatus(`booting kcp ${match ? match[1] : ""}s…`, "running");
      }
    }
  });
}

let stateBackendPromise = null;
function stateBackend() {
  if (!stateBackendPromise) {
    stateBackendPromise = openBestBackend({ dbName: stateDB, maxBytes: stateMaxBytes })
      .then(({ backend, kind, reason }) => {
        addLog("info", `kcp state backend: ${kind}`);
        if (kind === "memory" && reason) {
          addLog("warn", `IndexedDB unavailable (${reason.message ?? reason}); state will not survive reload`);
        }
        return backend;
      });
  }
  return stateBackendPromise;
}

async function loadInitialState(key = stateKey) {
  const backend = await stateBackend();
  const saved = await backend.load(key);
  if (saved || key !== stateKey) return saved ?? "";
  // One-time migration from the 5 MB localStorage era.
  const legacy = localStorage.getItem(legacyStateKey);
  if (legacy) {
    await backend.save(stateKey, legacy).catch(() => {});
    localStorage.removeItem(legacyStateKey);
    return legacy;
  }
  return "";
}

// Rebuilds the full snapshot kcp restores from: per-object records when they
// exist, otherwise a saved single-string snapshot.
async function loadKcpState() {
  const backend = await stateBackend();
  const records = await backend.loadPrefix(kcpRecordPrefix);
  if (records.length > 0) return JSON.stringify(assembleSnapshot(records));
  return loadInitialState(stateKey);
}

// kcp posts a full snapshot after boot and deltas afterwards, so every
// message is applied, in order; none may be coalesced away.
let kcpPersistQueue = Promise.resolve();
function persistKcp(snapshot, seq) {
  kcpPersistQueue = kcpPersistQueue.then(async () => {
    const message = JSON.parse(snapshot);
    const backend = await stateBackend();
    if (!message.stores) {
      // Not a storage envelope (older shard): keep it as one string.
      await backend.apply({ clearPrefix: kcpRecordPrefix });
      await backend.save(stateKey, snapshot);
      return;
    }
    const ops = snapshotRecords(kcpRecordPrefix, message);
    if (!message.delta) ops.deletes.push(stateKey);
    // Same transaction as the data: the hibernation snapshot compares it.
    if (seq) ops.puts.push([kcpSeqKey, seq]);
    await backend.apply(ops);
  }).catch((err) => {
    addLog("warn", `kcp state save failed: ${err instanceof Error ? err.message : err}`);
  });
}

const persistTimers = new Map();
const persistInFlight = new Set();
function schedulePersist(snapshot, key = stateKey, seq = 0) {
  if (persistTimers.has(key)) clearTimeout(persistTimers.get(key));
  persistTimers.set(key, setTimeout(async () => {
    persistTimers.delete(key);
    if (persistInFlight.has(key)) {
      schedulePersist(snapshot, key, seq);
      return;
    }
    persistInFlight.add(key);
    try {
      const backend = await stateBackend();
      if (seq) await backend.apply({ puts: [[key, snapshot], [`${key}-seq`, seq]] });
      else await backend.save(key, snapshot);
    } catch (err) {
      addLog("warn", `state save failed (${key}): ${err instanceof Error ? err.message : err}`);
    } finally {
      persistInFlight.delete(key);
    }
  }, 250));
}

async function spawnKcp(useSnapshot = true) {
  const spawnStarted = performance.now();
  const [initialState, socialwebSnapshot, hibernate] = await Promise.all([
    loadKcpState(),
    loadInitialState(socialwebStateKey),
    hibernateInit(useSnapshot),
  ]);
  const worker = new Worker("/kcp_worker.js", { type: "module" });
  attachKcp(worker);
  const readyPromise = waitReady(worker);
  worker.postMessage({
    type: "init",
    wasmUrl: "/kcp.wasm",
    execUrl: "/wasm_exec.js",
    initialState,
    hibernate,
    debugGo: localStorage.getItem("kcp.debugGo") === "1",
    socialweb: {
      origin: location.origin,
      identityMode: localStorage.getItem("socialweb.identity") === "localRepo"
        ? "oauthBrowser"
        : (localStorage.getItem("socialweb.identity") ?? "oauthBrowser"),
      handle: localStorage.getItem("socialweb.handle") ?? "",
      did: localStorage.getItem("socialweb.did") ?? "",
      pdsUrl: localStorage.getItem("socialweb.pdsUrl") ?? "",
      oauthClientId: `${location.origin}/oauth-client-metadata.json`,
      oauthRedirectUri: `${location.origin}/oauth/callback/requester`,
      forceApply: localStorage.getItem("socialweb.forceApply") === "1",
      statusFeed: localStorage.getItem("socialweb.statusFeed") !== "0",
      restore: socialwebSnapshot,
    },
  });
  const ready = await readyPromise;
  if (ready?.restart) {
    worker.terminate();
    return spawnKcp(false);
  }
  logBootKind(ready, spawnStarted);
  kcpWorker = worker;
  worker.postMessage({ type: "state-subscribe" });
}

// --- kcp hibernation (kcp_hibernate.js) --------------------------------------

const kcpSeqKey = `${kcpRecordPrefix}q`;
const socialwebSeqKey = `${socialwebStateKey}-seq`;
let firstKcpAnswerLogged = false;
let kcpSpawnStarted = 0;

// Snapshots are per user and local; localStorage "kcp.snapshot" = "0" turns
// them off, "kcp.snapshot.gzip" = "0" stores them raw, "kcp.snapshot.store" =
// "indexeddb" forces the fallback store.
async function hibernateInit(useSnapshot) {
  const enabled = localStorage.getItem("kcp.snapshot") !== "0";
  if (enabled) {
    navigator.storage?.persist?.().then((granted) => {
      if (!granted) addLog("debug", "persistent storage not granted; the browser may evict the kcp snapshot");
    }).catch(() => {});
  }
  const backend = await stateBackend();
  const [kcp, socialweb] = await Promise.all([
    backend.load(kcpSeqKey).catch(() => null),
    backend.load(socialwebSeqKey).catch(() => null),
  ]);
  return {
    enabled,
    restore: useSnapshot,
    persistSeq: { kcp: Number(kcp ?? 0), socialweb: Number(socialweb ?? 0) },
    gzip: localStorage.getItem("kcp.snapshot.gzip") !== "0",
    store: localStorage.getItem("kcp.snapshot.store") ?? undefined,
  };
}

function logBootKind(ready, spawnStarted) {
  kcpSpawnStarted = spawnStarted;
  const seconds = ((performance.now() - spawnStarted) / 1000).toFixed(1);
  if (ready?.restored) {
    addLog("ok", `kcp restored from snapshot (${seconds}s)`);
    addLog("debug", `kcp snapshot restore: ${JSON.stringify(ready.restored)}`);
  } else {
    addLog("ok", `kcp cold boot (${seconds}s)`);
  }
}

// Logged once: time from spawn to the first kcp answer kubectl got.
function noteKcpAnswer() {
  if (firstKcpAnswerLogged || !kcpSpawnStarted) return;
  firstKcpAnswerLogged = true;
  addLog("debug", `first kubectl answer ${((performance.now() - kcpSpawnStarted) / 1000).toFixed(1)}s after kcp start`);
}

// Capture when the tab is hidden: the next visit restores from it.
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "hidden") kcpWorker?.postMessage({ type: "hibernate" });
});

// Deletes the stored snapshot; the next load cold boots. The running worker
// stops capturing until reload.
async function discardKcpSnapshot() {
  if (kcpWorker) {
    kcpWorker.postMessage({ type: "snapshot-discard" });
    return;
  }
  await discardSnapshot();
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
      // kcp restarts (e.g. a discarded snapshot) leave a gap with no worker;
      // fail the request instead of leaving kubectl waiting for its timeout.
      if (!kcpWorker) {
        worker.postMessage({ type: "fetchResult", id: data.id, request: { error: "kcp worker is not running" } });
        return;
      }
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

let cliLoading = null;
function ensureCli() {
  cliLoading ??= spawnCli().catch((err) => {
    cliLoading = null;
    addLog("warn", `goat/yq unavailable: ${err instanceof Error ? err.message : err}`);
    throw err;
  });
  return cliLoading;
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
    "  ssh vmshell-abc123            (a Held ComputeContract, over WebRTC; see ~/.ssh/config)",
    "  ssh vmshell-abc123 uname -a | grep Linux",
    "",
    "builtins: help · clear · reset (wipe kcp state and reload) · snapshot-discard (cold boot next load)",
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
  return ["kubectl", "goat", "yq", "ssh"].includes(first) ? first : null;
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
    if (!cliWorker && !cliRestarting) await ensureCli().catch(() => {});
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

  const snapshotDiscard = defineCommand("snapshot-discard", async () => {
    await discardKcpSnapshot();
    return { stdout: "kcp snapshot discarded; the next load cold boots\n", stderr: "", exitCode: 0 };
  });

  const reset = defineCommand("reset", async () => {
    // A worker may hold the snapshot's OPFS file open; stop it first.
    kcpWorker?.terminate();
    kcpWorker = null;
    await discardSnapshot().catch(() => {});
    try {
      const backend = await stateBackend();
      await backend.remove(stateKey);
      await backend.apply({ clearPrefix: kcpRecordPrefix });
      await backend.remove(socialwebStateKey);
      await backend.remove(socialwebSeqKey);
    } catch {
      // A missing or read-only store is already a reset.
    }
    localStorage.removeItem(legacyStateKey);
    localStorage.removeItem(shellStateKey);
    addLog("warn", "shell, kcp and socialweb state cleared; reloading");
    setTimeout(() => location.reload(), 300);
    return { stdout: "cleared shell and kcp state; reloading…\n", stderr: "", exitCode: 0 };
  });

  const ssh = defineCommand("ssh", (args, ctx) => runSsh(args, ctx));

  return [kubectl, goat, yq, ssh, help, clear, reset, snapshotDiscard];
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
    env: { HOME: homeDir, ...(saved.env ?? {}) },
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

function visiblePromptLength() {
  const cwd = bash?.getCwd() ?? homeDir;
  return cwd.length + 3; // "<cwd> $ "
}

function replaceLine(next) {
  // Erase the whole logical input, including wrapped rows, then redraw.
  // Assumes the cursor is at the end of the current input (history recall).
  const columns = term.cols || 80;
  const rows = Math.max(1, Math.ceil((visiblePromptLength() + line.length) / columns));
  if (rows > 1) term.write(`\x1b[${rows - 1}A`);
  term.write("\r\x1b[0J");
  line = next;
  cursor = next.length;
  term.write(promptText() + line);
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
    scrollToBottomSettled();
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
    scrollToBottomSettled();
    void poll();
    runQueuedDiagramCommand();
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
    "kubectl", "goat", "yq", "ssh", "help", "clear", "reset", "snapshot-discard",
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
    } else if (parts[0] === "ssh" && !token.startsWith("-") && !parts.slice(1).some((p) => !p.startsWith("-"))) {
      candidates = sshCmd.sshHosts(lastFeedState, { testPeer: Boolean(sshTestPeer) })
        .filter((name) => name.startsWith(token))
        .map((name) => ({ value: name, description: "ComputeContract" }));
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

function applyDiagramVisibility() {
  diagramEl.classList.toggle("hidden", !diagramVisible);
  diagramToggleEl.textContent = diagramVisible ? "hide" : "show";
  if (!diagramVisible) diagramUpdatedEl.textContent = "closed";
}

diagramToggleEl.addEventListener("click", () => {
  diagramVisible = !diagramVisible;
  localStorage.setItem("kcp-diagram-open", diagramVisible ? "1" : "0");
  applyDiagramVisibility();
  if (diagramVisible) void poll();
});
applyDiagramVisibility();
let pendingDiagramCommand = null;
let diagramTargets = new Map();

function runQueuedDiagramCommand() {
  if (!pendingDiagramCommand || busy || booting || bootActive || stdinMode) return;
  const command = pendingDiagramCommand;
  pendingDiagramCommand = null;
  term.focus();
  term.write(`\r\n${promptText()}${command}`);
  line = command;
  cursor = line.length;
  submit();
}

diagramEl.addEventListener("click", (event) => {
  const node = event.target.closest?.("[data-id]");
  if (!node) return;
  const command = diagramTargets.get(node.getAttribute("data-id"));
  if (!command) return;
  if (busy || booting) {
    pendingDiagramCommand = command;
    addLog("info", `queued diagram command: ${command}`);
    return;
  }
  if (stdinMode) {
    addLog("warn", "finish or abort stdin before describing from the diagram");
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
  if (!ready || polling || busy || bootActive || stdinMode || !diagramVisible) return;
  polling = true;
  let mermaid = null;
  let crds = null;
  try {
    crds = await kubectlJSON(["get", "crds"]);
    mermaid = buildDiagram(
      apiServer,
      crds.items ?? [],
      [],
      isNarrow() ? "TD" : "LR",
    );

    const targets = new Map();
    for (const crd of crds.items ?? []) {
      const name = crd?.metadata?.name;
      if (name) targets.set(nodeID("crd", name), `kubectl describe crd ${name}`);
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
    renderDiagram(
      mermaid,
      crds ? `${crdCount} CRD${crdCount === 1 ? "" : "s"}` : "error",
    );
    addLog(crds ? "info" : "warn", `state: ${crdCount} CRD(s)`);
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


// Boot sequence: show the real in-browser kcp surface. The full socialweb
// provider/app manifests are applied by the worker; these commands are a
// clean, copy-pasteable checkpoint that they are being served.
const BOOT_STEPS = [
  "kubectl get workspaces",
  "kubectl get shards.core.kcp.io",
  "kubectl get --raw /clusters/root/version | jq -r '.gitVersion'",
];

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
let booting = false;
let bootActive = false;

async function typeBootSteps(steps) {
  if (!ready || !kubectlWorker) return;
  bootActive = true;
  booting = true;
  busy = true;

  for (let stepIndex = 0; stepIndex < steps.length; stepIndex++) {
    const lines = steps[stepIndex].split("\n");
    let heredoc = null;
    for (let i = 0; i < lines.length; i++) {
      const current = lines[i];
      if (i === 0) {
        if (stepIndex === 0) term.write(promptText());
        // Later steps already have a prompt from the previous run.
      } else {
        term.write(`\r\n${heredoc ? continuationPrompt() : promptText()}`);
      }
      if (heredoc) {
        if (current.trim() === heredoc) heredoc = null;
      } else {
        const match = current.match(/<<-?\s*(['"]?)([A-Za-z0-9_]+)\1/);
        if (match) heredoc = match[2];
      }
      for (const ch of current) {
        term.write(ch);
        await sleep(2);
      }
    }

    const entry = steps[stepIndex].trim();
    if (entry && history[history.length - 1] !== entry) {
      history.push(entry);
      saveHistory();
    }
    historyIndex = history.length;

    // Visible Enter press before the step runs.
    await sleep(150);
    term.write("\r\n");
    await sleep(150);
    booting = false;
    await runScript(steps[stepIndex]);
    booting = true;
    busy = true;
  }
  booting = false;
  bootActive = false;
  busy = false;
  setStatus("connected", "connected");
  runQueuedDiagramCommand();
}

// --- socialweb requester UI ------------------------------------------------

const identityPrefs = {
  mode: "socialweb.identity",
  handle: "socialweb.handle",
  pdsUrl: "socialweb.pdsUrl",
};

let pendingSignInURL = "";
let signInWaiting = false;

function setAppStatus(el, text, className = "") {
  el.textContent = text;
  el.className = `app-status ${className}`.trim();
}

function requesterIdentity() {
  return {
    mode: identityModeEl.value,
    handle: identityHandleEl.value.trim(),
    association: "Skip",
  };
}

async function patchRequesterIdentity() {
  const identity = requesterIdentity();
  localStorage.setItem(identityPrefs.mode, identity.mode);
  localStorage.setItem(identityPrefs.handle, identity.handle);
  const patch = JSON.stringify({ spec: { identity } });
  const result = await runKubectl(withServer([
    "patch", "marketrequester", "app", "--type=merge", "-p", patch,
  ]));
  if (result.code !== 0) {
    throw new Error(String(result.stderr ?? "patch failed").trim());
  }
  addLog("info", `identity ${identity.mode} handle=${identity.handle || "(unset)"}`);
}

async function completeOAuth(message) {
  const query = new URLSearchParams();
  for (const key of ["code", "state", "iss", "error", "error_description"]) {
    const value = message?.[key];
    if (value) query.set(key, String(value));
  }
  setAppStatus(identityStatusEl, "completing sign-in…", "warn");
  const response = await kcpPageRequest({
    method: "GET",
    path: `/oauth/callback/requester?${query.toString()}`,
    headers: { Host: location.host, Accept: "text/plain" },
  });
  if (response.status >= 400) {
    throw new Error(String(response.body ?? `callback returned ${response.status}`));
  }
  addLog("ok", "OAuth callback accepted");
  setAppStatus(identityStatusEl, "signed in; waiting for MarketIdentity", "ok");
}

async function beginSignIn() {
  try {
    identitySignInEl.disabled = true;
    await patchRequesterIdentity();
    if (pendingSignInURL) {
      await openSignInPopup();
    } else {
      signInWaiting = true;
      setAppStatus(identityStatusEl, "starting OAuth… waiting for the PDS", "warn");
    }
  } catch (err) {
    signInWaiting = false;
    addLog("error", `sign-in failed: ${err instanceof Error ? err.message : err}`);
    setAppStatus(identityStatusEl, String(err.message ?? err), "err");
    identitySignInEl.disabled = false;
  }
}

async function openSignInPopup() {
  const url = pendingSignInURL;
  if (!url) throw new Error("the identity has no pending sign-in URL yet");
  signInWaiting = false;
  setAppStatus(identityStatusEl, "waiting for the AT Protocol popup…", "warn");
  const message = await openOAuthPopup(url);
  if (message.type !== OAUTH_CALLBACK_MESSAGE) return;
  await completeOAuth(message);
}

function parseTTLSeconds(value) {
  const text = value.trim();
  if (!text) return null;
  const match = text.match(/^(\d+)\s*(s|m|h|d)?$/i);
  if (!match) throw new Error("TTL must look like 30m, 1h or 3600s");
  const units = { s: 1, m: 60, h: 3600, d: 86400 };
  return Number(match[1]) * units[(match[2] ?? "s").toLowerCase()];
}

function applyState(state) {
  if (!state) return;
  applyComputeState(state);
  if (!state.marketRequester) {
    setAppStatus(identityStatusEl, "waiting for the MarketRequester app…", "warn");
    return;
  }
  identitySignInEl.disabled = false;
  const identity = state.marketIdentity;
  const phase = identity?.phase ?? "Pending";
  const action = identity?.pendingAction;
  pendingSignInURL = action && (action.kind === "visitUrl" || action.kind === "signInAgain")
    ? String(action.url ?? "")
    : "";
  if (pendingSignInURL && signInWaiting) {
    void openSignInPopup().catch((err) => {
      setAppStatus(identityStatusEl, String(err.message ?? err), "err");
    });
  }
  if (phase === "Ready") {
    const label = identity?.handle || identity?.did || "ready";
    setAppStatus(identityStatusEl, `MarketIdentity ready: ${label}`, "ok");
  } else if (pendingSignInURL) {
    setAppStatus(identityStatusEl, `sign-in required (${action.kind})\nclick Sign in to continue`, "warn");
  } else {
    setAppStatus(identityStatusEl, `MarketIdentity ${phase}`, "warn");
  }

  if (diagramVisible && Array.isArray(state.crds)) {
    const crds = state.crds.map((name) => ({
      metadata: { name },
      spec: { names: { kind: name.split(".")[0] } },
    }));
    const mermaid = buildDiagram(apiServer, crds, [], isNarrow() ? "TD" : "LR");
    if (mermaid !== lastDiagram) {
      lastDiagram = mermaid;
      renderDiagram(mermaid, `${crds.length} CRD${crds.length === 1 ? "" : "s"}`);
    }
  }
}

function initSocialwebUI() {
  const storedMode = localStorage.getItem(identityPrefs.mode);
  identityModeEl.value = storedMode === "localRepo" ? "oauthBrowser" : (storedMode ?? "oauthBrowser");
  if (storedMode === "localRepo") {
    // The local PDS adapter is not wired yet; boot with the supported mode.
    localStorage.setItem(identityPrefs.mode, "oauthBrowser");
  }
  identityHandleEl.value = localStorage.getItem(identityPrefs.handle) ?? "";
  identityModeEl.addEventListener("change", () => {
    localStorage.setItem(identityPrefs.mode, identityModeEl.value);
    setAppStatus(identityStatusEl, "mode saved; click Sign in to apply", "warn");
  });
  identityHandleEl.addEventListener("change", () => {
    localStorage.setItem(identityPrefs.handle, identityHandleEl.value.trim());
    setAppStatus(identityStatusEl, "handle saved; click Sign in to apply", "warn");
  });
  identitySignInEl.addEventListener("click", () => {
    void beginSignIn().catch((err) => {
      setAppStatus(identityStatusEl, String(err.message ?? err), "err");
    });
  });
  initComputeUI();
}

// --- compute -----------------------------------------------------------------
// One panel for ComputeContracts. "Request VM" applies a contract for the
// chosen bidders. Shell "ssh over WebRTC" (plan 0034) adds the webrtc ssh
// tunnel and the public half of a non-extractable WebCrypto Ed25519 key (kept
// in IndexedDB per contract). Every contract from the status feed is listed
// with its phase, its "Connect" action and its own Release button. One VM at a time: Request
// stays disabled while a contract is active, so repeated clicks cannot rent
// more. ?sshTestPeer=http://127.0.0.1:7795 adds a button that connects the
// pane to contrib/wasm/sshclient/testpeer instead of a guest.

const computeBiddersEl = document.getElementById("compute-bidders");
const computeSSHEl = document.getElementById("compute-ssh");
const computeTTLEl = document.getElementById("compute-ttl");
const computeShellEl = document.getElementById("compute-shell");
const computeRequestEl = document.getElementById("compute-request");
const computeStatusEl = document.getElementById("compute-status");
const computeListEl = document.getElementById("compute-list");
const vmshellIngressEl = document.getElementById("compute-ingress");
const computeTestPeerEl = document.getElementById("compute-testpeer");
const computePrefs = {
  bidders: "socialweb.vmshell.bidders",
  shell: "socialweb.compute.shell",
  transport: (name) => `socialweb.compute.transport.${name}`,
};
const sshTestPeer = new URLSearchParams(location.search).get("sshTestPeer");
const feedWatchers = new Set();
let lastFeedState = null;
let computeBusy = false;
let computeReleasing = false;
let computeLastRender = "";
const computeReleaseAsked = new Set();

// socialwebCall invokes a page-only Go export through kcp_worker.js.
function socialwebCall(fn, payload, timeoutMs = 30_000) {
  const worker = kcpWorker;
  if (!worker) return Promise.reject(new Error("kcp worker is not running"));
  const id = nextPageRequestID--;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      if (pendingPageRequests.delete(id)) reject(new Error(`${fn} timed out`));
    }, timeoutMs);
    pendingPageRequests.set(id, { resolve, reject, timer });
    worker.postMessage({ type: "socialweb-call", id, fn, payload: JSON.stringify(payload) });
  }).then((res) => {
    if (res?.error) throw new Error(String(res.error));
    return res;
  });
}

async function requestVM() {
  try {
    computeBusy = true;
    computeRequestEl.disabled = true;
    localStorage.setItem(computePrefs.bidders, computeBiddersEl.value.trim());
    localStorage.setItem(computePrefs.shell, computeShellEl.value);
    const bidders = vmshell.parseBidders(computeBiddersEl.value);
    const shell = computeShellEl.value;
    const ssh = computeSSHEl.value.trim();
    const sshKeys = ssh ? [ssh] : [];
    const ttlSeconds = parseTTLSeconds(computeTTLEl.value);
    const name = vmshell.contractName();
    if (shell === "webrtc") {
      const key = await webrtcGuest.newSSHKey();
      await webrtcGuest.saveSSHKey(name, key);
      sshKeys.unshift(key.authorized);
      localStorage.setItem(computePrefs.transport(name), "webrtc");
    }
    const identity = await kubectlJSON(["get", "marketidentity", "requester"]).catch(() => null);
    const manifest = vmshell.contractManifest({
      name, bidders, ownerDid: identity?.status?.did ?? "", shell, sshKeys, ttlSeconds,
    });
    const result = await runKubectl(withServer(["apply", "-f", "-"]), JSON.stringify(manifest));
    if (result.code !== 0) {
      throw new Error(String(result.stderr || result.stdout || "apply failed").trim());
    }
    setAppStatus(computeStatusEl, `${name} requested from ${bidders.join(", ")}`, "ok");
    addLog("ok", `ComputeContract ${name} requested from ${bidders.join(", ")}${shell !== "none" ? ` with a ${shell} shell` : ""}`);
  } catch (err) {
    setAppStatus(computeStatusEl, String(err.message ?? err), "err");
    computeBusy = false;
  }
  // computeBusy clears when the status feed shows the new contract.
}

async function releaseContract(name) {
  computeReleaseAsked.add(name);
  computeLastRender = "";
  const result = await runKubectl(withServer([
    "patch", "computecontract", name, "--type=merge", "-p", JSON.stringify({ spec: { release: true } }),
  ]));
  if (result.code !== 0) {
    computeReleaseAsked.delete(name);
    setAppStatus(computeStatusEl, String(result.stderr ?? "release failed").trim(), "err");
    return;
  }
  addLog("info", `ComputeContract ${name} released`);
  void webrtcGuest.deleteSSHKey(name).catch(() => {});
}

// The release runs in this tab's requester: closing the tab mid-release
// leaves the guest running until the bidder's release deadline. A reload
// resumes it, but warn before the tab goes away.
addEventListener("beforeunload", (event) => {
  if (!computeReleasing) return;
  event.preventDefault();
  event.returnValue = "";
});

function computeRow(contract) {
  const name = contract.name;
  const phase = contract.phase ?? "Pending";
  const li = document.createElement("li");
  const head = document.createElement("div");
  const title = document.createElement("strong");
  title.textContent = name;
  const phaseEl = document.createElement("span");
  phaseEl.className = `phase-${phase}`;
  phaseEl.textContent = ` ${phase}${contract.release && phase !== "Released" ? " · releasing" : ""}`;
  head.append(title, phaseEl);
  if (vmshell.isActive(contract) && !computeReleaseAsked.has(name)) {
    const release = document.createElement("button");
    release.type = "button";
    release.textContent = "Release";
    release.addEventListener("click", () => {
      release.disabled = true;
      void releaseContract(name);
    });
    head.append(release);
  }
  const detail = document.createElement("div");
  detail.className = "detail";
  detail.textContent = [
    contract.chosenDid ? `bidder ${contract.chosenDid}` : "",
    contract.secretsFetchedAt ? `secrets fetched ${contract.secretsFetchedAt}` : "",
    contract.message ?? "",
  ].filter(Boolean).join(" · ");
  li.append(head, detail);
  if (localStorage.getItem(computePrefs.transport(name)) === "webrtc" && vmshell.isActive(contract)) {
    li.append(webrtcShellAction(contract));
  }
  return li;
}

// "Connect" once the guest's publicKey, webrtc fingerprint and host keys are
// in the feed; it runs `ssh <contract>` in the main terminal.
function webrtcShellAction(contract) {
  const el = document.createElement("div");
  const ready = contract.phase === "Held" && webrtcGuest.guestFromState(lastFeedState, contract.name) !== null;
  if (!ready) {
    el.className = "detail";
    if (contract.phase === "Held") el.textContent = "Held; waiting for the guest's WebRTC publicKey and host keys…";
    return el;
  }
  const connect = document.createElement("button");
  connect.type = "button";
  connect.textContent = "Connect";
  connect.addEventListener("click", () => runInTerminal(`ssh ${contract.name}`));
  el.append(connect);
  return el;
}

/** Resolves with fn's first truthy result over the current and later feed snapshots. */
function watchFeed(fn) {
  return new Promise((resolve, reject) => {
    const watcher = (state) => {
      try {
        const value = fn(state);
        if (value) {
          feedWatchers.delete(watcher);
          resolve(value);
        }
      } catch (err) {
        feedWatchers.delete(watcher);
        reject(err);
      }
    };
    feedWatchers.add(watcher);
    if (lastFeedState) watcher(lastFeedState);
  });
}

async function kubectlCreate(manifest) {
  const result = await runKubectl(withServer(["create", "-f", "-"]), JSON.stringify(manifest));
  if (result.code !== 0) throw new Error(String(result.stderr || result.stdout || "create failed").trim());
}

// Runs a command line in the main terminal as if typed (the panel's Connect).
function runInTerminal(command) {
  term.focus();
  if (!ready || busy || stdinMode) {
    setAppStatus(computeStatusEl, `the terminal is busy; run: ${command}`, "warn");
    return;
  }
  replaceLine(command);
  submit();
}

// --- ssh -----------------------------------------------------------------------
// The shell's `ssh` (ssh.wasm in ssh_worker.js): hosts are ComputeContract
// names, ~/.ssh/config is seeded with "Host vmshell-* / ProxyCommand
// kcp-webrtc %h %p", and kcp-webrtc is this page dialing the guest over a
// WebRTC datachannel. Only public material is written to ~/.ssh (config,
// known_hosts from status.contract.hostKeys, <contract>.pub); the private
// key is the contract's non-extractable WebCrypto key in IndexedDB, and the
// shell filesystem (persisted to localStorage) never sees it.

const sshDir = `${homeDir}/.ssh`;
// While an interactive ssh runs it owns the main terminal's input.
let sshSession = null;
let testPeerKey = null;

function latin1Bytes(value) {
  if (!value) return null;
  if (typeof value !== "string") return value.length ? value : null;
  const bytes = new Uint8Array(value.length);
  for (let i = 0; i < value.length; i++) bytes[i] = value.charCodeAt(i) & 0xff;
  return bytes.length ? bytes : null;
}

async function sshResolve(host) {
  const target = sshCmd.resolveHost(lastFeedState, host, { testPeer: Boolean(sshTestPeer) });
  let key;
  let hostKeys;
  if (target.kind === "testpeer") {
    testPeerKey ??= await webrtcGuest.newSSHKey();
    key = testPeerKey;
    hostKeys = (await webrtcGuest.httpSignaling(sshTestPeer, key.authorized).guest()).hostKeys;
  } else {
    key = await webrtcGuest.loadSSHKey(host);
    if (!key) throw new Error("this browser holds no SSH key for the contract (it was requested elsewhere)");
    hostKeys = target.contract.hostKeys ?? [];
    if (hostKeys.length === 0) throw new Error("the guest has not reported its host keys yet");
  }
  const khPath = `${sshDir}/known_hosts`;
  let known = await bash.fs.readFile(khPath).catch(() => "");
  // The test peer makes a new host key per run; real guests never get
  // their lines rewritten.
  if (target.kind === "testpeer") known = sshCmd.dropKnownHost(known, host);
  const merged = sshCmd.mergeKnownHosts(known, host, hostKeys);
  await bash.fs.writeFile(khPath, merged);
  await bash.fs.writeFile(`${sshDir}/${host}.pub`, `${key.authorized}\n`);
  return { publicKey: key.raw, knownHosts: merged, key: key.privateKey };
}

async function sshDial(host, port, status, onEof) {
  if (port !== 22) throw new Error(`no webrtc tunnel for port ${port} (only ssh on 22)`);
  const target = sshCmd.resolveHost(lastFeedState, host, { testPeer: Boolean(sshTestPeer) });
  const signaling = target.kind === "testpeer"
    ? webrtcGuest.httpSignaling(sshTestPeer, testPeerKey.authorized)
    : webrtcGuest.contractEventSignaling({ contract: host, create: kubectlCreate, watch: watchFeed });
  return await webrtcGuest.dialGuest({ signaling, tag: "ssh", status, onEof });
}

async function runSsh(args, ctx) {
  await bash.fs.mkdir(sshDir, { recursive: true }).catch(() => {});
  let config = await bash.fs.readFile(`${sshDir}/config`).catch(() => null);
  if (config === null) {
    config = sshCmd.DEFAULT_SSH_CONFIG;
    await bash.fs.writeFile(`${sshDir}/config`, config);
  }
  // A terminal stdout (ssh last in its pipeline, not redirected) streams to
  // the terminal; otherwise output is buffered for the pipeline.
  const toTerminal = ttyTool === "ssh";
  const stdin = latin1Bytes(ctx.stdin);
  const interactive = toTerminal && !stdin;
  const decoders = { stdout: new TextDecoder(), stderr: new TextDecoder() };
  const buffered = { stdout: "", stderr: "" };
  const write = (stream, u8) => {
    if (toTerminal) term.write(u8);
    else buffered[stream] += decoders[stream].decode(u8, { stream: true });
  };
  const verboseErr = (text) => write("stderr", new TextEncoder().encode(`debug1: kcp-webrtc: ${text}\n`));
  const worker = new Worker(new URL("./ssh_worker.js", import.meta.url), { type: "module" });
  let dialed = null;
  return await new Promise((resolve) => {
    let raw = false;
    let done = false;
    const finish = (code) => {
      if (done) return;
      done = true;
      worker.terminate();
      dialed?.close();
      sshSession = null;
      if (toTerminal && raw) term.write("\r\n");
      resolve({ stdout: buffered.stdout, stderr: buffered.stderr, exitCode: code });
    };
    ctx.signal?.addEventListener("abort", () => finish(130));
    if (interactive) {
      sshSession = {
        // With a pty every key goes to the guest (Ctrl-C included); without
        // one, Ctrl-C ends ssh here and Ctrl-D closes its stdin.
        key(data) {
          if (!raw && data === "\u0003") return finish(130);
          if (!raw && data === "\u0004") return worker.postMessage({ type: "stdinEOF" });
          worker.postMessage({ type: "input", data: new TextEncoder().encode(data) });
        },
        resize: (cols, rows) => worker.postMessage({ type: "resize", cols, rows }),
      };
    }
    const reply = (id, value, transfer = []) => worker.postMessage({ type: "reply", id, ...value }, transfer);
    worker.onmessage = async (event) => {
      const m = event.data ?? {};
      if (m.type === "stdout" || m.type === "stderr") write(m.type, m.data);
      else if (m.type === "raw") raw = m.on;
      else if (m.type === "exit") finish(m.code);
      else if (m.type === "addKnownHost") {
        const path = `${sshDir}/known_hosts`;
        const text = await bash.fs.readFile(path).catch(() => "");
        await bash.fs.writeFile(path, `${text}${text && !text.endsWith("\n") ? "\n" : ""}${m.line}\n`);
      } else if (m.type === "resolve") {
        try {
          reply(m.id, await sshResolve(m.host));
        } catch (err) {
          reply(m.id, { error: String(err.message ?? err) });
        }
      } else if (m.type === "dial") {
        try {
          dialed = await sshDial(m.host, m.port, m.verbose ? verboseErr : () => {},
            () => worker.postMessage({ type: "eof" }));
          reply(m.id, { port: dialed.port }, [dialed.port]);
        } catch (err) {
          reply(m.id, { error: String(err.message ?? err) });
        }
      }
    };
    worker.postMessage({
      type: "run",
      execUrl: new URL("/wasm_exec.js", location.href).href,
      wasmUrl: new URL("/ssh.wasm", location.href).href,
      cfg: { argv: args, config, interactive, stdin, cols: term.cols, rows: term.rows },
    });
  });
}

function applyComputeState(state) {
  lastFeedState = state;
  for (const watcher of [...feedWatchers]) watcher(state);
  const contracts = state.contracts ?? [];
  const active = contracts.filter(vmshell.isActive);
  if (computeBusy && active.length > 0) computeBusy = false;
  computeReleasing = contracts.some(vmshell.isReleasing);
  const ready = state.marketIdentity?.phase === "Ready";
  computeRequestEl.disabled = computeBusy || !ready || active.length > 0;
  if (!computeBusy && !computeStatusEl.classList.contains("err")) {
    setAppStatus(computeStatusEl,
      !ready ? "sign in to request a VM"
        : active.length > 0 ? `release ${active.map((c) => c.name).join(", ")} to request another VM`
        : "ready to request a VM",
      !ready ? "warn" : "ok");
  }
  const render = JSON.stringify(contracts) + [...computeReleaseAsked].join() +
    JSON.stringify((state.contractEvents ?? []).map((e) => [e.name, e.phase]));
  if (render === computeLastRender) return;
  computeLastRender = render;
  computeListEl.replaceChildren(...contracts.map(computeRow));
}

function initComputeUI() {
  computeBiddersEl.value = localStorage.getItem(computePrefs.bidders) ?? vmshell.DEFAULT_BIDDERS.join(", ");
  // "0" is the old checkbox off; anything else that is not none is webrtc.
  const shell = localStorage.getItem(computePrefs.shell);
  computeShellEl.value = shell === "0" || shell === "none" ? "none" : "webrtc";
  computeRequestEl.addEventListener("click", () => {
    computeStatusEl.classList.remove("err");
    void requestVM();
  });
}

// The test peer needs no kcp, so it is wired before the page boots.
if (sshTestPeer) {
  computeTestPeerEl.hidden = false;
  computeTestPeerEl.addEventListener("click", () => runInTerminal(`ssh ${sshCmd.TEST_PEER_HOST}`));
}

// --- ingress relay -------------------------------------------------------------
// Ingress-mode Exposures (the requester's svc-requester-requester-ingress,
// which serves getSecrets to the guest) are reachable at
// https://<did label>.<relay>. Go owns the per-Exposure did:key; the page owns
// the com.fedproxy.temp.xrpc.subscribe WebSocket and forwards each request
// frame to socialwebIngressRequest, which serves only that host's routes.

const ingressSubs = new Map();
const ingressRegistered = new Set();
let ingressList = [];

function applyIngress(list) {
  ingressList = list;
  const hosts = new Set(list.map((entry) => entry.host));
  for (const [host, sub] of ingressSubs) {
    if (!hosts.has(host)) {
      sub.close();
      ingressSubs.delete(host);
      ingressRegistered.delete(host);
    }
  }
  for (const entry of list) {
    if (!ingressSubs.has(entry.host)) startIngress(entry);
  }
  renderIngress();
}

function renderIngress() {
  vmshellIngressEl.textContent = ingressList.map((entry) =>
    `${entry.name}: ${entry.url} (${ingressRegistered.has(entry.host) ? "registered" : "connecting"})`
  ).join("\n");
}

function startIngress(entry) {
  const pending = { closed: false, close() { this.closed = true; } };
  ingressSubs.set(entry.host, pending);
  const host = entry.host;
  registerIngress({
    proxyHost: entry.proxyHost,
    did: entry.did,
    getServiceAuthToken: async (nsid) =>
      (await socialwebCall("socialwebIngressToken", { host, nsid })).token,
    signer: async (nonce) =>
      decodeBase64((await socialwebCall("socialwebIngressSign", { host, nonce: encodeBase64(nonce) })).signature),
    handler: async (req) => {
      const res = await socialwebCall("socialwebIngressRequest", {
        host, method: req.method, path: req.path, params: req.params,
        headers: req.headers, body: req.body, rawBody: req.rawBody,
      }, 60_000);
      return { status: res.status, body: res.body, contentType: res.contentType || "application/json", headers: res.headers };
    },
    onRegistered: () => {
      ingressRegistered.add(host);
      addLog("ok", `ingress ${entry.name} registered at ${entry.url}`);
      renderIngress();
    },
    onClose: () => {
      ingressRegistered.delete(host);
      renderIngress();
    },
    onError: (err) => addLog("warn", `ingress ${entry.name}: ${err instanceof Error ? err.message : err}`),
  }).then((sub) => {
    if (pending.closed) sub.close();
    else ingressSubs.set(host, sub);
  }).catch((err) => {
    addLog("error", `ingress ${entry.name}: ${err instanceof Error ? err.message : err}`);
    ingressSubs.delete(host);
    // The first registration does not reconnect by itself; retry while listed.
    setTimeout(() => {
      const current = ingressList.find((e) => e.host === host);
      if (current && !ingressSubs.has(host)) startIngress(current);
    }, 10_000);
  });
}

// --- startup ---------------------------------------------------------------

async function start() {
  term.writeln(cyan("kcp-wasm") + dim(" loading real kcp.wasm…"));
  term.writeln(cyan("kubectl-wasm") + dim(" loading kubectl.wasm…"));
  setStatus("loading kcp + kubectl…", "running");
  // kcp boots for tens of seconds; download and compile kubectl meanwhile.
  // goat/yq follow kubectl at low priority, or load on first use.
  const kubectlLoaded = spawnKubectl();
  kubectlLoaded.then(ensureCli).catch(() => {});
  await Promise.all([spawnKcp(), kubectlLoaded]);

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
  setInterval(() => void poll(), Math.max(intervalMs, 5000));
  initSocialwebUI();
  void typeBootSteps(BOOT_STEPS);
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

term.onResize(({ cols, rows }) => sshSession?.resize(cols, rows));

term.onData((data) => {
  if (sshSession) {
    sshSession.key(data);
    return;
  }
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
    if (booting) return;
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
  if (sshSession) {
    const keys = { "ctrl-c": "\u0003", "ctrl-d": "\u0004", enter: "\r", tab: "\t", up: "\u001b[A" };
    if (keys[action]) sshSession.key(keys[action]);
    return;
  }
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
