// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import { ControlStream, DebugLogSender } from "./debug_link.js";

function assert(cond, message) {
  if (!cond) throw new Error(message);
}
const eq = (a, b, what) => assert(JSON.stringify(a) === JSON.stringify(b), `${what}: ${JSON.stringify(a)} != ${JSON.stringify(b)}`);

// A manual clock: timers run only when the test advances it.
function clock() {
  let t = 0;
  let seq = 0;
  const timers = new Map();
  const c = {
    now: () => t,
    setTimer: (fn, ms) => {
      const id = ++seq;
      timers.set(id, { at: t + ms, fn });
      return id;
    },
    clearTimer: (id) => timers.delete(id),
    async advance(ms) {
      const end = t + ms;
      for (;;) {
        const next = [...timers.entries()].filter(([, v]) => v.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
        if (!next) break;
        timers.delete(next[0]);
        t = next[1].at;
        next[1].fn();
        for (let i = 0; i < 5; i++) await Promise.resolve();
      }
      t = end;
      for (let i = 0; i < 5; i++) await Promise.resolve();
    },
  };
  return c;
}

function sender(c, statuses, opts = {}) {
  const posts = [];
  const events = [];
  const s = new DebugLogSender({
    post: (body) => {
      posts.push(JSON.parse(body));
      return Promise.resolve(typeof statuses === "function" ? statuses() : statuses.shift() ?? 204);
    },
    encode: (lines) => JSON.stringify(lines),
    setTimer: c.setTimer,
    now: c.now,
    onDisabled: (status) => events.push(["disabled", status]),
    onReconnect: (r) => events.push(["reconnect", r]),
    ...opts,
  });
  return { s, posts, events };
}

Deno.test("lines go out in one batch shortly after the first", async () => {
  const c = clock();
  const { s, posts } = sender(c, []);
  s.push("a");
  s.push("b");
  await c.advance(100);
  eq(posts, [["a", "b"]], "posts");
});

Deno.test("a 404 before any delivery turns the log off (static host)", async () => {
  const c = clock();
  const { s, posts, events } = sender(c, [404]);
  s.push("a");
  await c.advance(100);
  s.push("b");
  await c.advance(60_000);
  eq(posts.length, 1, "posts");
  eq(events, [["disabled", 404]], "events");
});

Deno.test("a 404 after a delivery (fedproxy: no route) is an outage, retried with backoff", async () => {
  const c = clock();
  let down = false;
  const { s, posts, events } = sender(c, () => (down ? 404 : 204));
  s.push("first");
  await c.advance(100);
  down = true;
  s.push("during-1");
  await c.advance(100);
  s.push("during-2");
  // Retries at 1, 2, 4, 8, 16, 30, 30 s after the first failure: none at 100 ms.
  await c.advance(500);
  eq(posts.length, 2, "no retry before the backoff");
  await c.advance(60_000);
  const retries = posts.length - 2;
  assert(retries >= 5 && retries <= 7, `retries ${retries}`);
  down = false;
  await c.advance(30_000);
  const last = posts.at(-1);
  eq(last, ["during-1", "during-2"], "kept lines resent in order");
  eq(events.length, 1, "one reconnect");
  eq(events[0][0], "reconnect", "event");
  eq(events[0][1].dropped, 0, "dropped");
  assert(events[0][1].downMs >= 60_000, `downMs ${events[0][1].downMs}`);
});

Deno.test("the backoff tops out at 30 s", async () => {
  const c = clock();
  const { s, posts } = sender(c, () => (posts.length === 1 ? 204 : 0));
  s.push("x");
  await c.advance(100);
  s.push("y");
  await c.advance(100);
  await c.advance(10 * 60_000);
  const n = posts.length;
  await c.advance(30_000);
  eq(posts.length, n + 1, "one retry per 30 s");
});

Deno.test("the buffer keeps the newest lines and counts the drops", async () => {
  const c = clock();
  let down = true;
  const { s, posts, events } = sender(c, () => (down ? 0 : 204), { max: 5 });
  for (let i = 0; i < 12; i++) s.push(i);
  await c.advance(100);
  down = false;
  await c.advance(30_000);
  eq(posts.at(-1), [7, 8, 9, 10, 11], "newest kept");
  eq(events[0][1].dropped, 7, "dropped");
});

Deno.test("a refused batch (400/413) is dropped, not retried", async () => {
  const c = clock();
  const { s, posts, events } = sender(c, [204, 413, 204]);
  s.push("a");
  await c.advance(100);
  s.push("huge");
  await c.advance(100);
  s.push("b");
  await c.advance(100);
  eq(posts, [["a"], ["huge"], ["b"]], "posts");
  eq(events, [["reconnect", { downMs: 0, dropped: 1 }]], "events");
});

Deno.test("large batches are split under the byte limit", async () => {
  const c = clock();
  const { s, posts } = sender(c, [], { batchBytes: 30 });
  for (let i = 0; i < 8; i++) s.push("xxxx");
  await c.advance(1000);
  assert(posts.length > 1, `posts ${posts.length}`);
  eq(posts.flat().length, 8, "all lines");
});

class FakeSource {
  static all = [];
  constructor(url) {
    this.url = url;
    this.closed = false;
    this.listeners = {};
    FakeSource.all.push(this);
  }
  addEventListener(type, fn) {
    this.listeners[type] = fn;
  }
  close() {
    this.closed = true;
  }
  ping() {
    this.listeners.ping?.();
  }
}

function stream(c, opts = {}) {
  FakeSource.all = [];
  const logs = [];
  const commands = [];
  const cs = new ControlStream({
    url: "/debug/control?session=s1",
    open: (url) => new FakeSource(url),
    onCommand: (d) => commands.push(d),
    log: (m) => logs.push(m),
    setTimer: c.setTimer,
    clearTimer: c.clearTimer,
    now: c.now,
    ...opts,
  });
  return { cs, logs, commands };
}
const live = () => FakeSource.all.filter((s) => !s.closed);

Deno.test("control: errors reopen with backoff up to 30 s, and commands run after", async () => {
  const c = clock();
  const { cs, logs, commands } = stream(c);
  cs.start();
  FakeSource.all[0].onopen();
  eq(logs, ["connected"], "logs");
  // The dev server goes away: every attempt fails.
  FakeSource.all.at(-1).onerror();
  for (let i = 0; i < 8; i++) {
    await c.advance(30_000);
    FakeSource.all.at(-1).onerror();
  }
  const before = FakeSource.all.length;
  await c.advance(29_000);
  eq(FakeSource.all.length, before, "no attempt before 30 s");
  await c.advance(1_000);
  eq(FakeSource.all.length, before + 1, "attempt at 30 s");
  const s = FakeSource.all.at(-1);
  s.onopen();
  assert(/^reconnected after \d+ s$/.test(logs.at(-1)), logs.at(-1));
  s.onmessage({ data: '{"id":"1","js":"return d.state()"}' });
  eq(commands, ['{"id":"1","js":"return d.state()"}'], "commands");
  eq(live().length, 1, "one open source");
});

Deno.test("control: a stalled open is abandoned and retried", async () => {
  const c = clock();
  const { cs } = stream(c);
  cs.start();
  await c.advance(30_000);
  assert(FakeSource.all[0].closed, "stalled source closed");
  await c.advance(2_000);
  eq(FakeSource.all.length, 2, "retried");
});

Deno.test("control: a silent stream is reopened once the server pings", async () => {
  const c = clock();
  const { cs } = stream(c);
  cs.start();
  const first = FakeSource.all[0];
  first.onopen();
  first.ping();
  await c.advance(40_000);
  first.ping();
  await c.advance(40_000);
  assert(!first.closed, "pings keep it");
  await c.advance(10_000);
  assert(first.closed, "no ping for 45 s closes it");
  await c.advance(2_000);
  eq(FakeSource.all.length, 2, "reopened");
});

Deno.test("control: without server pings a quiet stream is kept", async () => {
  const c = clock();
  const { cs } = stream(c);
  cs.start();
  FakeSource.all[0].onopen();
  await c.advance(10 * 60_000);
  eq(live().length, 1, "kept");
  eq(FakeSource.all.length, 1, "no reconnect");
});

Deno.test("control: kick reopens now", async () => {
  const c = clock();
  const { cs } = stream(c);
  cs.start();
  FakeSource.all[0].onopen();
  cs.kick();
  await c.advance(0);
  eq(FakeSource.all.length, 2, "reopened");
  eq(live().length, 1, "old one closed");
});

Deno.test("control: 404 before the first open stops; 404 after an open retries", async () => {
  const c = clock();
  const { cs } = stream(c, { probe: () => Promise.resolve(404) });
  cs.start();
  FakeSource.all[0].onerror();
  await c.advance(120_000);
  eq(FakeSource.all.length, 1, "stopped");

  const c2 = clock();
  const { cs: cs2 } = stream(c2, { probe: () => Promise.resolve(404) });
  cs2.start();
  FakeSource.all[0].onopen();
  FakeSource.all[0].onerror();
  await c2.advance(2_000);
  eq(FakeSource.all.length, 2, "retried after fedproxy 404");
});
