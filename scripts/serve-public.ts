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
 * Static server for the ./public browser bundle, plus the socialweb OAuth
 * client metadata document. The metadata is generated from the request Host
 * header (or --origin) so a tunnel or reverse proxy gets the right client_id
 * without rebuilding the bundle.
 *
 *   deno run --allow-read --allow-net scripts/serve-public.ts \
 *     --dir=public --port=8090 [--origin=example.com] [--scope="atproto ..."]
 *
 * Flags:
 *   --dir         directory to serve (default: public)
 *   --port        listen port (default: 8090)
 *   --origin      origin for metadata instead of the request Host header
 *   --scope       OAuth scope string (default: atproto transition:generic)
 *   --scope-file  file containing the OAuth scope string
 */

// Must match atprotooauth.ScopeString(atprotooauth.RequesterOAuthScope) in
// linux-kcp. The PDS rejects a token whose scope exceeds the client metadata.
export const REQUESTER_SCOPE = [
  "atproto",
  "repo:com.publicdomainrelay.temp.compute.vm?action=create",
  "repo:com.publicdomainrelay.temp.market.rfp?action=create",
  "repo:com.publicdomainrelay.temp.market.accept?action=create",
  "repo:com.publicdomainrelay.temp.market.event?action=create",
  "repo:com.publicdomainrelay.temp.compute.events.vm.delete?action=create",
  "repo:com.publicdomainrelay.temp.compute.events.vm.onNetwork?action=create",
  "repo:com.fedproxy.rbac?action=create&action=update&action=delete",
  "repo:computer.socialweb.temp.policy.ghalite?action=create",
  "repo:computer.socialweb.temp.policy.typescript?action=create",
  "repo:com.publicdomainrelay.temp.badgeBlueKeys?action=create&action=update&action=delete",
  "rpc:com.publicdomainrelay.temp.market.submitRfp?aud=*",
  "rpc:com.publicdomainrelay.temp.market.submitAccept?aud=*",
  "rpc:com.publicdomainrelay.temp.market.submitBid?aud=*",
  "rpc:com.publicdomainrelay.temp.market.submitEvent?aud=*",
].join(" ");

export const DEFAULT_SCOPE = REQUESTER_SCOPE;
export const DEFAULT_PORT = 8090;
export const METADATA_PATH = "/oauth-client-metadata.json";
export const CALLBACK_PATH = "/oauth/callback/requester";

const SPA_PATHS = new Set(["/", "/app.html", "/index.html"]);

const CONTENT_TYPES: Record<string, string> = {
  ".css": "text/css; charset=utf-8",
  ".gz": "application/gzip",
  ".html": "text/html; charset=utf-8",
  ".ico": "image/x-icon",
  ".js": "text/javascript; charset=utf-8",
  ".json": "application/json",
  ".map": "application/json",
  ".png": "image/png",
  ".svg": "image/svg+xml",
  ".txt": "text/plain; charset=utf-8",
  ".wasm": "application/wasm",
  ".webmanifest": "application/manifest+json",
  ".woff2": "font/woff2",
  ".yaml": "application/yaml; charset=utf-8",
  ".yml": "application/yaml; charset=utf-8",
};

export type ServerOptions = {
  root: string;
  originOverride?: string;
  scope: string;
};

/** Turn "example.com", "https://Example.COM/" or a Host header into an origin. */
export function normalizeOrigin(value: string): string {
  const trimmed = value.trim().replace(/\/+$/, "");
  if (!trimmed) {
    throw new Error("empty origin");
  }
  const url = new URL(trimmed.includes("://") ? trimmed : `https://${trimmed}`);
  if (url.protocol !== "https:" && url.protocol !== "http:") {
    throw new Error(`unsupported origin protocol: ${url.protocol}`);
  }
  return url.origin;
}

/** The origin used in client metadata: --origin wins, then the Host header. */
export function requestOrigin(
  request: Request,
  originOverride?: string,
): string {
  if (originOverride) {
    return normalizeOrigin(originOverride);
  }
  const host = request.headers.get("host");
  if (!host) {
    throw new Error("request has no Host header; pass --origin");
  }
  return normalizeOrigin(host);
}

/** The public OAuth client metadata document for an origin. */
export function buildClientMetadata(
  origin: string,
  scope: string,
): Record<string, unknown> {
  const base = normalizeOrigin(origin);
  return {
    client_id: `${base}${METADATA_PATH}`,
    redirect_uris: [`${base}${CALLBACK_PATH}`],
    dpop_bound_access_tokens: true,
    application_type: "web",
    grant_types: ["authorization_code", "refresh_token"],
    response_types: ["code"],
    token_endpoint_auth_method: "none",
    client_name: "socialweb-computer requester",
    client_uri: base,
    scope,
  };
}

function contentTypeFor(filePath: string): string {
  if (filePath.endsWith("/config")) {
    return "application/json";
  }
  const dot = filePath.lastIndexOf(".");
  const slash = filePath.lastIndexOf("/");
  if (dot > slash) {
    const type = CONTENT_TYPES[filePath.slice(dot).toLowerCase()];
    if (type) {
      return type;
    }
  }
  return "application/octet-stream";
}

/** Strong validator from size and mtime; cheap for the 100+ MB modules. */
export function fileETag(stat: Deno.FileInfo): string {
  return `"${stat.size.toString(16)}-${(stat.mtime?.getTime() ?? 0).toString(16)}"`;
}

/** If-None-Match comparison (weak, so W/ prefixes from proxies still match). */
export function etagMatches(header: string | null, etag: string): boolean {
  if (!header) return false;
  return header.split(",").some((tag) => {
    const value = tag.trim();
    return value === "*" || value.replace(/^W\//, "") === etag.replace(/^W\//, "");
  });
}

/**
 * Map a URL path below root to a filesystem path. Returns null for paths
 * trying to escape the root directory.
 */
function resolveFile(root: string, pathname: string): string | null {
  let decoded: string;
  try {
    decoded = decodeURIComponent(pathname);
  } catch {
    return null;
  }
  if (decoded.includes("\0") || decoded.includes("\\")) {
    return null;
  }
  const parts = decoded.split("/").filter((part) => part !== "" && part !== ".");
  if (parts.some((part) => part === "..")) {
    return null;
  }
  return parts.length === 0 ? root : `${root}/${parts.join("/")}`;
}

async function serveFile(
  root: string,
  pathname: string,
  request: Request,
): Promise<Response | null> {
  const filePath = resolveFile(root, pathname);
  if (!filePath) {
    return null;
  }

  let stat: Deno.FileInfo;
  try {
    stat = await Deno.stat(filePath);
  } catch (error) {
    if (error instanceof Deno.errors.NotFound) {
      return null;
    }
    throw error;
  }
  if (!stat.isFile) {
    return null;
  }

  // Nothing in the bundle is content-hashed, so every file revalidates.
  const etag = fileETag(stat);
  const headers = new Headers({
    "cache-control": "no-cache",
    "content-type": contentTypeFor(filePath),
    etag,
  });
  if (etagMatches(request.headers.get("if-none-match"), etag)) {
    return new Response(null, { status: 304, headers });
  }
  headers.set("content-length", String(stat.size));
  if (request.method === "HEAD") {
    return new Response(null, { headers });
  }

  let file: Deno.FsFile;
  try {
    file = await Deno.open(filePath, { read: true });
  } catch (error) {
    if (error instanceof Deno.errors.NotFound) {
      return null;
    }
    throw error;
  }
  return new Response(file.readable, { headers });
}

async function servePage(
  root: string,
  request: Request,
): Promise<Response | null> {
  for (const name of ["app.html", "index.html"]) {
    const response = await serveFile(root, `/${name}`, request);
    if (response) {
      return response;
    }
  }
  return null;
}

function notFound(): Response {
  return new Response("not found\n", {
    status: 404,
    headers: { "content-type": "text/plain; charset=utf-8" },
  });
}

async function handleRequest(
  request: Request,
  options: ServerOptions,
): Promise<Response> {
  if (request.method !== "GET" && request.method !== "HEAD") {
    return new Response("method not allowed\n", {
      status: 405,
      headers: { allow: "GET, HEAD", "content-type": "text/plain; charset=utf-8" },
    });
  }

  const url = new URL(request.url);

  if (url.pathname === METADATA_PATH) {
    try {
      const metadata = buildClientMetadata(
        requestOrigin(request, options.originOverride),
        options.scope,
      );
      return new Response(`${JSON.stringify(metadata, null, 2)}\n`, {
        headers: {
          "cache-control": "no-cache",
          "content-type": "application/json",
        },
      });
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      return new Response(`invalid origin: ${message}\n`, {
        status: 400,
        headers: { "content-type": "text/plain; charset=utf-8" },
      });
    }
  }

  if (url.pathname === "/config") {
    return (await serveFile(options.root, "/config", request)) ??
      Response.json(
        { apiServer: "http://kcp.local", intervalMs: 1500 },
        { headers: { "cache-control": "no-cache" } },
      );
  }

  if (url.pathname.startsWith("/oauth/callback/")) {
    return (await serveFile(options.root, "/oauth-callback.html", request)) ??
      notFound();
  }

  if (SPA_PATHS.has(url.pathname)) {
    return (await servePage(options.root, request)) ?? notFound();
  }

  return (await serveFile(options.root, url.pathname, request)) ?? notFound();
}

async function main(): Promise<void> {
  const flags = new Map<string, string>();
  for (const arg of Deno.args) {
    const match = arg.match(/^--([^=]+)=(.*)$/);
    if (!match) {
      throw new Error(`unrecognized argument: ${arg}`);
    }
    flags.set(match[1], match[2]);
  }

  const root = Deno.realPathSync(flags.get("dir") ?? "public");
  if (!Deno.statSync(root).isDirectory) {
    throw new Error(`not a directory: ${root}`);
  }

  const port = Number(flags.get("port") ?? DEFAULT_PORT);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error(`invalid port: ${flags.get("port")}`);
  }

  const originOverride = flags.get("origin");
  if (originOverride !== undefined) {
    normalizeOrigin(originOverride);
  }

  let scope = flags.get("scope");
  if (scope === undefined) {
    const scopeFile = flags.get("scope-file");
    if (scopeFile === undefined) {
      scope = DEFAULT_SCOPE;
    } else {
      scope = (await Deno.readTextFile(scopeFile)).trim().split(/\s+/).join(" ");
      if (!scope) {
        throw new Error(`scope file is empty: ${scopeFile}`);
      }
    }
  }

  Deno.serve(
    { hostname: "127.0.0.1", port },
    (request) => handleRequest(request, { root, originOverride, scope }),
  );
  console.error(`public: http://127.0.0.1:${port} (dir ${root})`);
}

if (import.meta.main) {
  main().catch((error) => {
    console.error(
      `serve-public: ${error instanceof Error ? error.message : error}`,
    );
    Deno.exit(1);
  });
}
