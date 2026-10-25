// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// OAuth token refresh worker: kcp's Go runtime (socialweb oauthclient's
// TokenRefresher seam, socialweb docs/ARCHITECTURE.md section 7) hands each
// refresh of the page's public atproto client here, so a busy Go runtime can
// never delay the token response and lose a rotated, single-use refresh
// token. The worker signs the DPoP proofs with a non-extractable WebCrypto
// key, stores every rotated set in IndexedDB before answering, and answers a
// stored newer set instead of POSTing a refresh token that is already spent.
// It never logs or stores the private key.
//
// Message in:  {id, request: TokenRefreshRequest JSON text} (+ a reply port)
// Message out: {id, response: TokenRefreshResponse or {error, error_description, status} JSON text}

const DB = "kcp-oauth-refresh";
const STORE = "sets";

/** base64url without padding. */
export function b64url(bytes) {
  let s = "";
  for (const b of new Uint8Array(bytes)) s += String.fromCharCode(b);
  return btoa(s).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}

const text = (s) => new TextEncoder().encode(s);

/** The htu of a DPoP proof: the endpoint without query or fragment. */
export function htu(endpoint) {
  const u = new URL(endpoint);
  return `${u.origin}${u.pathname}`;
}

/** A DPoP proof JWT for POST endpoint, signed with key (ES256, raw r||s). */
export async function dpopProof({ key, jwk, endpoint, nonce, now = Date.now() }) {
  const iat = Math.floor(now / 1000);
  const header = { typ: "dpop+jwt", alg: "ES256", jwk: { kty: jwk.kty, crv: jwk.crv, x: jwk.x, y: jwk.y } };
  const claims = { jti: b64url(crypto.getRandomValues(new Uint8Array(16))), htm: "POST", htu: htu(endpoint), iat, exp: iat + 30 };
  if (nonce) claims.nonce = nonce;
  const input = `${b64url(text(JSON.stringify(header)))}.${b64url(text(JSON.stringify(claims)))}`;
  const sig = await crypto.subtle.sign({ name: "ECDSA", hash: "SHA-256" }, key, text(input));
  return `${input}.${b64url(sig)}`;
}

// Keys imported once per session; non-extractable, so the private JWK is
// only ever held as the request text Go sent.
const keys = new Map();
async function signingKey(sessionID, jwk) {
  const id = `${sessionID}:${jwk.x}:${jwk.y}`;
  if (!keys.has(id)) {
    keys.set(id, crypto.subtle.importKey("jwk", { kty: "EC", crv: "P-256", x: jwk.x, y: jwk.y, d: jwk.d },
      { name: "ECDSA", namedCurve: "P-256" }, false, ["sign"]));
  }
  return await keys.get(id);
}

function openDB() {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(DB, 1);
    req.onupgradeneeded = () => req.result.createObjectStore(STORE);
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

/** IndexedDB, or an in-memory map when it is unavailable (the set is still answered). */
export function tokenStore(db = openDB().catch(() => null)) {
  const memory = new Map();
  const tx = async (mode, fn) => {
    const d = await db;
    if (!d) return fn(null);
    return await new Promise((resolve, reject) => {
      const t = d.transaction(STORE, mode);
      const out = fn(t.objectStore(STORE));
      t.oncomplete = () => resolve(out?.result ?? out);
      t.onerror = () => reject(t.error);
    });
  };
  return {
    async get(id) {
      try {
        const got = await tx("readonly", (s) => s ? s.get(id) : null);
        return got ?? memory.get(id) ?? null;
      } catch {
        return memory.get(id) ?? null;
      }
    },
    async put(id, set) {
      memory.set(id, set);
      await tx("readwrite", (s) => s?.put(set, id));
    },
  };
}

const fail = (error, description = "", status = 0) => ({ error, error_description: description, status });

/**
 * refresher(request) -> response object. fetchFn, store and clientID are
 * injected for tests. Single-flight per did+session_id.
 */
export function refresher({ fetchFn = fetch, store = tokenStore(), clientID = `${self.location.origin}/oauth-client-metadata.json` } = {}) {
  const flights = new Map();
  async function once(req) {
    const id = `${req.did}|${req.session_id}`;
    // A stored set newer than the token Go holds: answer it, never POST a
    // spent refresh token.
    const stored = await store.get(id);
    if (stored && stored.previous_refresh_token === req.refresh_token && stored.refresh_token !== req.refresh_token) {
      return stored.response;
    }
    const key = await signingKey(req.session_id, req.dpop_jwk);
    let nonce = req.dpop_nonce ?? "";
    for (let attempt = 0; attempt < 2; attempt++) {
      let res;
      try {
        res = await fetchFn(req.token_endpoint, {
          method: "POST",
          headers: {
            "content-type": "application/x-www-form-urlencoded",
            dpop: await dpopProof({ key, jwk: req.dpop_jwk, endpoint: req.token_endpoint, nonce }),
          },
          body: new URLSearchParams({ grant_type: "refresh_token", refresh_token: req.refresh_token, client_id: req.client_id }),
        });
      } catch (err) {
        return fail("network", String(err?.message ?? err));
      }
      nonce = res.headers.get("dpop-nonce") ?? nonce;
      const body = await res.json().catch(() => ({}));
      if (res.ok) {
        const now = Math.floor(Date.now() / 1000);
        const response = {
          access_token: body.access_token, refresh_token: body.refresh_token, token_type: body.token_type,
          scope: body.scope, sub: body.sub, expires_in: body.expires_in,
          expires_at: body.expires_in ? now + body.expires_in : undefined, dpop_nonce: nonce || undefined,
        };
        // Persist before answering; a failed write still answers the set.
        await store.put(id, { previous_refresh_token: req.refresh_token, refresh_token: body.refresh_token, response })
          .catch(() => {});
        return response;
      }
      if (attempt === 0 && body.error === "use_dpop_nonce" && (res.status === 400 || res.status === 401)) continue;
      return fail(body.error ?? "http_error", body.error_description ?? "", res.status);
    }
    return fail("use_dpop_nonce", "the authorization server kept asking for a new DPoP nonce");
  }
  return (req) => {
    if (req.client_id !== clientID) return Promise.resolve(fail("pass_through"));
    const id = `${req.did}|${req.session_id}`;
    if (!flights.has(id)) flights.set(id, once(req).finally(() => flights.delete(id)));
    return flights.get(id);
  };
}

if (typeof WorkerGlobalScope !== "undefined" && self instanceof WorkerGlobalScope) {
  const refresh = refresher();
  self.onmessage = async (event) => {
    const { id, request } = event.data ?? {};
    const reply = event.ports[0] ?? self;
    let response;
    try {
      response = await refresh(JSON.parse(request));
    } catch (err) {
      response = fail("worker", String(err?.message ?? err));
    }
    reply.postMessage({ id, response: JSON.stringify(response) });
  };
}
