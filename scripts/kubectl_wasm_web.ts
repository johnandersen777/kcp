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
// --debug-control=<file> lets the operator drive a page: every JSON line
// appended to <file> ({"session": "<id>" | "*", "id", "js"}) is streamed to the
// matching page at GET /debug/control?session=<id> (server-sent events); the
// page runs it and posts the result to /debug/log. Commands come only from the
// local file, never from a request. Off by default.
const debugControlPath = options.get("debug-control");

async function* controlLines(signal: AbortSignal): AsyncGenerator<string> {
  // Start at the beginning: the stream below replays only commands from the
  // last two minutes, so a page that was away (locked phone, reload) still
  // gets what was sent meanwhile; the page skips ids it already ran.
  let offset = 0;
  try {
    await Deno.stat(debugControlPath!);
  } catch {
    await Deno.writeTextFile(debugControlPath!, "", { append: true });
  }
  let partial = "";
  while (!signal.aborted) {
    const size = (await Deno.stat(debugControlPath!)).size;
    if (size < offset) offset = 0;
    if (size > offset) {
      const file = await Deno.open(debugControlPath!);
      await file.seek(offset, Deno.SeekMode.Start);
      const buf = new Uint8Array(size - offset);
      let read = 0;
      while (read < buf.length) {
        const n = await file.read(buf.subarray(read));
        if (n === null) break;
        read += n;
      }
      file.close();
      offset += read;
      const lines = (partial + new TextDecoder().decode(buf.subarray(0, read))).split("\n");
      partial = lines.pop() ?? "";
      for (const line of lines) if (line.trim()) yield line;
    }
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
}

function controlStream(request: Request, session: string): Response {
  const abort = new AbortController();
  request.signal.addEventListener("abort", () => abort.abort());
  const enc = new TextEncoder();
  const body = new ReadableStream({
    async start(controller) {
      controller.enqueue(enc.encode(`: control for ${session}\n\n`));
      const ping = setInterval(() => {
        try {
          controller.enqueue(enc.encode(": ping\n\n"));
        } catch {
          abort.abort();
        }
      }, 15_000);
      try {
        for await (const line of controlLines(abort.signal)) {
          let cmd: { session?: string; ts?: string };
          try {
            cmd = JSON.parse(line);
          } catch {
            continue;
          }
          if (cmd.session !== "*" && cmd.session !== session) continue;
          if (!cmd.ts || Date.now() - Date.parse(cmd.ts) > 120_000) continue;
          controller.enqueue(enc.encode(`data: ${JSON.stringify(cmd)}\n\n`));
        }
      } catch {
        // client went away
      } finally {
        clearInterval(ping);
        try {
          controller.close();
        } catch {
          // already closed
        }
      }
    },
    cancel() {
      abort.abort();
    },
  });
  return new Response(body, {
    headers: { "content-type": "text/event-stream", "cache-control": "no-cache" },
  });
}
const debugLogMax = 256 * 1024;
// The scope comes from requester-scope.txt next to kcp.wasm and is read per
// metadata request: a kcp.wasm deploy changes it, and a scope cached at
// startup made bsky reject the new build's sign-in (invalid_scope).
const scopeFile = options.get("scope-file") ?? kcpWasmPath.replace(/[^/]*$/, SCOPE_FILE);
await loadScope(scopeFile, options.has("scope-file"));
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
  ["/identity_ui.js", "identity_ui.js"],
  ["/state_store.js", "state_store.js"],
  ["/wasm_fetch.js", "wasm_fetch.js"],
  ["/ingress_ws.js", "ingress_ws.js"],
  ["/ingress_manager.js", "ingress_manager.js"],
  ["/vmshell.js", "vmshell.js"],
  ["/webrtc_guest.js", "webrtc_guest.js"],
  ["/ssh_worker.js", "ssh_worker.js"],
  ["/ssh_cmd.js", "ssh_cmd.js"],
  ["/wasm_snapshot.js", "wasm_snapshot.js"],
  ["/kcp_hibernate.js", "kcp_hibernate.js"],
]);

const wasmModules = new Map<string, string>([
  ["/kcp.wasm", kcpWasmPath],
  ["/kubectl.wasm", kubectlWasmPath],
  ["/goat.wasm", goatWasmPath],
  ["/yq.wasm", yqWasmPath],
]);
// Optional: without it, low-memory mode falls back to kubectl.wasm.
try {
  wasmModules.set(
    "/kubectl-slim.wasm",
    Deno.realPathSync(
      options.get("kubectl-slim-wasm") ??
        kubectlWasmPath.replace(/[^/]*$/, "kubectl-slim.wasm"),
    ),
  );
} catch {
  console.error("no kubectl-slim.wasm; low-memory mode loads kubectl.wasm");
}
// ssh.wasm (scripts/build-sshclient-wasm.sh) is optional: without it only
// the WebRTC shell is unavailable.
try {
  wasmModules.set("/ssh.wasm", Deno.realPathSync(options.get("ssh-wasm") ?? "bin/wasm/ssh.wasm"));
} catch {
  console.error("no ssh.wasm; the WebRTC VM shell is unavailable");
}

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

// The OAuth client_id is the --origin's metadata URL, and the authorization
// server rejects an authorization started from any other host name ("client_id
// does not match"). Page loads from another public host name of this server
// are sent to the origin; local access (loopback) is left alone.
function canonicalRedirect(request: Request, url: URL): Response | null {
  if (!origin || (request.method !== "GET" && request.method !== "HEAD")) return null;
  const host = request.headers.get("host") ?? url.host;
  const want = new URL(normalizeOrigin(origin));
  const bare = host.replace(/:\d+$/, "");
  if (host === want.host || bare === "localhost" || bare === "127.0.0.1" || bare === "[::1]") {
    return null;
  }
  return Response.redirect(new URL(url.pathname + url.search, want).toString(), 308);
}

Deno.serve({ hostname, port }, async (request) => {
  const url = new URL(request.url);
  const canonical = canonicalRedirect(request, url);
  if (canonical) return canonical;

  if (url.pathname === "/debug/control" && request.method === "GET") {
    if (!debugControlPath) return new Response("debug control disabled", { status: 404, headers });
    return controlStream(request, url.searchParams.get("session") ?? "");
  }

  if (url.pathname === "/debug/log" && request.method === "POST") {
    if (!debugLogPath) return new Response("debug log disabled", { status: 404, headers });
    const text = await request.text();
    if (text.length > debugLogMax) return new Response("too large", { status: 413, headers });
    try {
      const { session, ua, lines } = JSON.parse(text);
      const tag = `${String(session).slice(0, 12)} ${String(ua).replace(/^.*?\(([^;)]+).*$/, "$1").slice(0, 40)}`;
      const out = (Array.isArray(lines) ? lines : []).slice(0, 500).map((l) =>
        `${l.t} [${tag}] ${String(l.level).slice(0, 8)} ${String(l.message).replace(/\r?\n/g, "\\n").replace(/[\t ]+/g, " ").slice(0, 60_000)}`
      ).join("\n");
      if (out) await Deno.writeTextFile(debugLogPath, out + "\n", { append: true });
    } catch {
      return new Response("bad request", { status: 400, headers });
    }
    return new Response(null, { status: 204, headers });
  }

  // A Go heap profile from a debugged page (localStorage kcp.heapProfile),
  // saved next to the debug log as heap-<session>-<time>.pb.gz for go tool pprof.
  if (url.pathname === "/debug/heap" && request.method === "POST") {
    if (!debugLogPath) return new Response("debug log disabled", { status: 404, headers });
    const bytes = new Uint8Array(await request.arrayBuffer());
    if (bytes.length > 32 << 20) return new Response("too large", { status: 413, headers });
    const session = (url.searchParams.get("session") ?? "page").replace(/[^a-z0-9]/gi, "").slice(0, 12);
    const dir = debugLogPath.replace(/\/[^/]*$/, "");
    await Deno.writeFile(`${dir}/heap-${session}-${new Date().toISOString().replace(/[:.]/g, "-")}.pb.gz`, bytes);
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
      await loadScope(scopeFile, options.has("scope-file")),
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
