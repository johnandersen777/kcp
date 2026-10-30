// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import { createKcpLock, LOCK_NAME } from "./kcp_lock.js";

function assertEquals(actual, expected, label = "value") {
  const a = JSON.stringify(actual);
  const e = JSON.stringify(expected);
  if (a !== e) throw new Error(`${label}: expected ${e}, got ${a}`);
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const abort = (message) => new DOMException(message, "AbortError");

// One origin's navigator.locks, shared by the fake tabs: exclusive, FIFO,
// ifAvailable, steal and signal as in the Web Locks API.
class FakeLocks {
  holder = null;
  queue = [];
  requests = [];
  request(name, opts, cb) {
    this.requests.push({ name, opts: { ...opts, signal: opts.signal ? "signal" : undefined } });
    return new Promise((resolve, reject) => {
      const entry = { cb, resolve, reject, stolen: false };
      if (opts.steal) {
        const old = this.holder;
        if (old) {
          old.stolen = true;
          old.reject(abort("stolen"));
        }
        this.grant(entry);
        return;
      }
      if (!this.holder) return this.grant(entry);
      if (opts.ifAvailable) {
        Promise.resolve(cb(null)).then(resolve, reject);
        return;
      }
      this.queue.push(entry);
      opts.signal?.addEventListener("abort", () => {
        const i = this.queue.indexOf(entry);
        if (i < 0) return;
        this.queue.splice(i, 1);
        reject(abort("aborted"));
      });
    });
  }
  grant(entry) {
    this.holder = entry;
    Promise.resolve(entry.cb({ name: LOCK_NAME, mode: "exclusive" })).then((value) => {
      if (entry.stolen) return;
      entry.resolve(value);
      if (this.holder === entry) {
        this.holder = null;
        const next = this.queue.shift();
        if (next) this.grant(next);
      }
    });
  }
}

// BroadcastChannel between the fake tabs: delivers to every other open
// channel, asynchronously.
function channelHub() {
  const open = new Set();
  return () => {
    const ch = {
      onmessage: null,
      postMessage(data) {
        for (const other of open) if (other !== ch) setTimeout(() => other.onmessage?.({ data: structuredClone(data) }), 0);
      },
    };
    open.add(ch);
    return ch;
  };
}

function tab(name, locks, openChannel, events, onYield = async () => {}) {
  return createKcpLock({
    locks,
    openChannel,
    id: name,
    onYield: async (why) => {
      events.push(`${name}: yield ${why}`);
      await onYield(why);
      events.push(`${name}: stopped`);
    },
  });
}

Deno.test("kcp lock: without navigator.locks a tab boots as before", async () => {
  const lock = createKcpLock({ locks: null, openChannel: () => null });
  assertEquals(lock.state, "unsupported");
  assertEquals(await lock.acquire(), "unsupported");
});

Deno.test("kcp lock: a second tab finds kcp busy; the lock frees on unload", async () => {
  const locks = new FakeLocks();
  const open = channelHub();
  const events = [];
  const a = tab("a", locks, open, events);
  const b = tab("b", locks, open, events);
  assertEquals(await a.acquire(), "held", "first tab");
  assertEquals(await b.acquire(), "busy", "second tab");
  assertEquals(locks.requests[0], { name: "kcp-state", opts: { ifAvailable: true } }, "ifAvailable request");
  a.release(); // the document unloads (d.reload)
  await sleep(0);
  assertEquals(await b.acquire(), "held", "after the first tab reloaded away");
  assertEquals(events, [], "no take over happened");
});

Deno.test("kcp lock: Take over flushes and stops the holder before the new tab holds the lock", async () => {
  const locks = new FakeLocks();
  const open = channelHub();
  const events = [];
  const a = tab("a", locks, open, events, async () => {
    await sleep(20); // flushAll, kcp flush, terminate
    events.push("a: flushed");
  });
  const b = tab("b", locks, open, events);
  assertEquals(await a.acquire(), "held");
  assertEquals(await b.acquire(), "busy");
  const outcome = await b.takeover({ ackMs: 1000, releaseMs: 1000 });
  events.push("b: held");
  assertEquals(outcome, "held");
  assertEquals(events, ["a: yield takeover", "a: flushed", "a: stopped", "b: held"]);
  assertEquals([a.state, b.state], ["taken-over", "held"]);
  // Waits for the release; never steals.
  const waited = locks.requests.find((r) => r.opts.signal);
  assertEquals(waited.opts.steal, undefined, "takeover does not steal");
});

Deno.test("kcp lock: a frozen holder gets no-answer, then force steals and the holder stops when it runs", async () => {
  const locks = new FakeLocks();
  const open = channelHub();
  const events = [];
  const a = tab("a", locks, () => null, events); // frozen: never hears the request
  const b = tab("b", locks, open, events);
  assertEquals(await a.acquire(), "held");
  assertEquals(await b.acquire(), "busy");
  assertEquals(await b.takeover({ ackMs: 30, releaseMs: 1000 }), "no-answer");
  assertEquals(b.state, "busy", "nothing taken yet");
  assertEquals(await b.force(), "held");
  assertEquals(locks.requests.at(-1).opts, { steal: true }, "force steals");
  await sleep(0);
  assertEquals(events, ["a: yield stolen", "a: stopped"]);
  assertEquals([a.state, b.state], ["taken-over", "held"]);
});

Deno.test("kcp lock: a holder that answers but does not release in time times out", async () => {
  const locks = new FakeLocks();
  const open = channelHub();
  const events = [];
  let finish;
  const a = tab("a", locks, open, events, () => new Promise((resolve) => (finish = resolve)));
  const b = tab("b", locks, open, events);
  await a.acquire();
  assertEquals(await b.takeover({ ackMs: 1000, releaseMs: 40 }), "timeout");
  finish();
  await sleep(0);
  // The holder still released in the end; a retry gets the lock.
  assertEquals(await b.acquire(), "held");
});

Deno.test("page wiring: the lock is taken before kcp boots; persists stop once taken over", async () => {
  const app = await Deno.readTextFile(new URL("./app.js", import.meta.url));
  const start = app.slice(app.indexOf("async function start()"));
  assert(start.indexOf("await acquireKcpLock()") >= 0 && start.indexOf("await acquireKcpLock()") < start.indexOf("spawnKcp("), "lock before spawnKcp");
  assert(/data\.type === "persist"\) \{\s*if \(!kcpStateOwned\(\)\) return;/.test(app), "kcp persists gated");
  assert(/lock: \(\) => kcpLock\.state/.test(app), "d.lock()");
  const yieldFn = app.slice(app.indexOf("async function yieldKcp"), app.indexOf("async function acquireKcpLock"));
  assert(yieldFn.indexOf("flushKcpWorker(") < yieldFn.indexOf("terminate()"), "flush before terminate");
  assert(yieldFn.indexOf("persister.flushAll()") < yieldFn.indexOf("persister.stop()"), "flush before stop");
  assert(/why === "takeover"/.test(yieldFn), "a stolen tab does not write");
});

Deno.test("kcp lock: a holder stuck handing over stops owning the state once forced", async () => {
  const locks = new FakeLocks();
  const open = channelHub();
  const events = [];
  let finish;
  const a = tab("a", locks, open, events, () => new Promise((resolve) => (finish = resolve)));
  const b = tab("b", locks, open, events);
  await a.acquire();
  assertEquals(await b.takeover({ ackMs: 1000, releaseMs: 40 }), "timeout");
  assertEquals(a.state, "yielding", "a acked and is flushing");
  assertEquals(await b.force(), "held");
  await sleep(0);
  assertEquals(a.state, "stolen", "a stops owning the state before its flush returns");
  finish();
  await sleep(0);
  assertEquals([a.state, b.state], ["taken-over", "held"]);
});

function assert(condition, message) {
  if (!condition) throw new Error(message);
}
