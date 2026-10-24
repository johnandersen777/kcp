// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// Browser side of the fedproxy ingress subscribe protocol
// (com.fedproxy.temp.xrpc.subscribe). Registration mirrors
// socialweb-computer-kcp/internal/atproto/ingress/client.go: fetch a nonce over
// XRPC, sign the nonce bytes, then open the subscriber WebSocket with the
// registration, DID and service-auth token in the query.
//
// The JSON frame helpers are pure so tests can drive the protocol without a
// live relay. WebSocket and fetch are injectable for the same reason.
//
// Plain JavaScript so the page loads it without a build step (ported from
// the former ingress_ws.ts).

var SUBSCRIBE_NSID = "com.fedproxy.temp.xrpc.subscribe";
var GET_NONCE_NSID = "com.fedproxy.temp.xrpc.getRegistrationNonce";
var REGISTRATION_TYPE = "com.fedproxy.temp.xrpc.registration";
var KEEPALIVE_PATH = "/xrpc/com.publicdomainrelay.temp.keepalive.ping";
function frame(kind, fields = {}) {
  return JSON.stringify({
    $type: `${SUBSCRIBE_NSID}#${kind}`,
    ...fields
  });
}
function parseFrame(text) {
  let value;
  try {
    value = JSON.parse(text);
  } catch {
    return null;
  }
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    return null;
  }
  const record = value;
  const type = record.$type;
  if (typeof type !== "string" || !type.startsWith(`${SUBSCRIBE_NSID}#`)) {
    return null;
  }
  return {
    ...record,
    $type: type,
    kind: type.slice(SUBSCRIBE_NSID.length + 1)
  };
}
function encodeBase64(bytes) {
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
}
function decodeBase64(text) {
  const binary = atob(text);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return bytes;
}
function httpBase(proxyHost) {
  return origin(proxyHost, "http", "https");
}
function wsBase(proxyHost) {
  return origin(proxyHost, "ws", "wss");
}
function origin(host, insecure, secure) {
  const trimmed = host.replace(/\/+$/, "");
  const match = /^([a-z][a-z0-9+.-]*):\/\//i.exec(trimmed);
  if (match) {
    const scheme = match[1].toLowerCase();
    const rest = trimmed.slice(match[0].length);
    if (scheme === insecure || scheme === secure) return trimmed;
    if (scheme === "http" || scheme === "https") {
      return `${scheme === "http" ? insecure : secure}://${rest}`;
    }
    if (scheme === "ws" || scheme === "wss") {
      return `${scheme === "ws" ? insecure : secure}://${rest}`;
    }
    return trimmed;
  }
  const local = trimmed === "localhost" || trimmed.startsWith("127.") || trimmed.startsWith("[::1]") || /:\d+$/.test(trimmed);
  return `${local ? insecure : secure}://${trimmed}`;
}
function listen(ws, type, listener) {
  if (typeof ws.addEventListener === "function") {
    ws.addEventListener(type, listener);
    return;
  }
  switch (type) {
    case "message":
      ws.onmessage = listener;
      break;
    case "close":
      ws.onclose = listener;
      break;
    case "error":
      ws.onerror = listener;
      break;
    case "open":
      ws.onopen = listener;
      break;
  }
}
function stringMap(value) {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    return {};
  }
  const out = {};
  for (const [key, item] of Object.entries(value)) {
    if (typeof item === "string") out[key] = item;
  }
  return out;
}
function responseBody(body) {
  if (body === void 0 || body === null) return "";
  if (typeof body === "string") {
    try {
      return JSON.parse(body);
    } catch {
      return body;
    }
  }
  if (body instanceof Uint8Array) return new TextDecoder().decode(body);
  return body;
}
function errorMessage(error) {
  return error instanceof Error ? error.message : String(error);
}
var SubscriberConnection = class {
  options;
  fetchImpl;
  wsFactory;
  ready;
  resolveReady;
  rejectReady;
  readySettled;
  readyTimer;
  registeredOnce;
  ws;
  closed;
  attempt;
  reconnectTimer;
  constructor(options, fetchImpl, wsFactory) {
    this.options = options;
    this.fetchImpl = fetchImpl;
    this.wsFactory = wsFactory;
    this.readySettled = false;
    this.readyTimer = null;
    this.registeredOnce = false;
    this.ws = null;
    this.closed = false;
    this.attempt = 0;
    this.reconnectTimer = null;
    this.ready = new Promise((resolve, reject) => {
      this.resolveReady = resolve;
      this.rejectReady = reject;
    });
    this.ready.catch(() => {
    });
  }
  get connected() {
    return this.ws !== null && (this.ws.readyState === void 0 || this.ws.readyState === 1);
  }
  // True while the socket is still upgrading (WebSocket.CONNECTING).
  get connecting() {
    return this.ws !== null && this.ws.readyState === 0;
  }
  async connect() {
    if (this.closed) return;
    const url = await this.prepare();
    if (this.closed) return;
    const ws = this.wsFactory(url);
    this.ws = ws;
    listen(ws, "message", (event) => this.onMessage(event));
    listen(ws, "close", (event) => this.onCloseEvent(event));
    listen(ws, "error", (event) => this.onErrorEvent(event));
    if (!this.readySettled && this.readyTimer === null) {
      const timeout = this.options.registrationTimeoutMs ?? 3e4;
      this.readyTimer = setTimeout(() => {
        this.readyTimer = null;
        this.failReady(new Error(`ingress: registration did not complete within ${timeout}ms`));
      }, timeout);
    }
  }
  send(kind, fields = {}) {
    const ws = this.ws;
    if (!ws || ws.readyState !== void 0 && ws.readyState !== 1) {
      return false;
    }
    try {
      ws.send(frame(kind, fields));
      return true;
    } catch (error) {
      this.options.onError?.(error);
      return false;
    }
  }
  close(code = 1e3, reason = "") {
    if (this.closed) return;
    this.closed = true;
    this.clearReconnect();
    const ws = this.ws;
    this.ws = null;
    this.failReady(new Error("ingress: connection closed before registration"));
    try {
      ws?.close(code, reason);
    } catch {
    }
  }
  async prepare() {
    const { proxyHost, did, wsUrl } = this.options;
    if (!proxyHost) throw new Error("ingress: proxyHost is required");
    if (!did) throw new Error("ingress: did is required");
    if (typeof this.options.signer !== "function") {
      throw new Error("ingress: signer callback is required");
    }
    const nonceToken = await this.serviceToken(GET_NONCE_NSID);
    const nonceUrl = `${httpBase(proxyHost)}/xrpc/${GET_NONCE_NSID}`;
    // Call unbound: window.fetch throws "Illegal invocation" as a method.
    const fetchImpl = this.fetchImpl;
    const response = await fetchImpl(nonceUrl, {
      method: "POST",
      headers: {
        Accept: "application/json",
        Authorization: `Bearer ${nonceToken}`,
        "Content-Type": "application/json"
      },
      body: JSON.stringify({
        key: did,
        signatures: []
      })
    });
    const text = await response.text();
    if (!response.ok) {
      throw new Error(`ingress: nonce request failed: ${response.status} ${text.trim()}`);
    }
    let payload;
    try {
      payload = JSON.parse(text);
    } catch {
      throw new Error("ingress: nonce response is not JSON");
    }
    const nonce = payload?.nonce;
    if (typeof nonce !== "string" || nonce === "") {
      throw new Error("ingress: nonce response is missing nonce");
    }
    const signed = await this.options.signer(decodeBase64(nonce));
    const signature = signed instanceof Uint8Array ? signed : new Uint8Array(signed);
    const subscribeToken = await this.serviceToken(SUBSCRIBE_NSID);
    const registration = JSON.stringify({
      $type: REGISTRATION_TYPE,
      key: did,
      nonce,
      signatures: [
        {
          key: did,
          signature: encodeBase64(signature)
        }
      ]
    });
    const params = new URLSearchParams({
      registration,
      did,
      service_auth: subscribeToken
    });
    const base = wsUrl ?? `${wsBase(proxyHost)}/xrpc/${SUBSCRIBE_NSID}`;
    return `${base}${base.includes("?") ? "&" : "?"}${params}`;
  }
  async serviceToken(nsid) {
    if (this.options.getServiceAuthToken) {
      const token = await this.options.getServiceAuthToken(nsid);
      if (typeof token !== "string" || token === "") {
        throw new Error(`ingress: getServiceAuthToken returned no token for ${nsid}`);
      }
      return token;
    }
    if (this.options.serviceAuth) return this.options.serviceAuth;
    throw new Error(`ingress: no service-auth token for ${nsid}; pass serviceAuth or getServiceAuthToken`);
  }
  onMessage(event) {
    if (typeof event.data !== "string") return;
    const parsed = parseFrame(event.data);
    if (!parsed) return;
    this.options.onFrame?.(parsed);
    if (parsed.kind === "registered") {
      this.markRegistered(parsed);
    } else if (parsed.kind === "request") {
      void this.handleRequest(parsed);
    }
  }
  markRegistered(frame2) {
    this.attempt = 0;
    const info = {};
    if (typeof frame2.subdomain === "string") info.subdomain = frame2.subdomain;
    if (typeof frame2.ingressRef === "string") info.ingressRef = frame2.ingressRef;
    if (!this.readySettled) {
      this.readySettled = true;
      if (this.readyTimer !== null) {
        clearTimeout(this.readyTimer);
        this.readyTimer = null;
      }
      this.resolveReady(info);
    }
    if (this.registeredOnce) this.options.onReconnect?.(info);
    this.registeredOnce = true;
    this.options.onRegistered?.(info);
  }
  async handleRequest(frame2) {
    const request = {
      requestId: typeof frame2.requestId === "string" ? frame2.requestId : "",
      method: typeof frame2.method === "string" ? frame2.method : "GET",
      path: typeof frame2.path === "string" ? frame2.path : "",
      params: stringMap(frame2.params),
      body: frame2.body,
      rawBody: typeof frame2.rawBody === "string" ? frame2.rawBody : void 0,
      headers: stringMap(frame2.headers)
    };
    // The relay's keepalive probe is answered here, before the handler: a
    // handler that has no route for it answers 404, and the Go relay drops a
    // subscriber whose keepalive fails.
    if (request.path === KEEPALIVE_PATH) {
      this.send("response", {
        requestId: request.requestId,
        status: 204,
        body: "",
        contentType: "text/plain"
      });
      return;
    }
    const handler = this.options.handler;
    if (!handler) return;
    try {
      const response = await handler(request) ?? {};
      const fields = {
        requestId: request.requestId,
        status: response.status ?? 200,
        body: responseBody(response.body),
        contentType: response.contentType ?? "application/json"
      };
      if (response.headers && Object.keys(response.headers).length > 0) {
        fields.headers = response.headers;
      }
      this.send("response", fields);
    } catch (error) {
      this.send("response", {
        requestId: request.requestId,
        status: 500,
        body: {
          error: "InternalError",
          message: errorMessage(error)
        },
        contentType: "application/json"
      });
    }
  }
  onCloseEvent(event) {
    this.ws = null;
    const code = typeof event?.code === "number" ? event.code : 0;
    const reason = typeof event?.reason === "string" ? event.reason : "";
    this.options.onClose?.({
      code,
      reason
    });
    if (this.closed) return;
    if (this.options.reconnect === false) {
      this.failReady(new Error(`ingress: connection closed before registration${reason ? `: ${reason}` : ""}`));
      return;
    }
    this.scheduleReconnect();
  }
  onErrorEvent(event) {
    this.options.onError?.(event instanceof Error ? event : new Error("ingress: WebSocket error"));
  }
  scheduleReconnect() {
    if (this.closed || this.reconnectTimer !== null) return;
    const base = this.options.reconnectDelayMs ?? 1e3;
    const max = this.options.maxReconnectDelayMs ?? 3e4;
    const delay = Math.min(base * 2 ** this.attempt, max);
    this.attempt += 1;
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      void this.reconnect();
    }, delay);
  }
  async reconnect() {
    if (this.closed) return;
    try {
      await this.connect();
    } catch (error) {
      this.options.onError?.(error);
      this.scheduleReconnect();
    }
  }
  clearReconnect() {
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }
  failReady(error) {
    if (this.readySettled) return;
    this.readySettled = true;
    if (this.readyTimer !== null) {
      clearTimeout(this.readyTimer);
      this.readyTimer = null;
    }
    this.rejectReady(error);
  }
};
async function register(options) {
  if (!options || typeof options !== "object") {
    throw new Error("ingress: options are required");
  }
  const fetchImpl = options.fetchImpl ?? globalThis.fetch;
  if (typeof fetchImpl !== "function") {
    throw new Error("ingress: fetch is not available; pass fetchImpl");
  }
  if (!options.WebSocketImpl && typeof globalThis.WebSocket !== "function") {
    throw new Error("ingress: WebSocket is not available; pass WebSocketImpl");
  }
  const wsFactory = options.WebSocketImpl ?? ((url) => new WebSocket(url));
  const subscriber = new SubscriberConnection(options, fetchImpl, wsFactory);
  await subscriber.connect();
  return subscriber;
}
export {
  GET_NONCE_NSID,
  KEEPALIVE_PATH,
  REGISTRATION_TYPE,
  SUBSCRIBE_NSID,
  decodeBase64,
  encodeBase64,
  frame,
  httpBase,
  parseFrame,
  register,
  wsBase
};
