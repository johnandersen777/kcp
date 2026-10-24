// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import { createIngressManager, DROP_AFTER_MS } from "./ingress_manager.js";

function assertEquals(actual, expected) {
  const a = JSON.stringify(actual);
  const e = JSON.stringify(expected);
  if (a !== e) throw new Error(`expected:\n${e}\nactual:\n${a}`);
}

const flush = () => new Promise((resolve) => setTimeout(resolve, 0));

// harness wires a manager to a fake clock and a fake connect whose promise
// and subscriber the test settles by hand.
function harness() {
  let time = 0;
  let timers = [];
  const calls = [];
  const statuses = [];
  const logs = [];
  const manager = createIngressManager({
    now: () => time,
    setTimer: (fn, ms) => {
      const t = { at: time + ms, fn };
      timers.push(t);
      return t;
    },
    clearTimer: (t) => (timers = timers.filter((x) => x !== t)),
    onStatus: (entries) => statuses.push(entries),
    log: (level, message) => logs.push(`${level} ${message}`),
    connect: (entry, hooks) =>
      new Promise((resolve, reject) => {
        const sub = { connecting: true, closed: 0, close() { this.closed++; } };
        calls.push({ entry, hooks, sub, resolve: () => resolve(sub), reject });
      }),
  });
  const advance = async (ms) => {
    const end = time + ms;
    for (;;) {
      timers.sort((a, b) => a.at - b.at);
      const next = timers[0];
      if (!next || next.at > end) break;
      timers.shift();
      time = next.at;
      next.fn();
      await flush();
    }
    time = end;
  };
  const state = (host) => manager.status().find((e) => e.host === host)?.state;
  return { manager, calls, statuses, logs, advance, state, timers: () => timers };
}

const entry = (host) => ({ name: `exp-${host}`, host, url: `https://${host}`, did: `did:key:${host}`, proxyHost: "relay" });

Deno.test("single-flight: repeated lists start one subscriber per host", async () => {
  const h = harness();
  h.manager.apply([entry("a")]);
  h.manager.apply([entry("a")]);
  h.manager.apply([entry("a"), entry("a")]);
  assertEquals(h.calls.length, 1);
  h.calls[0].resolve();
  await flush();
  h.manager.apply([entry("a")]);
  assertEquals(h.calls.length, 1);
});

Deno.test("status transitions connecting -> registered -> reconnecting -> registered", async () => {
  const h = harness();
  h.manager.apply([entry("a")]);
  assertEquals(h.state("a"), "connecting");
  h.calls[0].resolve();
  await flush();
  h.calls[0].sub.connecting = false;
  h.calls[0].hooks.onRegistered({});
  assertEquals(h.state("a"), "registered");
  h.calls[0].hooks.onClose({ code: 1006, reason: "" });
  assertEquals(h.manager.status()[0], { name: "exp-a", host: "a", url: "https://a", state: "reconnecting", lastError: null, closeCode: 1006 });
  h.calls[0].hooks.onError(new Error("boom"));
  assertEquals(h.manager.status()[0].lastError, "boom");
  h.calls[0].hooks.onRegistered({});
  assertEquals(h.manager.status()[0], { name: "exp-a", host: "a", url: "https://a", state: "registered", lastError: null, closeCode: null });
  assertEquals(h.logs.filter((l) => l.startsWith("ok ")), ["ok ingress exp-a registered at https://a", "ok ingress exp-a registered at https://a"]);
  assertEquals(h.logs.includes("warn ingress exp-a closed: code 1006"), true);
  assertEquals(h.statuses.map((s) => s[0].state), ["connecting", "registered", "reconnecting", "reconnecting", "registered"]);
});

Deno.test("a host missing for less than the drop delay keeps its subscriber", async () => {
  const h = harness();
  h.manager.apply([entry("a")]);
  h.calls[0].resolve();
  await flush();
  h.calls[0].sub.connecting = false;
  h.calls[0].hooks.onRegistered({});
  h.manager.apply([]);
  await h.advance(DROP_AFTER_MS - 1);
  h.manager.apply([entry("a")]);
  await h.advance(DROP_AFTER_MS * 2);
  assertEquals(h.calls[0].sub.closed, 0);
  assertEquals(h.state("a"), "registered");
  assertEquals(h.calls.length, 1);
});

Deno.test("a host missing for the drop delay is closed, even without another list", async () => {
  const h = harness();
  h.manager.apply([entry("a"), entry("b")]);
  for (const c of h.calls) c.resolve();
  await flush();
  for (const c of h.calls) {
    c.sub.connecting = false;
    c.hooks.onRegistered({});
  }
  h.manager.apply([entry("b")]);
  await h.advance(DROP_AFTER_MS);
  assertEquals(h.calls[0].sub.closed, 1);
  assertEquals(h.calls[1].sub.closed, 0);
  assertEquals(h.statuses.at(-1).map((e) => [e.host, e.state]), [["a", "closed"], ["b", "registered"]]);
  assertEquals(h.manager.status().map((e) => e.host), ["b"]);
  // A stopped subscriber's own close event does not revive the entry.
  h.calls[0].hooks.onClose({ code: 1000, reason: "" });
  assertEquals(h.manager.status().map((e) => e.host), ["b"]);
  // Listed again: a fresh subscriber.
  h.manager.apply([entry("a"), entry("b")]);
  assertEquals(h.calls.length, 3);
});

Deno.test("a connecting socket is never closed by a list update", async () => {
  const h = harness();
  h.manager.apply([entry("a")]);
  h.manager.apply([]);
  // register() still in flight past the drop delay.
  await h.advance(DROP_AFTER_MS * 3);
  assertEquals(h.state("a"), "connecting");
  h.calls[0].resolve();
  await flush();
  // Socket created but still CONNECTING.
  await h.advance(DROP_AFTER_MS);
  assertEquals(h.calls[0].sub.closed, 0);
  h.calls[0].sub.connecting = false;
  h.calls[0].hooks.onRegistered({});
  await h.advance(1_000);
  assertEquals(h.calls[0].sub.closed, 1);
  assertEquals(h.manager.status(), []);
  assertEquals(h.calls.length, 1);
});

Deno.test("a failed first registration retries with backoff, one timer at a time", async () => {
  const h = harness();
  h.manager.apply([entry("a")]);
  h.calls[0].reject(new Error("nonce request failed: 502"));
  await flush();
  assertEquals(h.state("a"), "reconnecting");
  assertEquals(h.manager.status()[0].lastError, "nonce request failed: 502");
  h.manager.apply([entry("a")]);
  assertEquals(h.timers().length, 1);
  await h.advance(999);
  assertEquals(h.calls.length, 1);
  await h.advance(1);
  assertEquals(h.calls.length, 2);
  h.calls[1].reject(new Error("again"));
  await flush();
  await h.advance(1_999);
  assertEquals(h.calls.length, 2);
  await h.advance(1);
  assertEquals(h.calls.length, 3);
  h.calls[2].resolve();
  await flush();
  h.calls[2].sub.connecting = false;
  h.calls[2].hooks.onRegistered({});
  assertEquals(h.state("a"), "registered");
  assertEquals(h.timers().length, 0);
});

Deno.test("retry backoff caps at 30s and a dropped host stops retrying", async () => {
  const h = harness();
  h.manager.apply([entry("a")]);
  for (let i = 0; i < 7; i++) {
    h.calls.at(-1).reject(new Error("down"));
    await flush();
    await h.advance(Math.min(1_000 * 2 ** i, 30_000));
    assertEquals(h.calls.length, i + 2);
  }
  h.calls.at(-1).reject(new Error("down"));
  await flush();
  h.manager.apply([]);
  await h.advance(DROP_AFTER_MS);
  assertEquals(h.manager.status(), []);
  await h.advance(60_000);
  assertEquals(h.calls.length, 8);
});
