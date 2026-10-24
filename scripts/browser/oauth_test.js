// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import {
  DEFAULT_SCOPE,
  buildClientMetadata,
  normalizeOrigin,
  requestOrigin,
} from "../serve-public.ts";
import {
  OAUTH_CALLBACK_MESSAGE,
  callbackMatchesURL,
  isOAuthCallbackMessage,
  isValidOAuthCallback,
} from "./oauth.js";

Deno.test("callbackMatchesURL drops a late answer to an earlier URL", () => {
  const url = "https://bsky.social/oauth/authorize?client_id=x&state=new";
  assert(callbackMatchesURL({ state: "new", code: "c" }, url), "the URL's own state");
  assert(!callbackMatchesURL({ state: "old", code: "c" }, url), "another state rejected");
  assert(callbackMatchesURL({ state: "any" }, "https://bsky.social/oauth/authorize?request_uri=urn:x"), "a PAR URL has no state");
});

function assertEquals(actual, expected, label = "value") {
  const a = JSON.stringify(actual);
  const e = JSON.stringify(expected);
  if (a !== e) {
    throw new Error(`${label}: expected ${e}, got ${a}`);
  }
}

function assert(condition, label) {
  if (!condition) {
    throw new Error(`expected ${label}`);
  }
}

Deno.test("normalizeOrigin adds https and strips trailing slashes", () => {
  assertEquals(normalizeOrigin("example.com"), "https://example.com");
  assertEquals(normalizeOrigin("https://Example.COM/"), "https://example.com");
  assertEquals(
    normalizeOrigin("http://127.0.0.1:8090/"),
    "http://127.0.0.1:8090",
  );

  let threw = false;
  try {
    normalizeOrigin("  ");
  } catch {
    threw = true;
  }
  assert(threw, "empty origin to throw");
});

Deno.test("requestOrigin prefers the Host header unless overridden", () => {
  const request = new Request("http://localhost/oauth-client-metadata.json", {
    headers: { host: "socialweb.example:8443" },
  });
  assertEquals(requestOrigin(request), "https://socialweb.example:8443");
  assertEquals(
    requestOrigin(request, "https://public.example/"),
    "https://public.example",
  );
});

Deno.test("buildClientMetadata is the atproto public client document", () => {
  const metadata = buildClientMetadata(
    "https://socialweb.example",
    DEFAULT_SCOPE,
  );
  assertEquals(
    JSON.stringify(metadata),
    JSON.stringify({
      client_id: "https://socialweb.example/oauth-client-metadata.json",
      redirect_uris: ["https://socialweb.example/oauth/callback/requester"],
      dpop_bound_access_tokens: true,
      application_type: "web",
      grant_types: ["authorization_code", "refresh_token"],
      response_types: ["code"],
      token_endpoint_auth_method: "none",
      client_name: "socialweb-computer requester",
      client_uri: "https://socialweb.example",
      scope: DEFAULT_SCOPE,
    }),
  );
  assertEquals(
    buildClientMetadata("socialweb.example:8443", DEFAULT_SCOPE).client_id,
    "https://socialweb.example:8443/oauth-client-metadata.json",
  );
});

Deno.test("scope is passed through verbatim", () => {
  const scope = "atproto repo:app.bsky.feed.post?action=create";
  assertEquals(
    buildClientMetadata("https://socialweb.example", scope).scope,
    scope,
  );
});

Deno.test("isValidOAuthCallback checks message type, origin and source", () => {
  const popup = {};
  const origin = "https://socialweb.example";
  const event = {
    origin,
    source: popup,
    data: {
      type: OAUTH_CALLBACK_MESSAGE,
      code: "abc",
      state: "state-1",
      iss: "https://pds.example",
      href: `${origin}/oauth-callback.html?code=abc`,
    },
  };

  assertEquals(OAUTH_CALLBACK_MESSAGE, "socialweb-oauth-callback");
  assert(isValidOAuthCallback(event, popup, origin), "valid callback");
  assert(
    !isValidOAuthCallback(
      { ...event, origin: "https://evil.example" },
      popup,
      origin,
    ),
    "foreign origin rejected",
  );
  assert(
    !isValidOAuthCallback({ ...event, source: {} }, popup, origin),
    "foreign source rejected",
  );
  assert(
    !isValidOAuthCallback(
      { ...event, data: { type: "other" } },
      popup,
      origin,
    ),
    "foreign message rejected",
  );
  assert(!isValidOAuthCallback(null, popup, origin), "missing event rejected");
  assert(!isValidOAuthCallback(event, null, origin), "missing popup rejected");
});

Deno.test("BroadcastChannel callbacks carry a code or an error", () => {
  assertEquals(isOAuthCallbackMessage({ type: OAUTH_CALLBACK_MESSAGE, code: "c" }), true);
  assertEquals(isOAuthCallbackMessage({ type: OAUTH_CALLBACK_MESSAGE, error: "access_denied" }), true);
  assertEquals(isOAuthCallbackMessage({ type: OAUTH_CALLBACK_MESSAGE, code: null, error: null }), false);
});
