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

// fetchWasm returns a streaming application/wasm Response for
// WebAssembly.compileStreaming/instantiateStreaming, so the module bytes are
// never held in a JS ArrayBuffer.
//
// Static hosts (wisp.place) never gzip application/wasm and cannot be relied
// on for Content-Encoding, so the bundle ships `<module>.wasm.gz` as a plain
// file and it is inflated here. Without a .gz sibling (local dev servers) the
// raw module is fetched.
//
// The second return value is the number of bytes on the wire (Content-Length
// of whichever file was fetched), or 0 when unknown.
export async function fetchWasm(url, fetchFn = globalThis.fetch) {
  const gz = await fetchFn(`${url}.gz`);
  if (gz.ok && !/^text\/html/i.test(gz.headers.get("content-type") ?? "")) {
    const response = new Response(
      gz.body.pipeThrough(new DecompressionStream("gzip")),
      { headers: { "content-type": "application/wasm" } },
    );
    return [response, Number(gz.headers.get("content-length") ?? 0)];
  }
  await gz.body?.cancel();
  const raw = await fetchFn(url);
  if (!raw.ok) throw new Error(`${url}: HTTP ${raw.status}`);
  return [raw, Number(raw.headers.get("content-length") ?? 0)];
}

export const mib = (bytes) => (bytes / 1048576).toFixed(1);
