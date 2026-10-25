// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import { b64url, dpopProof, htu, refresher } from "./oauth_refresh_worker.js";

function assert(cond, message) {
  if (!cond) throw new Error(message);
}

const fromB64url = (s) => Uint8Array.from(atob(s.replaceAll("-", "+").replaceAll("_", "/")), (c) => c.charCodeAt(0));

async function sessionKey() {
  const pair = await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, true, ["sign", "verify"]);
  const jwk = await crypto.subtle.exportKey("jwk", pair.privateKey);
  return { jwk: { kty: jwk.kty, crv: jwk.crv, x: jwk.x, y: jwk.y, d: jwk.d }, publicKey: pair.publicKey };
}

function memoryStore() {
  const m = new Map();
  return { m, get: async (id) => m.get(id) ?? null, put: async (id, set) => void m.set(id, set) };
}

const CLIENT = "https://page.example/oauth-client-metadata.json";
const request = (jwk, refresh = "r0") => ({
  session_id: "s1", did: "did:plc:alice", issuer: "https://bsky.social", token_endpoint: "https://bsky.social/oauth/token?x=1",
  client_id: CLIENT, refresh_token: refresh, dpop_jwk: jwk,
});
const tokenResponse = (refresh, headers = {}) =>
  new Response(JSON.stringify({ access_token: "a", refresh_token: refresh, token_type: "DPoP", scope: "atproto", sub: "did:plc:alice", expires_in: 60 }),
    { status: 200, headers });

Deno.test("dpopProof: ES256 over header.claims, verifiable with the public key", async () => {
  const { jwk, publicKey } = await sessionKey();
  const key = await crypto.subtle.importKey("jwk", { ...jwk }, { name: "ECDSA", namedCurve: "P-256" }, false, ["sign"]);
  const jwt = await dpopProof({ key, jwk, endpoint: "https://bsky.social/oauth/token?q=1#f", nonce: "n1", now: 1_000_000 });
  const [h, c, s] = jwt.split(".");
  const header = JSON.parse(new TextDecoder().decode(fromB64url(h)));
  const claims = JSON.parse(new TextDecoder().decode(fromB64url(c)));
  assert(header.typ === "dpop+jwt" && header.alg === "ES256" && header.jwk.d === undefined, "header carries the public jwk only");
  assert(claims.htm === "POST" && claims.htu === "https://bsky.social/oauth/token" && claims.nonce === "n1", "claims");
  assert(claims.iat === 1000 && claims.exp === 1030 && claims.jti.length > 16, "times and jti");
  const ok = await crypto.subtle.verify({ name: "ECDSA", hash: "SHA-256" }, publicKey, fromB64url(s), new TextEncoder().encode(`${h}.${c}`));
  assert(ok, "signature verifies");
  assert(htu("https://a.example/p?q#f") === "https://a.example/p" && b64url(new Uint8Array([251, 255])) === "-_8", "helpers");
});

Deno.test("refresher: pass_through for other clients", async () => {
  const { jwk } = await sessionKey();
  const refresh = refresher({ fetchFn: () => Promise.reject(new Error("no POST")), store: memoryStore(), clientID: CLIENT });
  const out = await refresh({ ...request(jwk), client_id: "https://other.example/client.json" });
  assert(out.error === "pass_through", "pass_through");
});

Deno.test("refresher: single flight, nonce retry, persist, never re-POST a spent token", async () => {
  const { jwk } = await sessionKey();
  const store = memoryStore();
  let posts = 0;
  const fetchFn = async (_url, init) => {
    posts++;
    assert(init.headers.dpop.split(".").length === 3, "a DPoP proof on every POST");
    if (posts === 1) {
      return new Response(JSON.stringify({ error: "use_dpop_nonce" }), { status: 400, headers: { "dpop-nonce": "n2" } });
    }
    return tokenResponse("r1", { "dpop-nonce": "n3" });
  };
  const refresh = refresher({ fetchFn, store, clientID: CLIENT });
  const [a, b] = await Promise.all([refresh(request(jwk)), refresh(request(jwk))]);
  assert(posts === 2 && a === b, `one flight with one nonce retry (posts=${posts})`);
  assert(a.refresh_token === "r1" && a.dpop_nonce === "n3" && a.expires_at > 0, "rotated set answered");
  assert(store.m.get("did:plc:alice|s1").refresh_token === "r1", "persisted");
  const again = await refresh(request(jwk, "r0"));
  assert(posts === 2 && again.refresh_token === "r1", "spent r0 answered from the store without a POST");
});

Deno.test("refresher: invalid_grant is returned as an error code", async () => {
  const { jwk } = await sessionKey();
  const fetchFn = async () => new Response(JSON.stringify({ error: "invalid_grant", error_description: "Invalid refresh token" }), { status: 400 });
  const out = await refresher({ fetchFn, store: memoryStore(), clientID: CLIENT })(request(jwk));
  assert(out.error === "invalid_grant" && out.status === 400, "invalid_grant");
});
