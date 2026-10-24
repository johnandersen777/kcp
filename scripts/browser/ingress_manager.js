// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// ingress_manager.js keeps one relay subscriber (ingress_ws.js register) per
// ingress host, level-triggered on the Exposure list Go pushes. kcp_worker.js
// owns it; the page only renders the status entries it emits.
//
// - Single-flight: a host has at most one subscriber, connecting or open.
// - A host missing from the list is closed only after dropAfterMs, and never
//   while its socket is still CONNECTING (the relay logs "closed before the
//   connection is established" and the bidder sees "no active subscriber").
// - A failed first registration retries with ingress_ws.js's backoff (1 s
//   doubling to 30 s, reset on success). Later drops are retried by
//   ingress_ws.js itself, so a host never has two reconnect timers.
//
// connect(entry, hooks) returns a promise of a subscriber ({close(),
// connecting}); hooks are ingress_ws.js's onRegistered/onClose/onError.

export const DROP_AFTER_MS = 15_000;
const RETRY_BASE_MS = 1_000;
const RETRY_MAX_MS = 30_000;
const RECHECK_MS = 1_000;

const errorMessage = (err) => (err instanceof Error ? err.message : String(err));

export function createIngressManager({
  connect,
  onStatus = () => {},
  log = () => {},
  now = Date.now,
  setTimer = setTimeout,
  clearTimer = clearTimeout,
  dropAfterMs = DROP_AFTER_MS,
}) {
  const subs = new Map();
  let lastStatus = "";

  function status() {
    return [...subs.values()].map(({ entry, state, lastError, closeCode }) => ({
      name: entry.name,
      host: entry.host,
      url: entry.url,
      state,
      lastError,
      closeCode,
    }));
  }

  function emit() {
    const entries = status();
    const text = JSON.stringify(entries);
    if (text === lastStatus) return;
    lastStatus = text;
    onStatus(entries);
  }

  function setState(rec, state) {
    if (rec.stopped) return;
    rec.state = state;
    emit();
  }

  function apply(list) {
    const wanted = new Map((list ?? []).filter((e) => e?.host).map((e) => [e.host, e]));
    for (const [host, entry] of wanted) {
      const rec = subs.get(host);
      if (!rec) {
        start(entry);
        continue;
      }
      rec.entry = entry;
      rec.missingSince = null;
    }
    const t = now();
    for (const [host, rec] of subs) {
      if (wanted.has(host) || rec.missingSince !== null) continue;
      rec.missingSince = t;
      arm(rec, dropAfterMs);
    }
    emit();
  }

  function start(entry) {
    const rec = {
      entry,
      state: "connecting",
      lastError: null,
      closeCode: null,
      sub: null,
      pending: false,
      stopped: false,
      missingSince: null,
      dropTimer: null,
      retryTimer: null,
      attempt: 0,
    };
    subs.set(entry.host, rec);
    open(rec);
  }

  function open(rec) {
    rec.retryTimer = null;
    if (rec.stopped || rec.pending || rec.sub) return;
    rec.pending = true;
    let promise;
    try {
      promise = Promise.resolve(connect(rec.entry, hooks(rec)));
    } catch (err) {
      promise = Promise.reject(err);
    }
    promise.then((sub) => {
      rec.pending = false;
      rec.sub = sub;
    }, (err) => {
      rec.pending = false;
      rec.lastError = errorMessage(err);
      log("error", `ingress ${rec.entry.name}: ${rec.lastError}`);
      const delay = Math.min(RETRY_BASE_MS * 2 ** rec.attempt, RETRY_MAX_MS);
      rec.attempt += 1;
      rec.retryTimer = setTimer(() => open(rec), delay);
      setState(rec, "reconnecting");
    });
  }

  function hooks(rec) {
    return {
      onRegistered: () => {
        rec.attempt = 0;
        rec.lastError = null;
        rec.closeCode = null;
        log("ok", `ingress ${rec.entry.name} registered at ${rec.entry.url}`);
        setState(rec, "registered");
      },
      onClose: ({ code, reason }) => {
        log("warn", `ingress ${rec.entry.name} closed: code ${code}${reason ? ` (${reason})` : ""}`);
        rec.closeCode = code;
        setState(rec, "reconnecting");
      },
      onError: (err) => {
        rec.lastError = errorMessage(err);
        log("warn", `ingress ${rec.entry.name}: ${rec.lastError}`);
        if (!rec.stopped) emit();
      },
    };
  }

  // A register still in flight or a CONNECTING socket is left alone; closing
  // either one closes a socket the relay is still upgrading.
  const connecting = (rec) => rec.pending || rec.sub?.connecting === true;

  function arm(rec, ms) {
    if (rec.dropTimer === null) rec.dropTimer = setTimer(() => check(rec), ms);
  }

  function check(rec) {
    rec.dropTimer = null;
    if (rec.stopped || rec.missingSince === null) return;
    const left = rec.missingSince + dropAfterMs - now();
    if (left > 0) return arm(rec, left);
    if (connecting(rec)) return arm(rec, RECHECK_MS);
    log("info", `ingress ${rec.entry.host} is gone from the Exposure list; closing it`);
    stop(rec);
  }

  function stop(rec) {
    if (rec.retryTimer !== null) clearTimer(rec.retryTimer);
    if (rec.dropTimer !== null) clearTimer(rec.dropTimer);
    rec.retryTimer = rec.dropTimer = null;
    rec.state = "closed";
    emit();
    rec.stopped = true;
    subs.delete(rec.entry.host);
    rec.sub?.close();
  }

  return { apply, status };
}
