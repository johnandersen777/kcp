// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// The page's link to the dev server's /debug hooks: the debug log sender
// (POST /debug/log) and the control stream client (GET /debug/control). Both
// outlive dev-server restarts and tunnel outages. fedproxy answers 404 "no
// route configured for host" while the tunnel is down, so a 404/405 means a
// host without the endpoint (a static deploy) only before the first success.

export const RETRY_MAX_MS = 30_000;

const noEndpoint = (status) => status === 404 || status === 405;

// Lines wait in a bounded buffer (oldest dropped and counted) while the server
// is unreachable and go out again with backoff (1 s doubling to 30 s). One
// request is in flight at a time. post(body) resolves to the HTTP status, 0 for
// a network error or timeout. onReconnect({downMs, dropped}) runs once after
// the first success that follows a failure or a drop.
export class DebugLogSender {
  constructor({
    post,
    encode,
    setTimer = (fn, ms) => setTimeout(fn, ms),
    now = () => Date.now(),
    max = 2000,
    batchLines = 400,
    batchBytes = 200_000,
    delayMs = 100,
    maxRetryMs = RETRY_MAX_MS,
    onDisabled = () => {},
    onReconnect = () => {},
  }) {
    Object.assign(this, { post, encode, setTimer, now, max, batchLines, batchBytes, delayMs, maxRetryMs, onDisabled, onReconnect });
    this.queue = [];
    this.dropped = 0;
    this.delivered = false;
    this.disabled = false;
    this.inFlight = false;
    this.retryMs = 0;
    this.downSince = 0;
    this.timer = null;
  }

  push(line) {
    if (this.disabled) return;
    this.queue.push(line);
    this.#trim();
    if (!this.inFlight) this.#schedule(this.retryMs || this.delayMs);
  }

  // Send now unless the server is known to be down (the retry timer does).
  flush() {
    if (this.retryMs > 0) return Promise.resolve();
    return this.#send();
  }

  // Everything still queued, encoded, for a last keepalive request on pagehide.
  takeAll() {
    if (this.disabled || this.queue.length === 0) return null;
    const body = this.encode(this.queue);
    this.queue = [];
    return body;
  }

  #trim() {
    const over = this.queue.length - this.max;
    if (over > 0) {
      this.queue.splice(0, over);
      this.dropped += over;
    }
  }

  #schedule(ms) {
    if (this.timer !== null) return;
    this.timer = this.setTimer(() => {
      this.timer = null;
      this.#send();
    }, ms);
  }

  async #send() {
    if (this.disabled || this.inFlight || this.queue.length === 0) return;
    let n = Math.min(this.queue.length, this.batchLines);
    let body = this.encode(this.queue.slice(0, n));
    while (body.length > this.batchBytes && n > 1) {
      n = Math.ceil(n / 2);
      body = this.encode(this.queue.slice(0, n));
    }
    const batch = this.queue.splice(0, n);
    this.inFlight = true;
    let status = 0;
    try {
      status = await this.post(body);
    } catch {
      status = 0;
    }
    this.inFlight = false;
    if (status >= 200 && status < 300) return this.#ok();
    if (noEndpoint(status) && !this.delivered) {
      this.disabled = true;
      this.queue = [];
      this.onDisabled(status);
      return;
    }
    if (status === 400 || status === 413) {
      // The server is up and refused this batch; resending cannot help.
      this.dropped += batch.length;
      return this.#ok();
    }
    this.queue.unshift(...batch);
    this.#trim();
    this.downSince ||= this.now();
    this.retryMs = Math.min(Math.max(this.retryMs * 2, 1000), this.maxRetryMs);
    this.#schedule(this.retryMs);
  }

  #ok() {
    this.delivered = true;
    this.retryMs = 0;
    if (this.downSince || this.dropped) {
      const report = { downMs: this.downSince ? this.now() - this.downSince : 0, dropped: this.dropped };
      this.downSince = 0;
      this.dropped = 0;
      this.onReconnect(report);
    }
    if (this.queue.length) this.#schedule(0);
  }
}

// The control stream: an EventSource reopened with backoff (2 s doubling to
// 30 s) after any error, after an open that takes longer than openTimeoutMs
// (a stalled tunnel holds the request), and, once the server has sent a "ping"
// event, after idleTimeoutMs without one (a stalled tunnel keeps a dead stream
// open). kick() reopens it now (the log sender saw the server come back).
// probe() resolves to the status of HEAD /debug/control; a 404/405 before the
// first open means the server has no control stream and stops the client.
export class ControlStream {
  constructor({
    url,
    open,
    probe = () => Promise.resolve(0),
    onCommand,
    log = () => {},
    setTimer = (fn, ms) => setTimeout(fn, ms),
    clearTimer = (id) => clearTimeout(id),
    now = () => Date.now(),
    retryMs = 2_000,
    maxRetryMs = RETRY_MAX_MS,
    openTimeoutMs = 30_000,
    idleTimeoutMs = 45_000,
  }) {
    Object.assign(this, { url, open, probe, onCommand, log, setTimer, clearTimer, now, maxRetryMs, openTimeoutMs, idleTimeoutMs });
    this.initialRetryMs = retryMs;
    this.retryMs = retryMs;
    this.source = null;
    this.watch = null;
    this.timer = null;
    this.everOpened = false;
    this.serverPings = false;
    this.downSince = 0;
    this.stopped = false;
  }

  start() {
    if (!this.stopped && !this.source && this.timer === null) this.#connect();
  }

  kick() {
    if (this.stopped) return;
    this.retryMs = this.initialRetryMs;
    if (this.timer !== null) {
      this.clearTimer(this.timer);
      this.timer = null;
      this.#connect();
    } else if (this.source) {
      this.#fail(true);
    }
  }

  #connect() {
    this.timer = null;
    const source = this.open(this.url);
    this.source = source;
    this.#arm(this.openTimeoutMs);
    source.onopen = () => {
      if (source !== this.source) return;
      const downMs = this.downSince ? this.now() - this.downSince : 0;
      this.retryMs = this.initialRetryMs;
      this.everOpened = true;
      this.downSince = 0;
      this.#arm(this.serverPings ? this.idleTimeoutMs : null);
      this.log(downMs ? `reconnected after ${Math.round(downMs / 1000)} s` : "connected");
    };
    source.onerror = () => {
      if (source === this.source) this.#fail(false);
    };
    source.onmessage = (event) => {
      if (source !== this.source) return;
      this.#alive();
      this.onCommand(event.data);
    };
    source.addEventListener?.("ping", () => {
      if (source !== this.source) return;
      this.serverPings = true;
      this.#alive();
    });
  }

  #alive() {
    this.#arm(this.serverPings ? this.idleTimeoutMs : null);
  }

  #arm(ms) {
    if (this.watch !== null) this.clearTimer(this.watch);
    this.watch = ms === null ? null : this.setTimer(() => {
      this.watch = null;
      if (this.source) this.#fail(false);
    }, ms);
  }

  async #fail(immediate) {
    const source = this.source;
    if (!source) return;
    this.source = null;
    source.close();
    this.#arm(null);
    this.downSince ||= this.now();
    if (!this.everOpened) {
      let status = 0;
      try {
        status = await this.probe();
      } catch {
        status = 0;
      }
      if (noEndpoint(status)) {
        this.stopped = true;
        return;
      }
    }
    if (this.stopped || this.source || this.timer !== null) return;
    const delay = immediate ? 0 : this.retryMs;
    if (!immediate) this.retryMs = Math.min(this.retryMs * 2, this.maxRetryMs);
    this.timer = this.setTimer(() => this.#connect(), delay);
  }
}
