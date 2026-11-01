// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import { assertEquals, assertRejects } from "jsr:@std/assert@1";
import { retryingFetch } from "./fetch_retry.js";

// flaky fails the first n calls with err, then answers ok.
function flaky(n, err = new TypeError("Load failed")) {
  const calls = [];
  const fn = (input, init) => {
    calls.push({ input, init });
    if (calls.length <= n) return Promise.reject(err);
    return Promise.resolve(new Response("ok"));
  };
  return { fn, calls };
}

Deno.test("a GET dropped once is retried once and answers", async () => {
  const { fn, calls } = flaky(1);
  const retried = [];
  const f = retryingFetch(fn, { delayMs: 1, onRetry: (url) => retried.push(url) });
  const res = await f("https://pds.example/xrpc/com.atproto.repo.getRecord");
  assertEquals(await res.text(), "ok");
  assertEquals(calls.length, 2);
  assertEquals(retried, ["https://pds.example/xrpc/com.atproto.repo.getRecord"]);
});

Deno.test("a GET dropped twice fails after one retry", async () => {
  const { fn, calls } = flaky(2);
  const f = retryingFetch(fn, { delayMs: 1 });
  await assertRejects(() => f("https://pds.example/a"), TypeError, "Load failed");
  assertEquals(calls.length, 2);
});

Deno.test("a dropped POST is not retried", async () => {
  const { fn, calls } = flaky(1);
  const f = retryingFetch(fn, { delayMs: 1 });
  await assertRejects(() => f("https://pds.example/a", { method: "post", body: "x" }), TypeError);
  assertEquals(calls.length, 1);
});

Deno.test("HEAD and a GET Request object are retried", async () => {
  for (const [input, init] of [["https://pds.example/a", { method: "HEAD" }], [new Request("https://pds.example/b"), undefined]]) {
    const { fn, calls } = flaky(1);
    await retryingFetch(fn, { delayMs: 1 })(input, init);
    assertEquals(calls.length, 2);
  }
});

Deno.test("an aborted request is not retried", async () => {
  const ctl = new AbortController();
  const { fn, calls } = flaky(1);
  ctl.abort();
  await assertRejects(() => retryingFetch(fn, { delayMs: 1 })("https://pds.example/a", { signal: ctl.signal }), TypeError);
  assertEquals(calls.length, 1);
});

Deno.test("an abort during the retry delay stops the retry", async () => {
  const ctl = new AbortController();
  const { fn, calls } = flaky(1);
  const p = retryingFetch(fn, { delayMs: 50 })("https://pds.example/a", { signal: ctl.signal });
  setTimeout(() => ctl.abort(new DOMException("stop", "AbortError")), 5);
  await assertRejects(() => p, DOMException, "stop");
  assertEquals(calls.length, 1);
});

Deno.test("an error that is not a network TypeError passes through", async () => {
  const { fn, calls } = flaky(1, new DOMException("aborted", "AbortError"));
  await assertRejects(() => retryingFetch(fn, { delayMs: 1 })("https://pds.example/a"), DOMException);
  assertEquals(calls.length, 1);
});

Deno.test("an HTTP error response is returned, not retried", async () => {
  const calls = [];
  const fn = () => (calls.push(1), Promise.resolve(new Response("no", { status: 503 })));
  const res = await retryingFetch(fn, { delayMs: 1 })("https://pds.example/a");
  assertEquals(res.status, 503);
  assertEquals(calls.length, 1);
});
