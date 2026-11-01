// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// A gate for "kcp is ready" (rc130 boot race). With a snapshot restore the
// status feed reports MarketIdentity Ready before the page has the kcp worker,
// so everything that talks to kcp waits here instead of failing with "kcp
// worker is not running" and switching features off for the session.

/**
 * @returns {{wait(timeoutMs?: number): Promise<unknown>, open(value: unknown): void,
 *   fail(err: Error): void, readonly state: "waiting"|"open"|"failed"}}
 *   open() lets every waiter and later wait() through; fail() rejects them
 *   (kcp stopped: taken over by another tab); open() again recovers.
 */
export function createReadyGate({ timers = globalThis } = {}) {
  let state = "waiting";
  let value;
  let error = null;
  let waiters = [];
  const settle = () => {
    const list = waiters;
    waiters = [];
    for (const w of list) {
      timers.clearTimeout(w.timer);
      if (state === "open") w.resolve(value);
      else w.reject(error);
    }
  };
  return {
    get state() {
      return state;
    },
    wait(timeoutMs = 180_000) {
      if (state === "open") return Promise.resolve(value);
      if (state === "failed") return Promise.reject(error);
      return new Promise((resolve, reject) => {
        const w = { resolve, reject, timer: null };
        w.timer = timers.setTimeout(() => {
          waiters = waiters.filter((x) => x !== w);
          reject(new Error(`kcp is still starting (waited ${Math.round(timeoutMs / 1000)} s)`));
        }, timeoutMs);
        waiters.push(w);
      });
    },
    open(v) {
      state = "open";
      value = v;
      settle();
    },
    fail(err) {
      state = "failed";
      error = err;
      settle();
    },
  };
}
