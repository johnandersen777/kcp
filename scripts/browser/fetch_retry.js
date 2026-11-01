/*
Copyright 2026 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// A fetch that retries an idempotent request (GET or HEAD) once when the
// browser drops it without any HTTP response: fetch() rejects with a
// TypeError ("Load failed" in Safari, "Failed to fetch" in Chrome). iOS drops
// in-flight requests when a tab is backgrounded, the screen locks or the
// network switches; one such drop surfaced as a failed EphemeralInfra record
// read (owner report, page-rc135). HTTP errors are responses and pass through;
// an aborted request is never retried.

export const RETRY_DELAY_MS = 500;

function methodOf(input, init) {
  const method = init?.method ?? (typeof Request === "function" && input instanceof Request ? input.method : "GET");
  return String(method).toUpperCase();
}

function signalOf(input, init) {
  return init?.signal ?? (typeof Request === "function" && input instanceof Request ? input.signal : undefined);
}

function wait(ms, signal) {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason);
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(timer);
      reject(signal.reason);
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

/**
 * Wraps fetchFn. onRetry(url, err) is called before the one retry.
 */
export function retryingFetch(fetchFn, { delayMs = RETRY_DELAY_MS, onRetry } = {}) {
  return async (input, init) => {
    try {
      return await fetchFn(input, init);
    } catch (err) {
      const signal = signalOf(input, init);
      const method = methodOf(input, init);
      if (!(err instanceof TypeError) || signal?.aborted || (method !== "GET" && method !== "HEAD")) throw err;
      onRetry?.(typeof input === "string" ? input : input?.url ?? String(input), err);
      await wait(delayMs, signal);
      return await fetchFn(input, init);
    }
  };
}
