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

// Snapshot and restore a paused Go js/wasm instance (wasm_exec.js Go class).
//
// While no Go code runs (any JS turn outside a Go callback), the whole Go
// program is linear memory + the module's mutable globals + wasm_exec's JS
// tables (_values, _goRefCounts, _ids, _idPool, scheduled timeouts). The
// module must be rewritten by scripts/wasm_globals.js so the globals are
// exported.
//
// JS values Go references are recreated by provenance: a property path from
// globalThis, a func wrapper id from _makeFuncWrapper, or a primitive. A Go
// goroutine waiting on a JS Promise (net/http fetch, body reads) cannot be
// carried over, so busy() reports those and callers capture only when idle.

export const SNAPSHOT_FORMAT = 2;

const funcIds = new WeakMap();
const GO_ROOTS = 7; // NaN, 0, null, true, false, globalThis, go

// Go runs only inside run() and _resume(); a Promise.then() call made while
// it runs is Go awaiting that promise (syscall/js has no other way to).
let goDepth = 0;
let goAwaits = 0;
let thenPatched = false;

function patchThen() {
  if (thenPatched) return;
  thenPatched = true;
  const then = Promise.prototype.then;
  const settled = () => goAwaits--;
  Promise.prototype.then = function (onFulfilled, onRejected) {
    if (goDepth > 0 && this instanceof Promise) {
      goAwaits++;
      then.call(this, settled, settled);
    }
    return then.call(this, onFulfilled, onRejected);
  };
}

// instrument must run right after `new Go()`, before run() or restore().
export function instrument(go) {
  patchThen();
  const make = go._makeFuncWrapper.bind(go);
  go._makeFuncWrapper = (id) => {
    const fn = make(id);
    funcIds.set(fn, id);
    return fn;
  };
  const resume = go._resume.bind(go);
  go._resume = () => {
    goDepth++;
    try {
      resume();
    } finally {
      goDepth--;
    }
  };
  const run = go.run.bind(go);
  go.run = (instance) => {
    goDepth++;
    try {
      return run(instance);
    } finally {
      goDepth--;
    }
  };
  // The monotonic clock continues from the captured nanotime on restore, so
  // Go's timers keep their relative deadlines; only the wall clock jumps.
  go._snapBaseMs = Date.now() - performance.now();
  go.importObject.gojs["runtime.nanotime1"] = (sp) => {
    sp >>>= 0;
    const v = (go._snapBaseMs + performance.now()) * 1000000;
    go.mem.setUint32(sp + 8, v, true);
    go.mem.setUint32(sp + 12, Math.floor(v / 4294967296), true);
  };
  return go;
}

// busy returns why the instance cannot be captured right now, or "".
export function busy(go) {
  if (go.exited) return "Go program has exited";
  if (goDepth > 0 || go._pendingEvent) return "inside a Go callback";
  if (goAwaits > 0) return `Go awaits ${goAwaits} promise(s)`;
  if (!Object.keys(go._inst?.exports ?? {}).some((n) => n.startsWith("__snap_g"))) {
    return "wasm globals are not exported (scripts/wasm_globals.js)";
  }
  return "";
}

function globalPaths() {
  // Shortest property path from globalThis for every object/function reachable
  // in two hops. ponytail: depth 2 covers what Go's syscall packages hold
  // (fs, fs.constants, process, crypto, constructors); deepen if refs go unknown.
  const paths = new Map();
  const queue = [[globalThis, []]];
  for (let i = 0; i < queue.length; i++) {
    const [obj, path] = queue[i];
    if (path.length >= 2) continue;
    let names = [];
    try {
      names = Object.getOwnPropertyNames(obj);
    } catch {
      continue;
    }
    for (const name of names) {
      let v;
      try {
        const d = Object.getOwnPropertyDescriptor(obj, name);
        if (!d || !("value" in d)) {
          if (path.length > 0) continue;
          v = obj[name];
        } else v = d.value;
      } catch {
        continue;
      }
      if ((typeof v !== "object" && typeof v !== "function") || v === null) continue;
      if (paths.has(v) || v === globalThis) continue;
      const p = [...path, name];
      paths.set(v, p);
      queue.push([v, p]);
    }
  }
  return paths;
}

function resolvePath(path) {
  let v = globalThis;
  for (const name of path) v = v?.[name];
  return v;
}

function b64(bytes) {
  let s = "";
  for (let i = 0; i < bytes.length; i += 0x8000) {
    s += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000));
  }
  return btoa(s);
}

function unb64(s) {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

// capture returns {meta, memory}. memory is a view of the LIVE linear memory:
// write or copy it before Go runs again (before the next await).
export function capture(go, extra = {}) {
  const reason = busy(go);
  if (reason) throw new Error(`cannot capture: ${reason}`);
  const t0 = performance.now();
  const exp = go._inst.exports;
  const globals = {};
  for (const [name, g] of Object.entries(exp)) {
    if (!name.startsWith("__snap_g")) continue;
    globals[name] = typeof g.value === "bigint" ? { i64: g.value.toString() } : { i32: g.value };
  }

  const paths = globalPaths();
  const values = [];
  const kinds = {};
  const unknown = [];
  for (let id = GO_ROOTS; id < go._values.length; id++) {
    const v = go._values[id];
    const rc = go._goRefCounts[id];
    let e;
    if (v === null && !(rc > 0)) e = { k: "free" };
    else if (typeof v === "string" || typeof v === "number" || typeof v === "boolean") e = { k: "prim", v };
    else if (typeof v === "bigint") e = { k: "bigint", v: v.toString() };
    else if (funcIds.has(v)) e = { k: "func", id: funcIds.get(v) };
    else if (paths.has(v)) e = { k: "path", p: paths.get(v) };
    else if (v instanceof Uint8Array) e = { k: "u8", v: b64(v) };
    else {
      e = { k: "unknown", t: typeof v, c: v?.constructor?.name ?? String(v) };
      unknown.push({ id, rc, t: e.t, c: e.c, keys: v ? Object.keys(v).slice(0, 8) : [] });
    }
    e.rc = rc;
    kinds[e.k] = (kinds[e.k] ?? 0) + 1;
    values.push(e);
  }
  // Func wrappers JS still holds (globalThis.kcpHandleRequest, ...) even if Go
  // dropped its own js.Value for them.
  const globalFuncs = [];
  for (const [v, p] of paths) if (funcIds.has(v)) globalFuncs.push({ p, id: funcIds.get(v) });

  const memory = new Uint8Array(exp.mem.buffer);
  const meta = {
    format: SNAPSHOT_FORMAT,
    nowMs: go._snapBaseMs + performance.now(),
    wallMs: Date.now(),
    bytes: memory.byteLength,
    globals,
    values,
    idPool: go._idPool.slice(),
    timeouts: [...go._scheduledTimeouts.keys()],
    nextTimeoutID: go._nextCallbackTimeoutID,
    globalFuncs,
    stats: { kinds, unknown, captureMs: 0 },
    extra,
  };
  meta.stats.captureMs = Math.round(performance.now() - t0);
  return { meta, memory };
}

function armTimeout(go, id, delay) {
  go._scheduledTimeouts.set(id, setTimeout(() => {
    go._resume();
    while (go._scheduledTimeouts.has(id)) {
      console.warn("scheduleTimeoutEvent: missed timeout event");
      go._resume();
    }
  }, delay));
}

// restore turns an instrumented, never-run Go into the captured program. The
// module must be the same rewritten module. fill(view) writes the captured
// linear memory into view (the new instance's memory, already grown). Go
// stays paused until the returned resume() is called: define every JS global
// it calls first.
export async function restore(go, module, meta, fill) {
  if (meta.format !== SNAPSHOT_FORMAT) throw new Error(`snapshot format ${meta.format}`);
  const t = { start: performance.now() };
  go._snapBaseMs = meta.nowMs - performance.now();
  const instance = await WebAssembly.instantiate(module, go.importObject);
  t.instantiate = performance.now();
  const mem = instance.exports.mem;
  const pages = meta.bytes / 65536;
  const have = mem.buffer.byteLength / 65536;
  if (pages > have) mem.grow(pages - have);
  await fill(new Uint8Array(mem.buffer, 0, meta.bytes));
  t.fill = performance.now();
  for (const [name, g] of Object.entries(meta.globals)) {
    if (!instance.exports[name]) throw new Error(`module has no ${name}: not the captured module`);
    instance.exports[name].value = g.i64 !== undefined ? BigInt(g.i64) : g.i32;
  }

  go._inst = instance;
  go.mem = new DataView(mem.buffer);
  go._values = [NaN, 0, null, true, false, globalThis, go];
  go._goRefCounts = new Array(GO_ROOTS).fill(Infinity);
  go._ids = new Map([[0, 1], [null, 2], [true, 3], [false, 4], [globalThis, 5], [go, 6]]);
  const missing = [];
  meta.values.forEach((e, i) => {
    const id = GO_ROOTS + i;
    let v;
    if (e.k === "free") v = null;
    else if (e.k === "prim") v = e.v;
    else if (e.k === "bigint") v = BigInt(e.v);
    else if (e.k === "func") v = go._makeFuncWrapper(e.id);
    else if (e.k === "u8") v = unb64(e.v);
    else if (e.k === "path") {
      v = resolvePath(e.p);
      if (v === undefined) missing.push(e.p.join("."));
    } else v = e.t === "function" ? function unrestorable() {} : { unrestorable: e.c };
    go._values[id] = v;
    go._goRefCounts[id] = e.rc ?? 0;
    if (v !== null && !go._ids.has(v)) go._ids.set(v, id);
  });
  go._idPool = meta.idPool.slice();
  go.exited = false;
  go._pendingEvent = null;
  go._exitPromise = new Promise((resolve) => {
    go._resolveExitPromise = resolve;
  });
  for (const { p, id } of meta.globalFuncs) {
    const owner = resolvePath(p.slice(0, -1));
    const ref = go._values.find((v, i) => i >= GO_ROOTS && funcIds.get(v) === id) ?? go._makeFuncWrapper(id);
    if (owner) owner[p[p.length - 1]] = ref;
  }
  go._nextCallbackTimeoutID = meta.nextTimeoutID;
  go._scheduledTimeouts = new Map();
  t.end = performance.now();
  return {
    instance,
    missing,
    // Re-arms every captured idle timeout at 0ms: an early wake is harmless,
    // Go re-schedules the next one from its own timer heap.
    resume() {
      for (const id of meta.timeouts) armTimeout(go, id, 0);
      if (meta.timeouts.length === 0) setTimeout(() => go._resume(), 0);
    },
    timings: {
      instantiateMs: Math.round(t.instantiate - t.start),
      fillMs: Math.round(t.fill - t.instantiate),
      tablesMs: Math.round(t.end - t.fill),
    },
  };
}
