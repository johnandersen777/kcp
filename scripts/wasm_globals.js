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

// Rewrites a Go js/wasm module so its mutable globals (SP, g, PAUSE, ...) are
// exported as "__snap_g<index>". Only the export section changes; no index is
// renumbered. Snapshot/restore of a paused Go instance needs those globals
// (scripts/browser/wasm_snapshot.js), so the build rewrites every kcp module:
//
//   deno run --allow-read --allow-write scripts/wasm_globals.js <in.wasm> [out.wasm]
//
// The rewrite is idempotent; out defaults to rewriting in place.

function readU32(buf, pos) {
  let result = 0, shift = 0, b;
  do {
    b = buf[pos++];
    result |= (b & 0x7f) << shift;
    shift += 7;
  } while (b & 0x80);
  return [result >>> 0, pos];
}

function u32(n) {
  const out = [];
  do {
    let b = n & 0x7f;
    n >>>= 7;
    if (n) b |= 0x80;
    out.push(b);
  } while (n);
  return out;
}

function skipInitExpr(buf, pos) {
  // Go only emits i32/i64 const init expressions; scan to the end opcode.
  while (buf[pos] !== 0x0b) pos++;
  return pos + 1;
}

function sections(buf) {
  const out = [];
  let pos = 8;
  while (pos < buf.length) {
    const id = buf[pos];
    const [size, body] = readU32(buf, pos + 1);
    out.push({ id, start: pos, body, end: body + size });
    pos = body + size;
  }
  return out;
}

export function listGlobals(buf) {
  const secs = sections(buf);
  let imported = 0;
  const imp = secs.find((s) => s.id === 2);
  if (imp) {
    let [n, p] = readU32(buf, imp.body);
    for (let i = 0; i < n; i++) {
      let len;
      [len, p] = readU32(buf, p); p += len;
      [len, p] = readU32(buf, p); p += len;
      const kind = buf[p++];
      if (kind === 0) [, p] = readU32(buf, p);
      else if (kind === 1) { p++; const f = buf[p++]; [, p] = readU32(buf, p); if (f & 1) [, p] = readU32(buf, p); }
      else if (kind === 2) { const f = buf[p++]; [, p] = readU32(buf, p); if (f & 1) [, p] = readU32(buf, p); }
      else if (kind === 3) { p += 2; imported++; }
    }
  }
  const globals = [];
  const gs = secs.find((s) => s.id === 6);
  if (gs) {
    let [n, p] = readU32(buf, gs.body);
    for (let i = 0; i < n; i++) {
      const type = buf[p++];
      const mutable = buf[p++] === 1;
      p = skipInitExpr(buf, p);
      globals.push({ index: imported + i, type, mutable });
    }
  }
  return globals;
}

export function exportMutableGlobals(input) {
  const buf = input instanceof Uint8Array ? input : new Uint8Array(input);
  const secs = sections(buf);
  const exp = secs.find((s) => s.id === 7);
  if (!exp) throw new Error("wasm has no export section");
  let [count, p] = readU32(buf, exp.body);
  const enc = new TextEncoder();
  const existing = [];
  for (let i = 0; i < count; i++) {
    const [len, q] = readU32(buf, p);
    existing.push(new TextDecoder().decode(buf.subarray(q, q + len)));
    p = q + len + 1;
    [, p] = readU32(buf, p);
  }
  if (existing.some((n) => n.startsWith("__snap_g"))) return buf;
  const extra = [];
  const mutable = listGlobals(buf).filter((g) => g.mutable);
  for (const g of mutable) {
    const name = enc.encode(`__snap_g${g.index}`);
    extra.push(...u32(name.length), ...name, 0x03, ...u32(g.index));
  }
  const entries = buf.subarray(readU32(buf, exp.body)[1], exp.end);
  const head = u32(count + mutable.length);
  const body = new Uint8Array(head.length + entries.length + extra.length);
  body.set(head, 0);
  body.set(entries, head.length);
  body.set(extra, head.length + entries.length);
  const secHead = [7, ...u32(body.length)];
  const out = new Uint8Array(buf.length - (exp.end - exp.start) + secHead.length + body.length);
  out.set(buf.subarray(0, exp.start), 0);
  out.set(secHead, exp.start);
  out.set(body, exp.start + secHead.length);
  out.set(buf.subarray(exp.end), exp.start + secHead.length + body.length);
  return out;
}

if (import.meta.main) {
  const [input, output = input] = Deno.args;
  const buf = await Deno.readFile(input);
  const out = exportMutableGlobals(buf);
  await WebAssembly.compile(out);
  if (out !== buf || output !== input) await Deno.writeFile(output, out);
  const n = listGlobals(out).filter((g) => g.mutable).length;
  console.log(`${output}: ${n} mutable globals exported (+${out.length - buf.length} bytes)`);
}
