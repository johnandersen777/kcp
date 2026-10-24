// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import { fetchWasm } from "./wasm_fetch.js";

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
