// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// The Request VM click's control flow (review of page-rc130 948ee11ba), kept
// apart from app.js so tests run the real thing:
// - one request at a time, decided before any await (a fast double click);
// - a click before kcp is ready waits for it, visibly;
// - the identity it was clicked for must still be signed in afterwards (a
//   Sign out may run meanwhile);
// - every way out but a request that went through re-enables the button
//   (refresh), so a quiet feed never leaves it disabled.

/**
 * @param {{getBusy: () => boolean, setBusy: (busy: boolean) => void,
 *   kcpUp: () => boolean, waitKcp: () => Promise<unknown>,
 *   identityReadyFor: (did: string, gen: number) => boolean, refresh: () => void,
 *   status: (text: string, kind: string) => void, log: (level: string, message: string) => void}} deps
 * @returns {(click: {did: string, gen?: number, prepare: () => Promise<{error?: string} | unknown>,
 *   request: (prepared: unknown) => Promise<boolean>}) => Promise<"busy"|"refused"|"failed"|"requested">}
 */
export function createRequestRunner({ getBusy, setBusy, kcpUp, waitKcp, identityReadyFor, refresh, status, log }) {
  return async function run({ did, gen, prepare, request }) {
    log("info", "Request VM clicked");
    if (getBusy()) {
      log("info", "Request VM is already running");
      return "busy";
    }
    setBusy(true);
    refresh();
    let requested = false;
    try {
      if (!kcpUp()) {
        status("kcp is still starting; the request runs once it is up", "warn");
        log("info", "Request VM waits for kcp to be ready");
        await waitKcp();
      }
      if (!identityReadyFor(did, gen)) throw new Error("signed out while the request waited; sign in and request again");
      const prepared = await prepare();
      if (prepared?.error) {
        status(prepared.error, "err");
        return "refused";
      }
      requested = await request(prepared);
      return requested ? "requested" : "failed";
    } catch (err) {
      log("error", `Request VM failed: ${err instanceof Error ? err.message : err}`);
      status(String(err?.message ?? err), "err");
      return "failed";
    } finally {
      // A request that went through stays busy until the feed shows it.
      if (!requested) setBusy(false);
      refresh();
    }
  };
}
