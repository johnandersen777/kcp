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

/**
 * Runs the kcp WASM shard and serves its Kubernetes API over HTTP.
 *
 *   deno run --allow-read --allow-net --allow-env scripts/kcp_wasm.ts \
 *     --wasm=bin/wasm/kcp.wasm \
 *     --wasm-exec=bin/wasm/wasm_exec.js \
 *     --port=18080
 *
 * Architecture:
 *
 *   kubectl/curl --HTTP--> Deno.serve (main thread)
 *                            |
 *                            | postMessage
 *                            v
 *                         Web Worker: WebAssembly.instantiate(go wasm)
 *                            |
 *                            | Go net/http (js/wasm port)
 *                            v
 *                         fetch() --> Deno (this process), e.g. /echo
 *
 * The Worker creation and message passing follow
 * https://docs.deno.com/examples/web_workers/; the module is instantiated
 * with the standard WebAssembly API as in
 * https://docs.deno.com/examples/webassembly/ (plus Go's wasm_exec.js import
 * object).
 *
 * Go's js/wasm net/http transport is implemented on top of the Fetch API
 * (net/http/roundtrip_js.go), so outbound requests made with plain
 * http.Get/http.Client are transparently executed by Deno. No custom
 * RoundTripper or call-site changes are involved.
 */

import {
  buildClientMetadata,
  DEFAULT_SCOPE,
  METADATA_PATH,
} from "./serve-public.ts";

const options = new Map<string, string>();
for (const arg of Deno.args) {
  const match = arg.match(/^--([^=]+)=(.*)$/);
  if (match) {
    options.set(match[1], match[2]);
  } else if (arg.startsWith("--")) {
    options.set(arg.slice(2), "true");
  }
}

const hostname = options.get("host") ?? "127.0.0.1";
const port = Number(options.get("port") ?? "18080");
const socialwebOrigin = options.get("socialweb-origin") ?? `http://${hostname}:${port}`;
const wasmPath = Deno.realPathSync(
  options.get("wasm") ?? "bin/wasm/kcp.wasm",
);
const wasmExecPath = Deno.realPathSync(
  options.get("wasm-exec") ?? "bin/wasm/wasm_exec.js",
);
const verbose = options.get("verbose") === "true";

// Deno owns persistence: it loads the snapshot before starting the wasm
// module, and atomically rewrites the file whenever Go reports a mutation.
const statePath = options.get("state") ?? `${wasmPath}.state.json`;
let initialState = "";
try {
  initialState = await Deno.readTextFile(statePath);
  console.error(`kcp-wasm: loaded state from ${statePath}`);
} catch (err) {
  if (!(err instanceof Deno.errors.NotFound)) {
    console.error(`kcp-wasm: unable to read state file ${statePath}:`, err);
  }
}

type ResponseMessage = {
  status?: number;
  headers?: Record<string, string>;
  body?: string;
  bodyBase64?: string;
};

type Pending = {
  resolve: (value: ResponseMessage) => void;
  reject: (reason: Error) => void;
};

const pending = new Map<number, Pending>();
let nextID = 1;
let ready = false;
let markReady: () => void = () => {};
const readyPromise = new Promise<void>((resolve) => {
  markReady = resolve;
});

const workerURL = new URL("./kcp_wasm_worker.ts", import.meta.url);
const worker = new Worker(workerURL, { type: "module" });

worker.onmessage = (event: MessageEvent) => {
  const data = event.data ?? {};
  if (data.type === "ready") {
    ready = true;
    markReady();
    console.error("kcp-wasm: ready");
    return;
  }
  if (data.type === "persist") {
    schedulePersist(String(data.snapshot ?? ""));
    return;
  }
  if (typeof data.id !== "number") return;
  const entry = pending.get(data.id);
  if (!entry) return;
  pending.delete(data.id);
  if (data.error) {
    entry.reject(new Error(String(data.error)));
  } else {
    entry.resolve(data.response as ResponseMessage);
  }
};

worker.onerror = (event: ErrorEvent) => {
  console.error("kcp-wasm worker error:", event.message ?? event.error);
  for (const entry of pending.values()) {
    entry.reject(new Error(event.message ?? "worker error"));
  }
  pending.clear();
};

worker.postMessage({
  type: "init",
  wasmPath,
  execPath: wasmExecPath,
  execUrl: "file://" + encodeURI(wasmExecPath),
  initialState,
  socialweb: {
    origin: socialwebOrigin,
    identityMode: options.get("identity-mode") ?? "oauthBrowser",
    handle: options.get("handle") ?? "",
    did: options.get("did") ?? "",
    oauthClientId: `${socialwebOrigin}/oauth-client-metadata.json`,
    oauthRedirectUri: `${socialwebOrigin}/oauth/callback/requester`,
    forceApply: options.get("force-apply") === "true",
    statusFeed: options.get("status-feed") !== "false",
  },
});

let pendingSnapshot: string | null = null;
let writeInFlight = false;

async function writeLoop(): Promise<void> {
  if (writeInFlight) return;
  writeInFlight = true;
  try {
    while (pendingSnapshot !== null) {
      const data = pendingSnapshot;
      pendingSnapshot = null;
      const tmp = `${statePath}.tmp`;
      await Deno.writeTextFile(tmp, data);
      await Deno.rename(tmp, statePath);
    }
  } catch (err) {
    console.error(`kcp-wasm: failed to persist state to ${statePath}:`, err);
  } finally {
    writeInFlight = false;
  }
}

// Coalesces bursts: only the newest snapshot waiting in the queue is written.
function schedulePersist(snapshot: string): void {
  pendingSnapshot = snapshot;
  void writeLoop();
}

async function flushPersistence(): Promise<void> {
  while (writeInFlight || pendingSnapshot !== null) {
    await new Promise((r) => setTimeout(r, 10));
    if (!writeInFlight && pendingSnapshot !== null) void writeLoop();
  }
}

function rpc(request: unknown): Promise<ResponseMessage> {
  const id = nextID++;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    worker.postMessage({ id, request });
    setTimeout(() => {
      if (pending.delete(id)) {
        reject(new Error("kcp-wasm request timed out after 120s"));
      }
    }, 120_000);
  });
}

// /echo is served directly by Deno. It gives the Fetch controller a stable,
// local URL to fetch, and proves that the request really left the WASM module
// and was performed by the Deno host.
function echo(request: Request, url: URL): Response {
  const message = url.searchParams.get("msg") ?? "";
  // Single line on purpose: kubectl's table printer truncates multi-line
  // cells, and a single line keeps the whole body visible in
  // `kubectl get fetches`.
  const body = `fetched-by=deno method=${request.method} path=${url.pathname} msg=${message}`;
  if (verbose) console.error(`echo: ${message}`);
  return new Response(body, {
    status: 200,
    headers: { "content-type": "text/plain; charset=utf-8", "x-served-by": "deno-echo" },
  });
}

console.error(
  `kcp-wasm: serving on http://${hostname}:${port} (wasm=${wasmPath}, state=${statePath})`,
);

const server = Deno.serve({ hostname, port }, async (request) => {
  const url = new URL(request.url);

  // The PDS fetches the client metadata document from the public origin, so it
  // must not wait for kcp readiness and must use the tunnel origin when set.
  if (url.pathname === METADATA_PATH) {
    const metadata = buildClientMetadata(
      request.headers.get("x-forwarded-proto") && request.headers.get("host")
        ? `${request.headers.get("x-forwarded-proto")}://${request.headers.get("host")}`
        : socialwebOrigin,
      DEFAULT_SCOPE,
    );
    return new Response(`${JSON.stringify(metadata, null, 2)}\n`, {
      headers: { "cache-control": "no-cache", "content-type": "application/json" },
    });
  }

  // The OAuth popup only relays code/state/iss back to the opener. Keeping this
  // static means sign-in works while the Requester component is still composing.
  if (url.pathname.startsWith("/oauth/callback/")) {
    const page = await Deno.readTextFile(
      new URL("./browser/oauth-callback.html", import.meta.url),
    );
    return new Response(page, {
      headers: { "cache-control": "no-cache", "content-type": "text/html; charset=utf-8" },
    });
  }

  if (!ready) {
    await Promise.race([readyPromise, new Promise((r) => setTimeout(r, 30_000))]);
    if (!ready) {
      return new Response("kcp-wasm is starting\n", { status: 503 });
    }
  }

  if (url.pathname === "/echo" || url.pathname.startsWith("/echo/")) {
    return echo(request, url);
  }

  const headers: Record<string, string> = {};
  request.headers.forEach((value, key) => {
    headers[key] = value;
  });

  const body = request.method === "GET" || request.method === "HEAD"
    ? ""
    : await request.text();

  try {
    const response = await rpc({
      method: request.method,
      path: url.pathname + url.search,
      headers,
      body,
    });
    // Deno's BodyInit type rejects the generic Uint8Array<ArrayBufferLike>
    // returned here even though it is a valid body at runtime.
    const responseBody = (response.bodyBase64
      ? decodeBase64(response.bodyBase64)
      : (response.body ?? "")) as unknown as BodyInit;
    const status = response.status ?? 500;
    return new Response(status === 204 || status === 304 ? null : responseBody, {
      status,
      headers: response.headers ?? {},
    });
  } catch (err) {
    console.error("kcp-wasm request failed:", err);
    return new Response(
      JSON.stringify({
        kind: "Status",
        apiVersion: "v1",
        status: "Failure",
        message: err instanceof Error ? err.message : String(err),
        code: 500,
      }),
      { status: 500, headers: { "content-type": "application/json" } },
    );
  }
});

// Flush the latest snapshot before exiting on a signal.
for (const signal of ["SIGINT", "SIGTERM"] as const) {
  try {
    Deno.addSignalListener(signal, () => {
      flushPersistence().finally(() => Deno.exit(0));
    });
  } catch (err) {
    if (verbose) {
      console.error(`kcp-wasm: cannot listen for ${signal}:`, err);
    }
  }
}

await server.finished;

function decodeBase64(encoded: string): Uint8Array {
  const binary = atob(encoded);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes;
}
