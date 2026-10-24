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
 * Static file server for the browser-hosted demo. kcp.wasm and kubectl.wasm
 * both run in the browser (Web Workers); this process only serves the page,
 * the two modules, wasm_exec.js and the workers. It also provides /echo and
 * /demo.yaml so the kcp Fetch controller has a same-origin URL to fetch.
 *
 *   deno run --allow-read --allow-net \
 *     scripts/kubectl_wasm_web.ts \
 *     --kcp-wasm=bin/wasm/kcp.wasm \
 *     --kubectl-wasm=bin/wasm/kubectl.wasm \
 *     --wasm-exec=bin/wasm/wasm_exec.js \
 *     --port=8090
 */

import {
  buildClientMetadata,
  etagMatches,
  fileETag,
  loadScope,
  METADATA_PATH,
  normalizeOrigin,
  SCOPE_FILE,
} from "./serve-public.ts";

const options = new Map<string, string>();
for (const arg of Deno.args) {
  const match = arg.match(/^--([^=]+)=(.*)$/);
  if (match) options.set(match[1], match[2]);
}

const kcpWasmPath = Deno.realPathSync(
  options.get("kcp-wasm") ?? "bin/wasm/kcp.wasm",
);
const kubectlWasmPath = Deno.realPathSync(
  options.get("kubectl-wasm") ?? "bin/wasm/kubectl.wasm",
);
const goatWasmPath = Deno.realPathSync(
  options.get("goat-wasm") ?? "bin/wasm/goat.wasm",
);
const yqWasmPath = Deno.realPathSync(
  options.get("yq-wasm") ?? "bin/wasm/yq.wasm",
);
const wasmExecPath = Deno.realPathSync(
  options.get("wasm-exec") ?? "bin/wasm/wasm_exec.js",
);
const wasmExecCliPath = Deno.realPathSync(
  options.get("wasm-exec-cli") ?? "bin/wasm/wasm_exec_cli.js",
);
const hostname = options.get("host") ?? "127.0.0.1";
// --debug-log=<file> accepts the page's opt-in remote debug log (?debuglog=1)
// at POST /debug/log and appends it to <file>. Off by default; bodies are
// capped so the public origin cannot be used to fill the disk.
const debugLogPath = options.get("debug-log");
const debugLogMax = 256 * 1024;
const scope = await loadScope(
  options.get("scope-file") ?? kcpWasmPath.replace(/[^/]*$/, SCOPE_FILE),
  options.has("scope-file"),
);
const port = Number(options.get("port") ?? "8090");
const origin = options.get("origin");
const apiServer = options.get("api-server") ?? "http://kcp.local";
const intervalMs = Number(options.get("interval") ?? "1500");
const browserDir = new URL("./browser/", import.meta.url);

const assets = new Map<string, string>([
  ["/", "app.html"],
  ["/app.html", "app.html"],
  ["/app.js", "app.js"],
  ["/diagram.js", "diagram.js"],
  ["/kcp_worker.js", "kcp_worker.js"],
  ["/kubectl_worker.js", "kubectl_worker.js"],
  ["/cli_worker.js", "cli_worker.js"],
  ["/fs_shim.js", "fs_shim.js"],
  ["/oauth-callback.html", "oauth-callback.html"],
  ["/oauth.js", "oauth.js"],
  ["/state_store.js", "state_store.js"],
  ["/wasm_fetch.js", "wasm_fetch.js"],
  ["/ingress_ws.js", "ingress_ws.js"],
  ["/vmshell.js", "vmshell.js"],
  ["/wasm_snapshot.js", "wasm_snapshot.js"],
  ["/kcp_hibernate.js", "kcp_hibernate.js"],
]);

const wasmModules = new Map<string, string>([
  ["/kcp.wasm", kcpWasmPath],
  ["/kubectl.wasm", kubectlWasmPath],
  ["/goat.wasm", goatWasmPath],
  ["/yq.wasm", yqWasmPath],
]);

const headers = {
  "cache-control": "no-cache",
  "vary": "accept-encoding",
};

function acceptsGzip(request: Request): boolean {
  return (request.headers.get("accept-encoding") ?? "").includes("gzip");
}

// Serve a build-time `<file>.gz` sibling when it is not older than the source.
// The wasm modules are ~5x smaller compressed, which matters over a tunnel.
async function gzipSibling(path: string): Promise<Deno.FileInfo | null> {
  try {
    const source = await Deno.stat(path);
    const gz = await Deno.stat(`${path}.gz`);
    if (source.mtime && gz.mtime && gz.mtime.getTime() >= source.mtime.getTime()) {
      return gz;
    }
  } catch {
    // No precompressed sibling; serve the plain file.
  }
  return null;
}

async function fileResponse(
  request: Request,
  path: string | URL,
  contentType: string,
): Promise<Response> {
  if (typeof path === "string" && acceptsGzip(request)) {
    const gz = await gzipSibling(path);
    if (gz) {
      const etag = fileETag(gz).replace(/"$/, '-gz"');
      if (etagMatches(request.headers.get("if-none-match"), etag)) {
        return new Response(null, { status: 304, headers: { ...headers, etag } });
      }
      const file = await Deno.open(`${path}.gz`, { read: true });
      return new Response(file.readable, {
        headers: {
          ...headers,
          "content-type": contentType,
          "content-length": String(gz.size),
          "content-encoding": "gzip",
          etag,
        },
      });
    }
  }
  const file = await Deno.open(path, { read: true });
  const stat = await file.stat();
  const etag = fileETag(stat);
  if (etagMatches(request.headers.get("if-none-match"), etag)) {
    file.close();
    return new Response(null, { status: 304, headers: { ...headers, etag } });
  }
  return new Response(file.readable, {
    headers: {
      ...headers,
      "content-type": contentType,
      "content-length": String(stat.size),
      etag,
    },
  });
}

Deno.serve({ hostname, port }, async (request) => {
  const url = new URL(request.url);

  if (url.pathname === "/debug/log" && request.method === "POST") {
    if (!debugLogPath) return new Response("debug log disabled", { status: 404, headers });
    const text = await request.text();
    if (text.length > debugLogMax) return new Response("too large", { status: 413, headers });
    try {
      const { session, ua, lines } = JSON.parse(text);
      const tag = `${String(session).slice(0, 12)} ${String(ua).replace(/^.*?\(([^;)]+).*$/, "$1").slice(0, 40)}`;
      const out = (Array.isArray(lines) ? lines : []).slice(0, 500).map((l) =>
        `${l.t} [${tag}] ${String(l.level).slice(0, 8)} ${String(l.message).replace(/\s+/g, " ").slice(0, 2000)}`
      ).join("\n");
      if (out) await Deno.writeTextFile(debugLogPath, out + "\n", { append: true });
    } catch {
      return new Response("bad request", { status: 400, headers });
    }
    return new Response(null, { status: 204, headers });
  }

  if (url.pathname === "/config") {
    return Response.json(
      { apiServer, intervalMs },
      { headers },
    );
  }

  if (url.pathname === METADATA_PATH) {
    const forwarded = request.headers.get("x-forwarded-proto");
    const host = request.headers.get("host");
    const derived = forwarded && host ? `${forwarded}://${host}` : url.origin;
    const metadata = buildClientMetadata(
      origin ? normalizeOrigin(origin) : derived,
      scope,
    );
    return new Response(`${JSON.stringify(metadata, null, 2)}\n`, {
      headers: { ...headers, "content-type": "application/json" },
    });
  }

  if (url.pathname.startsWith("/oauth/callback/")) {
    return await fileResponse(
      request,
      new URL("oauth-callback.html", browserDir),
      "text/html; charset=utf-8",
    );
  }

  if (url.pathname === "/echo") {
    const message = url.searchParams.get("msg") ?? "";
    return new Response(
      `fetched-by=browser method=${request.method} path=${url.pathname} msg=${message}`,
      {
        headers: {
          ...headers,
          "content-type": "text/plain; charset=utf-8",
          "access-control-allow-origin": "*",
        },
      },
    );
  }

  if (url.pathname === "/demo.yaml") {
    const body = [
      "apiVersion: fetch.kcp.io/v1alpha1",
      "kind: Fetch",
      "metadata:",
      "  name: demo",
      "spec:",
      `  url: ${url.origin}/echo?msg=browser-kcp`,
      "",
    ].join("\n");
    return new Response(body, {
      headers: {
        ...headers,
        "content-type": "application/yaml; charset=utf-8",
        "access-control-allow-origin": "*",
      },
    });
  }

  const wasmPath = wasmModules.get(url.pathname);
  if (wasmPath) {
    return await fileResponse(request, wasmPath, "application/wasm");
  }
  // The workers try `<module>.wasm.gz` first and inflate it themselves, as on
  // static hosts; a 404 here sends them to the plain module above.
  const gzSource = wasmModules.get(url.pathname.replace(/\.gz$/, ""));
  if (gzSource && url.pathname.endsWith(".gz") && await gzipSibling(gzSource)) {
    return await fileResponse(request, `${gzSource}.gz`, "application/gzip");
  }
  if (url.pathname === "/wasm_exec.js") {
    return await fileResponse(request, wasmExecPath, "text/javascript; charset=utf-8");
  }
  if (url.pathname === "/wasm_exec_cli.js") {
    return await fileResponse(request, wasmExecCliPath, "text/javascript; charset=utf-8");
  }

  const asset = assets.get(url.pathname);
  if (asset) {
    const contentType = asset.endsWith(".js")
      ? "text/javascript; charset=utf-8"
      : "text/html; charset=utf-8";
    return await fileResponse(request, new URL(asset, browserDir), contentType);
  }

  return new Response("not found\n", { status: 404, headers });
});

console.error(
  `browser demo: http://${hostname}:${port} (all WASM in the browser; virtual API ${apiServer}; CLIs fetch directly)`,
);
