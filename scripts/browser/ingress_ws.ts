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

export const SUBSCRIBE_NSID = "com.fedproxy.temp.xrpc.subscribe";
export const GET_NONCE_NSID = "com.fedproxy.temp.xrpc.getRegistrationNonce";
export const REGISTRATION_TYPE = "com.fedproxy.temp.xrpc.registration";
export const KEEPALIVE_PATH =
  "/xrpc/com.publicdomainrelay.temp.keepalive.ping";

export type FrameKind =
  | "registered"
  | "request"
  | "response"
  | "subscribe"
  | "subscriptionOpen"
  | "subscriptionEvent"
  | "subscriptionData"
  | "subscriptionCancel"
  | "subscriptionClose";

export interface Frame {
  $type: string;
  kind: string;
  [key: string]: unknown;
}

export interface IngressRequest {
  requestId: string;
  method: string;
  path: string;
  params: Record<string, string>;
  body: unknown;
  rawBody: string | undefined;
  headers: Record<string, string>;
}

export interface IngressResponse {
  status?: number;
  body?: unknown;
  contentType?: string;
  headers?: Record<string, string>;
}

export type IngressHandler = (
  request: IngressRequest,
) => IngressResponse | Promise<IngressResponse>;

export interface RegisteredInfo {
  subdomain?: string;
  ingressRef?: string;
}

export interface WebSocketLike {
  send(data: string | Uint8Array): void;
  close(code?: number, reason?: string): void;
  readyState?: number;
  addEventListener?(
    type: string,
    listener: (event: any) => void,
  ): void;
  onopen?: ((event: any) => void) | null;
  onmessage?: ((event: { data: unknown }) => void) | null;
  onclose?: ((event: any) => void) | null;
  onerror?: ((event: any) => void) | null;
}

export type WebSocketFactory = (url: string) => WebSocketLike;

export interface RegisterOptions {
  /** Relay proxy host, e.g. `relay.example.com` or `localhost:8080`. */
  proxyHost: string;
  did: string;
  /** Full subscribe endpoint override; query params are appended. */
  wsUrl?: string;
  /** Service-auth token used when getServiceAuthToken is not supplied. */
  serviceAuth?: string;
  /** Returns the service-auth token for an NSID (nonce or subscribe). */
  getServiceAuthToken?: (nsid: string) => string | Promise<string>;
  /** Signs the raw nonce bytes; the signature is base64-encoded on the wire. */
  signer: (
    nonce: Uint8Array,
  ) => Uint8Array | ArrayBuffer | Promise<Uint8Array | ArrayBuffer>;
  handler?: IngressHandler;
  fetchImpl?: typeof fetch;
  WebSocketImpl?: WebSocketFactory;
  registrationTimeoutMs?: number;
  reconnect?: boolean;
  reconnectDelayMs?: number;
  maxReconnectDelayMs?: number;
  onRegistered?: (info: RegisteredInfo) => void;
  onReconnect?: (info: RegisteredInfo) => void;
  onClose?: (event: { code: number; reason: string }) => void;
  onError?: (error: unknown) => void;
  onFrame?: (frame: Frame) => void;
}

export interface Subscriber {
  /** Resolves on the first `registered` frame. */
  readonly ready: Promise<RegisteredInfo>;
  readonly connected: boolean;
  send(kind: string, fields?: Record<string, unknown>): boolean;
  close(code?: number, reason?: string): void;
}

/**
 * Encodes a frame as JSON text. The `$type` discriminator is always first.
 */
export function frame(
  kind: string,
  fields: Record<string, unknown> = {},
): string {
  return JSON.stringify({ $type: `${SUBSCRIBE_NSID}#${kind}`, ...fields });
}

/**
 * Parses a subscribe-protocol text frame. Returns null for malformed JSON,
 * non-object payloads, and frames belonging to another lexicon.
 */
export function parseFrame(text: string): Frame | null {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    return null;
  }
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    return null;
  }
  const record = value as Record<string, unknown>;
  const type = record.$type;
  if (typeof type !== "string" || !type.startsWith(`${SUBSCRIBE_NSID}#`)) {
    return null;
  }
  return {
    ...record,
    $type: type,
    kind: type.slice(SUBSCRIBE_NSID.length + 1),
  };
}

export function encodeBase64(bytes: Uint8Array): string {
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
}

export function decodeBase64(text: string): Uint8Array {
  const binary = atob(text);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return bytes;
}

/** HTTP origin for the relay, honoring an explicit scheme or localhost. */
export function httpBase(proxyHost: string): string {
  return origin(proxyHost, "http", "https");
}

/** WebSocket origin for the relay, honoring an explicit scheme or localhost. */
export function wsBase(proxyHost: string): string {
  return origin(proxyHost, "ws", "wss");
}

function origin(host: string, insecure: string, secure: string): string {
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
  const local =
    trimmed === "localhost" ||
    trimmed.startsWith("127.") ||
    trimmed.startsWith("[::1]") ||
    /:\d+$/.test(trimmed);
  return `${local ? insecure : secure}://${trimmed}`;
}

function listen(
  ws: WebSocketLike,
  type: "message" | "close" | "error" | "open",
  listener: (event: any) => void,
): void {
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

function stringMap(value: unknown): Record<string, string> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    return {};
  }
  const out: Record<string, string> = {};
  for (const [key, item] of Object.entries(value)) {
    if (typeof item === "string") out[key] = item;
  }
  return out;
}

function responseBody(body: unknown): unknown {
  if (body === undefined || body === null) return "";
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

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

class SubscriberConnection implements Subscriber {
  readonly ready: Promise<RegisteredInfo>;
  private resolveReady!: (info: RegisteredInfo) => void;
  private rejectReady!: (error: unknown) => void;
  private readySettled = false;
  private readyTimer: ReturnType<typeof setTimeout> | null = null;
  private registeredOnce = false;
  private ws: WebSocketLike | null = null;
  private closed = false;
  private attempt = 0;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;

  constructor(
    private readonly options: RegisterOptions,
    private readonly fetchImpl: typeof fetch,
    private readonly wsFactory: WebSocketFactory,
  ) {
    this.ready = new Promise<RegisteredInfo>((resolve, reject) => {
      this.resolveReady = resolve;
      this.rejectReady = reject;
    });
    // Callers may only care about onRegistered; do not surface a rejection
    // nobody is listening for.
    this.ready.catch(() => {});
  }

  get connected(): boolean {
    return (
      this.ws !== null &&
      (this.ws.readyState === undefined || this.ws.readyState === 1)
    );
  }

  async connect(): Promise<void> {
    if (this.closed) return;
    const url = await this.prepare();
    if (this.closed) return;
    const ws = this.wsFactory(url);
    this.ws = ws;
    listen(ws, "message", (event) => this.onMessage(event));
    listen(ws, "close", (event) => this.onCloseEvent(event));
    listen(ws, "error", (event) => this.onErrorEvent(event));
    if (!this.readySettled && this.readyTimer === null) {
      const timeout = this.options.registrationTimeoutMs ?? 30000;
      this.readyTimer = setTimeout(() => {
        this.readyTimer = null;
        this.failReady(
          new Error(
            `ingress: registration did not complete within ${timeout}ms`,
          ),
        );
      }, timeout);
    }
  }

  send(kind: string, fields: Record<string, unknown> = {}): boolean {
    const ws = this.ws;
    if (
      !ws ||
      (ws.readyState !== undefined && ws.readyState !== 1)
    ) {
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

  close(code = 1000, reason = ""): void {
    if (this.closed) return;
    this.closed = true;
    this.clearReconnect();
    const ws = this.ws;
    this.ws = null;
    this.failReady(
      new Error("ingress: connection closed before registration"),
    );
    try {
      ws?.close(code, reason);
    } catch {
      // The socket may already be closed.
    }
  }

  private async prepare(): Promise<string> {
    const { proxyHost, did, wsUrl } = this.options;
    if (!proxyHost) throw new Error("ingress: proxyHost is required");
    if (!did) throw new Error("ingress: did is required");
    if (typeof this.options.signer !== "function") {
      throw new Error("ingress: signer callback is required");
    }

    const nonceToken = await this.serviceToken(GET_NONCE_NSID);
    const nonceUrl = `${httpBase(proxyHost)}/xrpc/${GET_NONCE_NSID}`;
    const response = await this.fetchImpl(nonceUrl, {
      method: "POST",
      headers: {
        Accept: "application/json",
        Authorization: `Bearer ${nonceToken}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ key: did, signatures: [] }),
    });
    const text = await response.text();
    if (!response.ok) {
      throw new Error(
        `ingress: nonce request failed: ${response.status} ${text.trim()}`,
      );
    }
    let payload: unknown;
    try {
      payload = JSON.parse(text);
    } catch {
      throw new Error("ingress: nonce response is not JSON");
    }
    const nonce = (payload as { nonce?: unknown } | null)?.nonce;
    if (typeof nonce !== "string" || nonce === "") {
      throw new Error("ingress: nonce response is missing nonce");
    }

    const signed = await this.options.signer(decodeBase64(nonce));
    const signature = signed instanceof Uint8Array
      ? signed
      : new Uint8Array(signed);
    const subscribeToken = await this.serviceToken(SUBSCRIBE_NSID);

    const registration = JSON.stringify({
      $type: REGISTRATION_TYPE,
      key: did,
      nonce,
      signatures: [{ key: did, signature: encodeBase64(signature) }],
    });
    const params = new URLSearchParams({
      registration,
      did,
      service_auth: subscribeToken,
    });
    const base = wsUrl ?? `${wsBase(proxyHost)}/xrpc/${SUBSCRIBE_NSID}`;
    return `${base}${base.includes("?") ? "&" : "?"}${params}`;
  }

  private async serviceToken(nsid: string): Promise<string> {
    if (this.options.getServiceAuthToken) {
      const token = await this.options.getServiceAuthToken(nsid);
      if (typeof token !== "string" || token === "") {
        throw new Error(
          `ingress: getServiceAuthToken returned no token for ${nsid}`,
        );
      }
      return token;
    }
    if (this.options.serviceAuth) return this.options.serviceAuth;
    throw new Error(
      `ingress: no service-auth token for ${nsid}; pass serviceAuth or getServiceAuthToken`,
    );
  }

  private onMessage(event: { data: unknown }): void {
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

  private markRegistered(frame: Frame): void {
    this.attempt = 0;
    const info: RegisteredInfo = {};
    if (typeof frame.subdomain === "string") info.subdomain = frame.subdomain;
    if (typeof frame.ingressRef === "string") info.ingressRef = frame.ingressRef;
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

  private async handleRequest(frame: Frame): Promise<void> {
    const request: IngressRequest = {
      requestId:
        typeof frame.requestId === "string" ? frame.requestId : "",
      method: typeof frame.method === "string" ? frame.method : "GET",
      path: typeof frame.path === "string" ? frame.path : "",
      params: stringMap(frame.params),
      body: frame.body,
      rawBody: typeof frame.rawBody === "string" ? frame.rawBody : undefined,
      headers: stringMap(frame.headers),
    };

    const handler = this.options.handler;
    if (!handler) {
      if (request.path === KEEPALIVE_PATH) {
        this.send("response", {
          requestId: request.requestId,
          status: 204,
          body: "",
          contentType: "text/plain",
        });
      }
      return;
    }

    try {
      const response = (await handler(request)) ?? {};
      const fields: Record<string, unknown> = {
        requestId: request.requestId,
        status: response.status ?? 200,
        body: responseBody(response.body),
        contentType: response.contentType ?? "application/json",
      };
      if (response.headers && Object.keys(response.headers).length > 0) {
        fields.headers = response.headers;
      }
      this.send("response", fields);
    } catch (error) {
      this.send("response", {
        requestId: request.requestId,
        status: 500,
        body: { error: "InternalError", message: errorMessage(error) },
        contentType: "application/json",
      });
    }
  }

  private onCloseEvent(event: { code?: unknown; reason?: unknown }): void {
    this.ws = null;
    const code = typeof event?.code === "number" ? event.code : 0;
    const reason = typeof event?.reason === "string" ? event.reason : "";
    this.options.onClose?.({ code, reason });
    if (this.closed) return;
    if (this.options.reconnect === false) {
      this.failReady(
        new Error(
          `ingress: connection closed before registration${reason ? `: ${reason}` : ""}`,
        ),
      );
      return;
    }
    this.scheduleReconnect();
  }

  private onErrorEvent(event: unknown): void {
    this.options.onError?.(
      event instanceof Error ? event : new Error("ingress: WebSocket error"),
    );
  }

  private scheduleReconnect(): void {
    if (this.closed || this.reconnectTimer !== null) return;
    const base = this.options.reconnectDelayMs ?? 1000;
    const max = this.options.maxReconnectDelayMs ?? 30000;
    const delay = Math.min(base * 2 ** this.attempt, max);
    this.attempt += 1;
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      void this.reconnect();
    }, delay);
  }

  private async reconnect(): Promise<void> {
    if (this.closed) return;
    try {
      await this.connect();
    } catch (error) {
      this.options.onError?.(error);
      this.scheduleReconnect();
    }
  }

  private clearReconnect(): void {
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }

  private failReady(error: unknown): void {
    if (this.readySettled) return;
    this.readySettled = true;
    if (this.readyTimer !== null) {
      clearTimeout(this.readyTimer);
      this.readyTimer = null;
    }
    this.rejectReady(error);
  }
}

/**
 * Registers this client as a subscriber with the relay and starts serving
 * request frames. Returns once the WebSocket exists; `ready` resolves when the
 * relay confirms registration. Reconnects re-register with a fresh nonce.
 */
export async function register(
  options: RegisterOptions,
): Promise<Subscriber> {
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
  const wsFactory: WebSocketFactory =
    options.WebSocketImpl ??
    ((url) => new WebSocket(url) as unknown as WebSocketLike);
  const subscriber = new SubscriberConnection(options, fetchImpl, wsFactory);
  await subscriber.connect();
  return subscriber;
}
