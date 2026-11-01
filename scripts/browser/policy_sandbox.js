// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// policy_sandbox.js is the page half of the fenced policy sandbox (plan 0037),
// the browser counterpart of socialweb's deno runner under the policy flags.
//
// - startFrame() (page main thread): one hidden <iframe sandbox="allow-scripts">
//   per tab. Its srcdoc has an opaque origin: no page storage, DOM or
//   channels, and fetch carries Origin: null without credentials.
// - connectFrame(frame) gives a MessagePort the kcp worker receives in init.
// - The frame script (serve/runProgram) starts one module Worker per run from
//   a data: bootstrap that imports the program as its own module, and terminates it on its answer, at the
//   deadline or on kill: one Worker per Run, as deno is one process per Run.
//   The frame only relays; program code never runs on the page's thread.
// - connect(port) (kcp worker) is __socialwebSandbox {run, kill, probe} for
//   socialweb-wasm/internal/browsersandbox.
//
// The bootstrap (the Worker's module) gives a program what deno gives policy code and nothing of the
// page: Deno.env (Program.Env), exit, stdout/stderr.writeSync, inspect,
// read/writeTextFile that fail like deno without --allow-read, console.log/
// info/debug on stdout and warn/error on stderr (64 KiB each), uncaught
// errors exit 1. "node:" imports of the bundled actions resolve to small data:
// shims (Buffer, crypto.webcrypto/randomBytes, dns that always fails so the
// atproto handle resolver falls back to HTTPS). fetch is the Worker's own.

export const OUTPUT_LIMIT = 64 << 10;
const PROBE_DB = "kcp-fence-probe";
const PROBE_CACHE_URL = "https://kcp-fence-probe.invalid/marker";

// --- runs inside the sandbox Worker (serialized into each program) ------------

function bootstrap(env, cap, program) {
  const out = { stdout: "", stderr: "" };
  let done = false;
  const decoder = new TextDecoder();
  const write = (stream, text) => {
    if (out[stream].length < cap) out[stream] = (out[stream] + text).slice(0, cap);
  };
  const exit = (code = 0) => {
    if (done) return;
    done = true;
    postMessage({ exitCode: Number(code) | 0, stdout: out.stdout, stderr: out.stderr });
  };
  const inspect = (value) => {
    if (value instanceof Error) return value.stack || String(value);
    try {
      const text = JSON.stringify(value);
      return text === undefined ? String(value) : text;
    } catch {
      return String(value);
    }
  };
  const line = (args) => args.map((v) => typeof v === "string" ? v : inspect(v)).join(" ") + "\n";
  for (const m of ["log", "info", "debug"]) console[m] = (...args) => write("stdout", line(args));
  for (const m of ["warn", "error", "trace"]) console[m] = (...args) => write("stderr", line(args));
  class NotCapable extends Error {
    name = "NotCapable";
  }
  const denied = (what, path) => new NotCapable(`Requires ${what} access to "${path}", run again with the --allow-${what} flag`);
  const stream = (name) => ({
    writeSync: (bytes) => {
      write(name, typeof bytes === "string" ? bytes : decoder.decode(bytes));
      return bytes.length;
    },
    write: (bytes) => Promise.resolve(stream(name).writeSync(bytes)),
  });
  const deno = {
    env: {
      get: (key) => env[key],
      has: (key) => Object.prototype.hasOwnProperty.call(env, key),
      set: (key, value) => { env[key] = String(value); },
      delete: (key) => { delete env[key]; },
      toObject: () => ({ ...env }),
    },
    exit,
    stdout: stream("stdout"),
    stderr: stream("stderr"),
    inspect,
    errors: { NotCapable },
    readTextFile: (path) => Promise.reject(denied("read", path)),
    readTextFileSync: (path) => { throw denied("read", path); },
    writeTextFile: (path) => Promise.reject(denied("write", path)),
    writeTextFileSync: (path) => { throw denied("write", path); },
  };
  Object.defineProperty(globalThis, "Deno", { value: deno, writable: true, configurable: true });
  // Uncaught errors exit 1 unless the program handled them (the action shim
  // calls preventDefault and exits itself); checked after all listeners ran.
  const uncaught = (prefix, error, event) => {
    if (done || event.defaultPrevented) return;
    write("stderr", `error: ${prefix}${inspect(error)}\n`);
    exit(1);
  };
  addEventListener("unhandledrejection", (e) => setTimeout(() => uncaught("Uncaught (in promise) ", e.reason, e)));
  addEventListener("error", (e) => setTimeout(() => uncaught("Uncaught ", e.error ?? e.message, e)));
  // The program is its own module, imported from here so its evaluation
  // (incl. top-level await) is observed: done exits 0, as deno does at the
  // end of the main module; a throw is dispatched as an "unhandledrejection"
  // event, as deno does for its main module.
  const evaluated = import(URL.createObjectURL(new Blob([program], { type: "text/javascript" })));
  evaluated.then(() => exit(0), (reason) => {
    const event = Object.assign(new Event("unhandledrejection", { cancelable: true }), { reason, promise: evaluated });
    dispatchEvent(event);
    uncaught("Uncaught (in promise) ", reason, event);
  });
}

function nodeShim(name) {
  const shims = {
    buffer: `export class Buffer extends Uint8Array {
  static from(value, a, b) {
    if (typeof value === "string") {
      if (a && !/^utf-?8$/i.test(a)) throw new Error("Buffer.from: only utf8 in the browser policy sandbox");
      return new Buffer(new TextEncoder().encode(value));
    }
    if (value instanceof ArrayBuffer) return new Buffer(value, a ?? 0, b ?? value.byteLength - (a ?? 0));
    return new Buffer(value);
  }
  static alloc(n) { return new Buffer(n); }
  static allocUnsafe(n) { return new Buffer(n); }
  static isBuffer(value) { return value instanceof Buffer; }
  static concat(list, total) {
    total ??= list.reduce((n, b) => n + b.length, 0);
    const out = new Buffer(total);
    let at = 0;
    for (const b of list) {
      if (at >= total) break;
      out.set(b.subarray(0, total - at), at);
      at += b.length;
    }
    return out;
  }
  toString(encoding = "utf8") {
    if (!/^utf-?8$/i.test(encoding)) throw new Error("Buffer.toString: only utf8 in the browser policy sandbox");
    return new TextDecoder().decode(this);
  }
}
export default { Buffer };`,
    crypto: `export const webcrypto = globalThis.crypto;
export const randomBytes = (n) => globalThis.crypto.getRandomValues(new Uint8Array(n));
export const randomUUID = () => globalThis.crypto.randomUUID();
export function createHash(algorithm) {
  throw new Error("node:crypto createHash(" + algorithm + ") is not available in the browser policy sandbox");
}
export default { webcrypto, randomBytes, randomUUID, createHash };`,
    "dns/promises": `const fail = async () => {
  const err = new Error("DNS is not available in the browser policy sandbox");
  err.code = "ENOTFOUND";
  throw err;
};
export const resolveTxt = fail;
export const lookup = fail;
export class Resolver { setServers() {} resolveTxt() { return fail(); } }
export default { resolveTxt, lookup, Resolver };`,
  };
  const code = shims[name] ?? `throw new Error(${JSON.stringify(`node:${name} is not available in the browser policy sandbox`)});`;
  return "data:text/javascript," + encodeURIComponent(code);
}

// ponytail: a regex, so a "node:" specifier spelled inside a string literal
// is rewritten too; a parser if a policy ever needs that text verbatim.
function rewriteNodeImports(source) {
  return source.replace(/(\bfrom\s*|\bimport\s*\(?\s*)(["'])node:([\w/]+)\2/g,
    (_m, head, _q, name) => head + JSON.stringify(nodeShim(name)));
}

function programModule(source, env, cap) {
  return `(${bootstrap})(${JSON.stringify(env ?? {})}, ${cap}, ${JSON.stringify(rewriteNodeImports(source))});\n`;
}

// --- runs in the frame (serialized into its srcdoc) and in tests -------------

function runProgram(request, done, cap) {
  let finished = false;
  let worker = null;
  let timer = null;
  // data:, not blob: Chromium fails every module Worker from a blob:null URL
  // made in an opaque-origin frame (a classic one loads); data: loads in
  // Chrome and iOS Safari (plan 0037 step 0).
  const url = "data:text/javascript," + encodeURIComponent(programModule(String(request.source ?? ""), request.env, cap));
  const finish = (result) => {
    if (finished) return;
    finished = true;
    clearTimeout(timer);
    worker?.terminate();
    done(result);
  };
  try {
    worker = new Worker(url, { type: "module" });
  } catch (err) {
    finish({ exitCode: 1, stdout: "", stderr: `error: ${err}\n`, timedOut: false });
    return { kill() {} };
  }
  worker.onmessage = ({ data }) =>
    finish({ exitCode: Number(data?.exitCode) | 0, stdout: String(data?.stdout ?? ""), stderr: String(data?.stderr ?? ""), timedOut: false });
  // A module that fails to load or throws past the bootstrap; the Worker's own
  // report (with its captured output) wins if it arrives within 100 ms.
  worker.onerror = (event) => {
    event.preventDefault();
    const message = event.message || "the program failed to load";
    setTimeout(() => finish({ exitCode: 1, stdout: "", stderr: `error: ${message}\n`, timedOut: false }), 100);
  };
  const killed = () => finish({ exitCode: -1, stdout: "", stderr: "", timedOut: true });
  timer = setTimeout(killed, request.timeoutMs > 0 ? request.timeoutMs : 120_000);
  return { kill: killed };
}

function serve(port, cap) {
  const runs = new Map();
  port.onmessage = ({ data }) => {
    if (data?.kill !== undefined) return runs.get(data.kill)?.kill();
    if (data?.frame) {
      let parentDocument = "blocked";
      try {
        if (parent.document) parentDocument = "READABLE";
      } catch {
        // SecurityError: the opaque origin cannot reach the page's DOM
      }
      return port.postMessage({ id: data.id, origin: self.origin, parentDocument });
    }
    const id = data?.id;
    runs.set(id, runProgram(data, (result) => {
      runs.delete(id);
      port.postMessage({ id, ...result });
    }, cap));
  };
}

function relay(cap) {
  addEventListener("message", (event) => {
    if (event.source === parent && event.data?.type === "policy-sandbox-port" && event.ports[0]) serve(event.ports[0], cap);
  });
  parent.postMessage({ type: "policy-sandbox-ready" }, "*");
}

// Defense in depth (the page has no CSP): scripts only inline (the relay),
// data: (the per-run Worker, node shims) and blob: (the program, imported by
// the Worker from a blob it creates); fetch only https.
export const FRAME_CSP = "default-src 'none'; script-src 'unsafe-inline' blob: data:; worker-src data:; connect-src https:";

export function frameSrcdoc(cap = OUTPUT_LIMIT) {
  const code = [bootstrap, nodeShim, rewriteNodeImports, programModule, runProgram, serve, relay].join("\n");
  return `<!doctype html><meta http-equiv="Content-Security-Policy" content="${FRAME_CSP}"><script>${code}\nrelay(${cap});<\/script>`;
}

// For tests: run one program in a module Worker as the frame does.
export function runInWorker(request, cap = OUTPUT_LIMIT) {
  let handle;
  const result = new Promise((resolve) => {
    handle = runProgram(request, resolve, cap);
  });
  return Object.assign(result, { kill: () => handle.kill() });
}

export { programModule, rewriteNodeImports };

// --- page main thread ----------------------------------------------------------

// startFrame appends the hidden sandbox iframe and resolves once its relay is
// listening.
export function startFrame(doc = document) {
  return new Promise((resolve, reject) => {
    const frame = doc.createElement("iframe");
    const timer = setTimeout(() => {
      removeEventListener("message", onMessage);
      reject(new Error("policy sandbox frame did not start"));
    }, 10_000);
    const onMessage = (event) => {
      if (event.source !== frame.contentWindow || event.data?.type !== "policy-sandbox-ready") return;
      clearTimeout(timer);
      removeEventListener("message", onMessage);
      resolve(frame);
    };
    addEventListener("message", onMessage);
    frame.setAttribute("sandbox", "allow-scripts");
    frame.setAttribute("aria-hidden", "true");
    frame.hidden = true;
    frame.srcdoc = frameSrcdoc();
    doc.body.appendChild(frame);
  });
}

// connectFrame returns a port for one kcp worker; the frame serves every port
// it is given, so it outlives kcp worker restarts.
export function connectFrame(frame) {
  const channel = new MessageChannel();
  frame.contentWindow.postMessage({ type: "policy-sandbox-port" }, "*", [channel.port2]);
  return channel.port1;
}

// --- kcp worker ------------------------------------------------------------------

// connect is __socialwebSandbox: run(requestJSON) and probe() resolve to JSON
// strings for Go; kill(id) ends a run early.
export function connect(port, { timeoutMs = 20_000 } = {}) {
  const pending = new Map();
  let next = 0;
  port.onmessage = ({ data }) => {
    pending.get(data?.id)?.(data);
    pending.delete(data?.id);
  };
  const call = (message, wait) => new Promise((resolve, reject) => {
    pending.set(message.id, resolve);
    port.postMessage(message);
    if (wait) {
      setTimeout(() => {
        if (pending.delete(message.id)) reject(new Error(`the policy sandbox frame did not answer in ${wait} ms`));
      }, wait);
    }
  });
  const run = (request) => call({ ...request, id: `probe-${++next}` }, (request.timeoutMs ?? 0) + timeoutMs);
  return {
    run: (json) => {
      const { id, source, env, timeoutMs: ms } = JSON.parse(json);
      return call({ id, source, env, timeoutMs: ms }).then(({ exitCode, stdout, stderr, timedOut }) =>
        JSON.stringify({ exitCode, stdout, stderr, timedOut }));
    },
    kill: (id) => port.postMessage({ kill: id }),
    probe: () => probe({ run, frame: () => call({ id: `probe-${++next}`, frame: true }, timeoutMs) }).then((r) => JSON.stringify(r)),
  };
}

// probe (kcp worker, page origin) plants markers in the page's IndexedDB,
// Cache Storage and OPFS and listens on a BroadcastChannel, then runs a probe
// program in the sandbox. Fenced only if the sandbox sees none of them, has
// an opaque origin and no DOM, the frame cannot read the page's document, a
// program that never ends is killed at its deadline and the next run works.
export async function probe({ run, frame }) {
  const marker = `m${crypto.getRandomValues(new Uint32Array(2)).join("")}`;
  const facts = {};
  const pageNames = { idb: [], opfs: [] };
  let heard = false;
  const channel = new BroadcastChannel(PROBE_DB);
  channel.onmessage = () => { heard = true; };
  try {
    facts.idbPlanted = await plantIDB(marker).then(() => "yes", (e) => `no:${e?.name ?? "error"}`);
    pageNames.idb = (await globalThis.indexedDB?.databases?.())?.map((d) => d.name) ?? [];
    facts.cachePlanted = await plantCache(marker);
    pageNames.opfs = await plantOPFS();
    const inner = await run({ source: probeProgram(marker, pageNames), env: {}, timeoutMs: 15_000 });
    let seen = {};
    try {
      seen = JSON.parse(inner.stdout.trim().split("\n").pop());
    } catch {
      facts.program = `exit=${inner.exitCode}${inner.timedOut ? " timedOut" : ""} ${inner.stderr.slice(0, 200)}`.trim();
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
    Object.assign(facts, seen);
    facts.bc = heard ? "HEARD" : "silent";
    const f = await frame();
    facts.frameOrigin = f.origin;
    facts.parent = f.parentDocument;
    const endless = await run({ source: "for (;;) {}", env: {}, timeoutMs: 1_000 });
    facts.timeout = endless.timedOut ? "killed" : `exit=${endless.exitCode}`;
    const again = await run({ source: 'console.log("ok");', env: {}, timeoutMs: 10_000 });
    facts.rerun = again.exitCode === 0 && again.stdout === "ok\n" ? "ok" : `exit=${again.exitCode}`;
  } catch (err) {
    facts.error = String(err?.message ?? err);
  } finally {
    channel.close();
    await cleanup();
  }
  const fenced = !facts.error && !facts.program && facts.origin === "null" && facts.frameOrigin === "null" &&
    facts.dom === "none" && facts.shim === "ok" &&
    !["idb", "cache", "opfs"].some((k) => String(facts[k]).startsWith("VISIBLE")) &&
    facts.bc === "silent" && facts.parent === "blocked" && facts.timeout === "killed" && facts.rerun === "ok";
  const message = Object.entries(facts).map(([k, v]) => `${k}=${v}`).join(" ");
  return { fenced, message };
}

function probeProgram(marker, pageNames) {
  return `import { Buffer } from "node:buffer";
const marker = ${JSON.stringify(marker)};
const pageNames = ${JSON.stringify(pageNames)};
const out = { origin: String(self.origin), shim: Buffer.from("ok").toString() };
out.dom = [typeof document, typeof parent, typeof top].every((t) => t === "undefined") ? "none" : "PRESENT";
const err = (e) => "denied:" + (e?.name ?? "error");
try {
  const names = (await indexedDB.databases?.())?.map((d) => d.name) ?? [];
  const value = await new Promise((resolve, reject) => {
    const r = indexedDB.open(${JSON.stringify(PROBE_DB)});
    r.onsuccess = () => {
      const db = r.result;
      if (!db.objectStoreNames.contains("s")) return resolve(undefined);
      const g = db.transaction("s").objectStore("s").get("k");
      g.onsuccess = () => resolve(g.result);
      g.onerror = () => reject(g.error);
    };
    r.onerror = () => reject(r.error);
  });
  const listed = names.filter((n) => pageNames.idb.includes(n) && n !== ${JSON.stringify(PROBE_DB)});
  out.idb = value === marker ? "VISIBLE" : listed.length ? "VISIBLE:" + listed.join(",") : "empty";
} catch (e) { out.idb = err(e); }
try {
  out.cache = typeof caches === "undefined" ? "none"
    : (await (await caches.open(${JSON.stringify(PROBE_DB)})).match(${JSON.stringify(PROBE_CACHE_URL)})) ? "VISIBLE" : "empty";
} catch (e) { out.cache = err(e); }
try {
  const root = await navigator.storage.getDirectory();
  const names = [];
  for await (const name of root.keys()) names.push(name);
  const seen = names.filter((n) => n === ${JSON.stringify(PROBE_DB)} || pageNames.opfs.includes(n));
  out.opfs = seen.length ? "VISIBLE:" + seen.join(",") : "empty";
} catch (e) { out.opfs = err(e); }
try { new BroadcastChannel(${JSON.stringify(PROBE_DB)}).postMessage(marker); } catch {}
console.log(JSON.stringify(out));
`;
}

async function plantIDB(marker) {
  await new Promise((resolve, reject) => {
    const r = indexedDB.open(PROBE_DB, 1);
    r.onupgradeneeded = () => r.result.createObjectStore("s");
    r.onsuccess = () => {
      const tx = r.result.transaction("s", "readwrite");
      tx.objectStore("s").put(marker, "k");
      tx.oncomplete = () => {
        r.result.close();
        resolve();
      };
      tx.onerror = () => reject(tx.error);
    };
    r.onerror = () => reject(r.error);
  });
}

async function plantCache(marker) {
  try {
    await (await caches.open(PROBE_DB)).put(PROBE_CACHE_URL, new Response(marker));
    return "yes";
  } catch (e) {
    return `no:${e?.name ?? "error"}`;
  }
}

// Returns the page's OPFS root names, including the planted marker file.
async function plantOPFS() {
  try {
    const root = await navigator.storage.getDirectory();
    await root.getFileHandle(PROBE_DB, { create: true });
    const names = [];
    for await (const name of root.keys()) names.push(name);
    return names;
  } catch {
    return [];
  }
}

async function cleanup() {
  await Promise.allSettled([
    new Promise((resolve) => {
      const r = globalThis.indexedDB?.deleteDatabase(PROBE_DB);
      if (!r) return resolve();
      r.onsuccess = r.onerror = r.onblocked = resolve;
    }),
    globalThis.caches?.delete(PROBE_DB),
    navigator.storage?.getDirectory?.().then((root) => root.removeEntry(PROBE_DB)),
  ]);
}
