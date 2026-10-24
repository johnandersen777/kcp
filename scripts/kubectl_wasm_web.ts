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
const wasmExecPath = Deno.realPathSync(
  options.get("wasm-exec") ?? "bin/wasm/wasm_exec.js",
);
const hostname = options.get("host") ?? "127.0.0.1";
const port = Number(options.get("port") ?? "8090");
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
  ["/fs_shim.js", "fs_shim.js"],
]);

const headers = {
  "cache-control": "no-cache",
};

async function fileResponse(
  request: Request,
  path: string | URL,
  contentType: string,
): Promise<Response> {
  const file = await Deno.open(path, { read: true });
  const stat = await file.stat();
  const etag = `"${stat.size}-${stat.mtime?.getTime() ?? 0}"`;
  if (request.headers.get("if-none-match") === etag) {
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

  if (url.pathname === "/config") {
    return Response.json(
      { apiServer, intervalMs },
      { headers },
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

  if (url.pathname === "/kcp.wasm") {
    return await fileResponse(request, kcpWasmPath, "application/wasm");
  }
  if (url.pathname === "/kubectl.wasm") {
    return await fileResponse(request, kubectlWasmPath, "application/wasm");
  }
  if (url.pathname === "/wasm_exec.js") {
    return await fileResponse(request, wasmExecPath, "text/javascript; charset=utf-8");
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
  `browser demo: http://${hostname}:${port} (all WASM in the browser; virtual API ${apiServer})`,
);
