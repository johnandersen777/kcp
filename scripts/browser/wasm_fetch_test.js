// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import { fetchWasm, wasmBuildId } from "./wasm_fetch.js";

// Smallest valid module: magic + version.
const wasm = new Uint8Array([0, 0x61, 0x73, 0x6d, 1, 0, 0, 0]);

async function gzip(bytes) {
  const stream = new Blob([bytes]).stream().pipeThrough(new CompressionStream("gzip"));
  return new Uint8Array(await new Response(stream).arrayBuffer());
}

function fakeFetch(files) {
  const seen = [];
  const fn = (url) => {
    seen.push(url);
    const file = files[url];
    if (!file) return Promise.resolve(new Response("not found", { status: 404 }));
    return Promise.resolve(new Response(file.body, {
      headers: { "content-type": file.type, "content-length": String(file.body.length) },
    }));
  };
  return { fn, seen };
}

Deno.test("fetchWasm inflates the .gz sibling", async () => {
  const gz = await gzip(wasm);
  const { fn, seen } = fakeFetch({ "/m.wasm.gz": { body: gz, type: "application/gzip" } });
  const [response, bytes] = await fetchWasm("/m.wasm", fn);
  if (bytes !== gz.length) throw new Error(`bytes ${bytes}`);
  if (response.headers.get("content-type") !== "application/wasm") throw new Error("type");
  await WebAssembly.compileStreaming(response);
  if (seen.join() !== "/m.wasm.gz") throw new Error(seen.join());
});

Deno.test("fetchWasm falls back to the raw module", async () => {
  const { fn, seen } = fakeFetch({
    "/m.wasm.gz": { body: new TextEncoder().encode("<html>"), type: "text/html" },
    "/m.wasm": { body: wasm, type: "application/wasm" },
  });
  const [response] = await fetchWasm("/m.wasm", fn);
  await WebAssembly.compileStreaming(response);
  if (seen.join() !== "/m.wasm.gz,/m.wasm") throw new Error(seen.join());
});

Deno.test("fetchWasm hashes the wire bytes", async () => {
  const gz = await gzip(wasm);
  const want = Array.from(
    new Uint8Array(await crypto.subtle.digest("SHA-256", gz)),
    (x) => x.toString(16).padStart(2, "0"),
  ).join("");
  const { fn } = fakeFetch({ "/m.wasm.gz": { body: gz, type: "application/gzip" } });
  const [response, , digest] = await fetchWasm("/m.wasm", fn, { hash: true });
  await WebAssembly.compileStreaming(response);
  if (await digest !== want) throw new Error("gz digest");
  const raw = fakeFetch({ "/m.wasm": { body: wasm, type: "application/wasm" } });
  const [rawResponse, , rawDigest] = await fetchWasm("/m.wasm", raw.fn, { hash: true });
  await WebAssembly.compileStreaming(rawResponse);
  if ((await rawDigest).length !== 64) throw new Error("raw digest");
});

Deno.test("fetchWasm reports a missing module", async () => {
  const { fn } = fakeFetch({});
  let message = "";
  try {
    await fetchWasm("/m.wasm", fn);
  } catch (err) {
    message = err.message;
  }
  if (!message.includes("404")) throw new Error(`got ${message}`);
});

function headFetch(files) {
  const seen = [];
  const fn = (url, init) => {
    seen.push(`${init?.method ?? "GET"} ${url}`);
    const file = files[url];
    if (!file) return Promise.resolve(new Response("not found", { status: 404 }));
    return Promise.resolve(new Response(null, { headers: { "content-type": file.type, ...file.headers } }));
  };
  return { fn, seen };
}

Deno.test("wasmBuildId reads the .gz sibling's ETag (production ships only .gz)", async () => {
  const { fn, seen } = headFetch({ "/m.wasm.gz": { type: "application/gzip", headers: { etag: '"gz1"' } } });
  const id = await wasmBuildId("/m.wasm", fn);
  if (id !== '"gz1"') throw new Error(`id ${id}`);
  if (seen.join() !== "HEAD /m.wasm.gz") throw new Error(seen.join());
});

Deno.test("wasmBuildId falls back to the raw module (dev server)", async () => {
  const { fn, seen } = headFetch({
    "/m.wasm.gz": { type: "text/html", headers: { etag: '"spa"' } },
    "/m.wasm": { type: "application/wasm", headers: { etag: '"raw1"' } },
  });
  const id = await wasmBuildId("/m.wasm", fn);
  if (id !== '"raw1"') throw new Error(`id ${id}`);
  if (seen.join() !== "HEAD /m.wasm.gz,HEAD /m.wasm") throw new Error(seen.join());
});

Deno.test("wasmBuildId uses Last-Modified without an ETag", async () => {
  const { fn } = headFetch({ "/m.wasm.gz": { type: "application/gzip", headers: { "last-modified": "Fri, 30 Oct 2026 20:32:25 GMT" } } });
  const id = await wasmBuildId("/m.wasm", fn);
  if (id !== "Fri, 30 Oct 2026 20:32:25 GMT") throw new Error(`id ${id}`);
});

Deno.test("wasmBuildId answers empty when nothing identifies the build", async () => {
  const { fn } = headFetch({});
  if ((await wasmBuildId("/m.wasm", fn)) !== "") throw new Error("expected empty");
  const throwing = () => Promise.reject(new TypeError("offline"));
  if ((await wasmBuildId("/m.wasm", throwing)) !== "") throw new Error("expected empty on network error");
});
