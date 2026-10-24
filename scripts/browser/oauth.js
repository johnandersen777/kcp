// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

/**
 * Browser helpers for the socialweb OAuth popup shell. The popup only carries
 * the authorization code back to the page; token exchange, DPoP keys and
 * session storage stay outside this module.
 */

export const OAUTH_CALLBACK_MESSAGE = "socialweb-oauth-callback";
export const OAUTH_CHANNEL = "socialweb-oauth";
export const OAUTH_POPUP_TIMEOUT_MS = 5 * 60 * 1000;

/** Validate a BroadcastChannel callback payload (same-origin by construction). */
export function isOAuthCallbackMessage(data) {
  return Boolean(data && data.type === OAUTH_CALLBACK_MESSAGE && typeof data.code === "string");
}

/** @type {Window | null} */
let currentPopup = null;

/**
 * Validate a postMessage event as the callback from the OAuth popup. The
 * message must come from this origin and from the popup we opened.
 *
 * @param {MessageEvent} event
 * @param {Window | null} popup
 * @param {string} origin
 * @returns {boolean}
 */
export function isValidOAuthCallback(event, popup, origin) {
  return Boolean(
    event &&
      popup &&
      event.origin === origin &&
      event.source === popup &&
      event.data &&
      event.data.type === OAUTH_CALLBACK_MESSAGE,
  );
}

/**
 * Open the authorization URL in a popup. Resolves with the validated callback
 * message, rejects if the popup is blocked, closed by the user, or times out.
 *
 * @param {string} url
 * @param {{ width?: number, height?: number, timeoutMs?: number }} [options]
 * @returns {Promise<Record<string, unknown>>}
 */
export function openOAuthPopup(url, options = {}) {
  const width = options.width ?? 520;
  const height = options.height ?? 720;
  const timeoutMs = options.timeoutMs ?? OAUTH_POPUP_TIMEOUT_MS;
  const left = Math.round(window.screenX + (window.outerWidth - width) / 2);
  const top = Math.round(window.screenY + (window.outerHeight - height) / 2);
  const popup = window.open(
    url,
    "socialweb-oauth",
    `popup=yes,width=${width},height=${height},left=${left},top=${top}`,
  );
  if (!popup) {
    return Promise.reject(new Error("OAuth popup was blocked"));
  }
  currentPopup = popup;

  return new Promise((resolve, reject) => {
    // Do not fail when the WindowProxy reports closed: COOP can flip that
    // while the callback page is still loading and broadcasting its message.
    const timeout = setTimeout(
      () => fail(new Error("OAuth popup timed out")),
      timeoutMs,
    );
    const onMessage = (event) => {
      if (isValidOAuthCallback(event, popup, location.origin)) {
        succeed(event.data);
      }
    };
    let channel = null;
    try {
      channel = new BroadcastChannel(OAUTH_CHANNEL);
      channel.onmessage = (event) => {
        if (isOAuthCallbackMessage(event.data)) succeed(event.data);
      };
    } catch {
      channel = null;
    }

    function cleanup() {
      clearTimeout(timeout);
      window.removeEventListener("message", onMessage);
      channel?.close();
      if (currentPopup === popup) {
        currentPopup = null;
      }
    }

    function finish(message, error) {
      cleanup();
      try {
        popup.close();
      } catch {
        // Already closed.
      }
      if (error) {
        reject(error);
      } else {
        resolve(message);
      }
    }

    function succeed(message) {
      finish(message, null);
    }

    function fail(error) {
      finish(null, error);
    }

    window.addEventListener("message", onMessage);
  });
}

/**
 * Subscribe to validated callback messages from the current popup.
 *
 * @param {(message: Record<string, unknown>) => void} fn
 * @returns {() => void} unsubscribe
 */
export function onOAuthCallback(fn) {
  const handler = (event) => {
    if (isValidOAuthCallback(event, currentPopup, location.origin)) {
      fn(event.data);
    }
  };
  window.addEventListener("message", handler);
  return () => window.removeEventListener("message", handler);
}
