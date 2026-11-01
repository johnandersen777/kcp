// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// One kcp per origin (and browser profile). Every tab of an origin shares one
// IndexedDB state and numbers its persists from the same stored base, so two
// running kcps collide on seqs and save over each other. A tab boots kcp only
// while it holds the Web Lock "kcp-state", for the page's lifetime (the
// browser releases it when the document unloads, d.reload included).
//
// Take over (linux-kcp): a second tab asks the holder over the
// BroadcastChannel "kcp-state". The holder acknowledges at once, flushes
// every pending persist, stops its kcp worker, releases the lock and says
// so; the new tab waits for the lock (not stealing) and only then boots. A
// holder that does not acknowledge within ackMs (crashed or frozen) leaves
// the force option: {steal: true}, and unsaved changes in it may be lost. A
// holder whose lock is stolen stops its kcp the same way when it runs again.

export const LOCK_NAME = "kcp-state";
export const CHANNEL_NAME = "kcp-state";
export const BUSY_MESSAGE = "kcp is running in another tab of this browser; close it or use that tab";
export const TAKEN_OVER_MESSAGE = "kcp was taken over by another tab of this browser";
export const FORCE_WARNING = "The other tab did not answer. Force take over? Unsaved changes in that tab may be lost.";

/**
 * @param {{locks?: object, openChannel?: (name: string) => object | null,
 *   onYield?: (why: "takeover"|"stolen") => Promise<void> | void,
 *   log?: (level: string, message: string) => void, id?: string,
 *   timers?: {setTimeout: Function, clearTimeout: Function}}} options
 *   onYield stops this tab's kcp (flush, terminate) before the lock goes.
 */
export function createKcpLock({
  locks = globalThis.navigator?.locks,
  openChannel = (name) => (typeof BroadcastChannel === "function" ? new BroadcastChannel(name) : null),
  onYield = () => {},
  log = () => {},
  id = Math.random().toString(36).slice(2, 10),
  timers = globalThis,
} = {}) {
  let state = locks ? "idle" : "unsupported";
  let release = null; // resolves the held lock's callback promise
  let channel = null;
  let onAck = null; // the take over in progress, waiting for the holder's ack

  function onMessage(event) {
    const msg = event?.data ?? {};
    if (msg.type === "takeover-request" && state === "held" && msg.from !== id) {
      channel?.postMessage({ type: "takeover-ack", to: msg.from, from: id });
      void yieldLock("takeover").then(() => channel?.postMessage({ type: "released", to: msg.from, from: id }));
      return;
    }
    if (msg.to === id && msg.type === "takeover-ack") onAck?.();
  }

  function ensureChannel() {
    if (channel) return channel;
    try {
      channel = openChannel(CHANNEL_NAME);
    } catch {
      channel = null;
    }
    if (channel) channel.onmessage = onMessage;
    return channel;
  }

  async function yieldLock(why) {
    if (state !== "held" && why !== "stolen") return;
    state = why === "stolen" ? "stolen" : "yielding";
    log("warn", why === "stolen" ? "kcp lock was taken by force by another tab; stopping kcp" : "another tab takes over kcp; flushing and stopping");
    try {
      await onYield(why);
    } catch (err) {
      log("warn", `stopping kcp for the other tab: ${err?.message ?? err}`);
    }
    const done = release;
    release = null;
    state = "taken-over";
    done?.();
  }

  // Requests the lock with opts; resolves true once held, false when
  // ifAvailable found it taken. The lock is held until yieldLock.
  function request(opts) {
    return new Promise((resolve, reject) => {
      let granted = false;
      const held = locks.request(LOCK_NAME, opts, (lock) => {
        if (!lock) {
          resolve(false);
          return undefined;
        }
        granted = true;
        state = "held";
        ensureChannel();
        resolve(true);
        return new Promise((done) => {
          release = done;
        });
      });
      held.catch((err) => {
        if (!granted) {
          reject(err);
          return;
        }
        // Stolen by a forced take over in another tab.
        if (err?.name === "AbortError" && state === "held") void yieldLock("stolen");
      });
    });
  }

  return {
    get state() {
      return state;
    },
    /** "held", "busy" (another tab holds it) or "unsupported" (no navigator.locks: boot as before). */
    async acquire() {
      if (!locks) return "unsupported";
      ensureChannel();
      if (await request({ ifAvailable: true })) return "held";
      state = "busy";
      return "busy";
    },
    /**
     * Asks the holder to hand over and waits for the lock: "held", or
     * "no-answer" when nothing acknowledged within ackMs, or "timeout" when
     * the holder acknowledged but did not release within releaseMs.
     */
    async takeover({ ackMs = 5000, releaseMs = 60_000 } = {}) {
      if (!locks) return "unsupported";
      const ch = ensureChannel();
      const controller = new AbortController();
      let acked = false;
      let timer = null;
      let timedOut = "";
      const arm = (ms, outcome) => {
        timers.clearTimeout(timer);
        timer = timers.setTimeout(() => {
          timedOut = outcome;
          controller.abort();
        }, ms);
      };
      onAck = () => {
        if (acked) return;
        acked = true;
        log("info", "the kcp tab answered; waiting for it to save and stop");
        arm(releaseMs, "timeout");
      };
      arm(ackMs, "no-answer");
      ch?.postMessage({ type: "takeover-request", from: id });
      try {
        await request({ signal: controller.signal });
        return "held";
      } catch (err) {
        if (err?.name === "AbortError" && timedOut) return timedOut;
        throw err;
      } finally {
        timers.clearTimeout(timer);
        onAck = null;
      }
    },
    /** Takes the lock from an unresponsive holder ({steal: true}). */
    async force() {
      if (!locks) return "unsupported";
      await request({ steal: true });
      return "held";
    },
    /** Drops the lock without a take over (tests). */
    release() {
      const done = release;
      release = null;
      if (state === "held") state = "idle";
      done?.();
    },
  };
}
