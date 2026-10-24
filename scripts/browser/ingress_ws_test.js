// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import {
  encodeBase64,
  frame,
  GET_NONCE_NSID,
  KEEPALIVE_PATH,
  parseFrame,
  register,
  SUBSCRIBE_NSID,
} from "./ingress_ws.js";

function assertEquals(actual, expected) {
  const a = JSON.stringify(actual);
  const e = JSON.stringify(expected);
  if (a !== e) {
    throw new Error(`expected:\n${e}\nactual:\n${a}`);
  }
}

function assert(condition, message) {
  if (!condition) throw new Error(message);
}

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

class FakeWebSocket {
  static instances = [];

  constructor(url) {
    this.url = url;
    this.readyState = 1;
    this.sent = [];
    FakeWebSocket.instances.push(this);
  }

  send(data) {
    this.sent.push(data);
  }

  close() {
    this.readyState = 3;
  }

  emit(value) {
    this.onmessage?.({
      data: typeof value === "string" ? value : JSON.stringify(value),
    });
  }
}

function nonceFetch(nonce, calls = []) {
  // Like window.fetch, refuse to run as a method of another object.
  return async function (url, init) {
    if (this !== undefined && this !== globalThis) {
      throw new TypeError("Failed to execute 'fetch': Illegal invocation");
    }
    calls.push({ url, init });
    return new Response(JSON.stringify({ nonce: encodeBase64(nonce) }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  };
}

Deno.test("frame encodes the lexicon discriminator first", () => {
  assertEquals(
    frame("request", { requestId: "r1", method: "GET" }),
    '{"$type":"com.fedproxy.temp.xrpc.subscribe#request","requestId":"r1","method":"GET"}',
  );
});

Deno.test("parseFrame recognizes every subscribe kind", () => {
  const kinds = [
    "registered",
    "request",
    "response",
    "subscribe",
    "subscriptionOpen",
    "subscriptionEvent",
    "subscriptionData",
    "subscriptionCancel",
    "subscriptionClose",
  ];
  for (const kind of kinds) {
    const parsed = parseFrame(frame(kind, { requestId: kind }));
    assert(parsed !== null, `parseFrame returned null for ${kind}`);
    assertEquals(parsed.kind, kind);
    assertEquals(parsed.$type, `${SUBSCRIBE_NSID}#${kind}`);
    assertEquals(parsed.requestId, kind);
  }
});

Deno.test("parseFrame rejects malformed and foreign frames", () => {
  assertEquals(parseFrame("not json"), null);
  assertEquals(parseFrame("[]"), null);
  assertEquals(parseFrame("null"), null);
  assertEquals(parseFrame('"text"'), null);
  assertEquals(parseFrame('{"$type":42}'), null);
  assertEquals(parseFrame('{"$type":"com.example.other#request"}'), null);
});

Deno.test("register posts the nonce request and builds the subscribe URL", async () => {
  FakeWebSocket.instances = [];
  const calls = [];
  const nonce = new Uint8Array([0, 1, 2, 250, 255]);
  const subscriber = await register({
    proxyHost: "relay.example.com",
    did: "did:plc:test",
    serviceAuth: "service-token",
    signer: (bytes) => bytes,
    fetchImpl: nonceFetch(nonce, calls),
    WebSocketImpl: (url) => new FakeWebSocket(url),
  });
  try {
    assertEquals(calls.length, 1);
    assertEquals(
      calls[0].url,
      `https://relay.example.com/xrpc/${GET_NONCE_NSID}`,
    );
    assertEquals(calls[0].init.method, "POST");
    assertEquals(
      calls[0].init.headers.Authorization,
      "Bearer service-token",
    );
    assertEquals(
      calls[0].init.body,
      '{"key":"did:plc:test","signatures":[]}',
    );

    assertEquals(FakeWebSocket.instances.length, 1);
    const url = new URL(FakeWebSocket.instances[0].url);
    assertEquals(url.protocol, "wss:");
    assertEquals(url.host, "relay.example.com");
    assertEquals(url.pathname, `/xrpc/${SUBSCRIBE_NSID}`);
    assertEquals(url.searchParams.get("did"), "did:plc:test");
    assertEquals(url.searchParams.get("service_auth"), "service-token");
    const registration = JSON.parse(url.searchParams.get("registration"));
    assertEquals(registration.$type, "com.fedproxy.temp.xrpc.registration");
    assertEquals(registration.key, "did:plc:test");
    assertEquals(registration.nonce, encodeBase64(nonce));
    assertEquals(registration.signatures, [
      { key: "did:plc:test", signature: encodeBase64(nonce) },
    ]);
  } finally {
    subscriber.close();
  }
});

Deno.test("register asks for a service-auth token per NSID", async () => {
  FakeWebSocket.instances = [];
  const seen = [];
  const calls = [];
  const subscriber = await register({
    proxyHost: "localhost:8787",
    did: "did:plc:test",
    getServiceAuthToken: (nsid) => {
      seen.push(nsid);
      return `token-for-${nsid}`;
    },
    signer: () => new Uint8Array([9]),
    fetchImpl: nonceFetch(new Uint8Array([7]), calls),
    WebSocketImpl: (url) => new FakeWebSocket(url),
  });
  try {
    assertEquals(seen, [GET_NONCE_NSID, SUBSCRIBE_NSID]);
    assertEquals(
      calls[0].init.headers.Authorization,
      `Bearer token-for-${GET_NONCE_NSID}`,
    );
    const url = new URL(FakeWebSocket.instances[0].url);
    assertEquals(url.protocol, "ws:");
    assertEquals(
      url.searchParams.get("service_auth"),
      `token-for-${SUBSCRIBE_NSID}`,
    );
  } finally {
    subscriber.close();
  }
});

Deno.test("a request frame is answered with a response frame", async () => {
  FakeWebSocket.instances = [];
  const seen = [];
  const subscriber = await register({
    proxyHost: "relay.example.com",
    did: "did:plc:test",
    serviceAuth: "token",
    signer: (bytes) => bytes,
    fetchImpl: nonceFetch(new Uint8Array([1, 2, 3])),
    WebSocketImpl: (url) => new FakeWebSocket(url),
    handler: async (request) => {
      seen.push(request);
      return {
        status: 201,
        body: { ok: true, path: request.path },
        contentType: "application/json",
        headers: { "x-test": "yes" },
      };
    },
  });
  try {
    const ws = FakeWebSocket.instances[0];
    ws.emit({
      $type: `${SUBSCRIBE_NSID}#registered`,
      subdomain: "did-plc-test",
      ingressRef: "did:web:did-plc-test.relay.example.com",
    });
    const info = await subscriber.ready;
    assertEquals(info.subdomain, "did-plc-test");
    assert(subscriber.connected, "subscriber reports connected");

    ws.emit({
      $type: `${SUBSCRIBE_NSID}#request`,
      requestId: "req-1",
      method: "POST",
      path: "/xrpc/com.example.write",
      params: { a: "b" },
      body: { hello: "world" },
      rawBody: '{"hello":"world"}',
      headers: { "content-type": "application/json" },
    });
    await tick();

    assertEquals(seen.length, 1);
    assertEquals(seen[0], {
      requestId: "req-1",
      method: "POST",
      path: "/xrpc/com.example.write",
      params: { a: "b" },
      body: { hello: "world" },
      rawBody: '{"hello":"world"}',
      headers: { "content-type": "application/json" },
    });

    assertEquals(ws.sent.length, 1);
    const response = JSON.parse(ws.sent[0]);
    assertEquals(response.$type, `${SUBSCRIBE_NSID}#response`);
    assertEquals(response.requestId, "req-1");
    assertEquals(response.status, 201);
    assertEquals(response.body, { ok: true, path: "/xrpc/com.example.write" });
    assertEquals(response.contentType, "application/json");
    assertEquals(response.headers, { "x-test": "yes" });
  } finally {
    subscriber.close();
  }
});

Deno.test("a handler failure produces an InternalError response", async () => {
  FakeWebSocket.instances = [];
  const subscriber = await register({
    proxyHost: "relay.example.com",
    did: "did:plc:test",
    serviceAuth: "token",
    signer: (bytes) => bytes,
    fetchImpl: nonceFetch(new Uint8Array([1])),
    WebSocketImpl: (url) => new FakeWebSocket(url),
    handler: () => {
      throw new Error("boom");
    },
  });
  try {
    const ws = FakeWebSocket.instances[0];
    ws.emit({
      $type: `${SUBSCRIBE_NSID}#request`,
      requestId: "req-2",
      method: "GET",
      path: "/xrpc/com.example.read",
    });
    await tick();

    const response = JSON.parse(ws.sent[0]);
    assertEquals(response.requestId, "req-2");
    assertEquals(response.status, 500);
    assertEquals(response.contentType, "application/json");
    assertEquals(response.body, {
      error: "InternalError",
      message: "boom",
    });
  } finally {
    subscriber.close();
  }
});

Deno.test("keepalive requests are answered without the handler", async () => {
  FakeWebSocket.instances = [];
  const subscriber = await register({
    proxyHost: "relay.example.com",
    did: "did:plc:test",
    serviceAuth: "token",
    signer: (bytes) => bytes,
    fetchImpl: nonceFetch(new Uint8Array([1])),
    WebSocketImpl: (url) => new FakeWebSocket(url),
    handler: () => ({ status: 404, body: { error: "NotFound" } }),
  });
  try {
    const ws = FakeWebSocket.instances[0];
    ws.emit({
      $type: `${SUBSCRIBE_NSID}#request`,
      requestId: "keepalive-1",
      method: "GET",
      path: KEEPALIVE_PATH,
      params: {},
      body: null,
      headers: {},
    });
    await tick();

    const response = JSON.parse(ws.sent[0]);
    assertEquals(response.requestId, "keepalive-1");
    assertEquals(response.status, 204);
  } finally {
    subscriber.close();
  }
});

Deno.test("response bodies are sent as raw JSON when possible", async () => {
  FakeWebSocket.instances = [];
  let body = '{"raw":true}';
  const subscriber = await register({
    proxyHost: "relay.example.com",
    did: "did:plc:test",
    serviceAuth: "token",
    signer: (bytes) => bytes,
    fetchImpl: nonceFetch(new Uint8Array([1])),
    WebSocketImpl: (url) => new FakeWebSocket(url),
    handler: () => ({ status: 200, body }),
  });
  try {
    const ws = FakeWebSocket.instances[0];
    ws.emit({
      $type: `${SUBSCRIBE_NSID}#request`,
      requestId: "req-3",
      method: "GET",
      path: "/xrpc/com.example.read",
    });
    await tick();
    assertEquals(JSON.parse(ws.sent[0]).body, { raw: true });

    body = "plain text";
    ws.emit({
      $type: `${SUBSCRIBE_NSID}#request`,
      requestId: "req-4",
      method: "GET",
      path: "/xrpc/com.example.read",
    });
    await tick();
    assertEquals(JSON.parse(ws.sent[1]).body, "plain text");
  } finally {
    subscriber.close();
  }
});
