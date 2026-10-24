// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// ssh_worker.js runs one `ssh` invocation (ssh.wasm, contrib/wasm/sshclient)
// in a Web Worker. Go asks for two things through the page: resolve(host)
// (known_hosts text, the contract's public key, and the non-extractable
// CryptoKey that signs) and dial(host, port) (a MessagePort carrying the
// WebRTC datachannel's raw bytes; the page owns the RTCPeerConnection because
// workers lack it on some browsers).
//
//   page -> {type:"run", execUrl, wasmUrl, cfg} | input | stdinEOF | resize | eof | reply
//   page <- {type:"resolve"|"dial", id, ...} | addKnownHost | stdout | stderr | raw | exit

import { fetchWasm } from "./wasm_fetch.js";

const pending = new Map();
let nextID = 1;
let handle = null;
let key = null;
let onEof = null;
const queued = [];

function ask(message, transfer = []) {
  const id = nextID++;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    self.postMessage({ ...message, id }, transfer);
  });
}

self.onmessage = (event) => {
  const m = event.data ?? {};
  if (m.type === "run") return void run(m);
  if (m.type === "reply") {
    const p = pending.get(m.id);
    pending.delete(m.id);
    if (m.error) p?.reject(new Error(m.error));
    else p?.resolve(m);
    return;
  }
  if (m.type === "eof") return onEof?.();
  if (!handle) return queued.push(m);
  if (m.type === "input") handle.input(m.data);
  else if (m.type === "stdinEOF") handle.stdinEOF();
  else if (m.type === "resize") handle.resize(m.cols, m.rows);
};

async function run({ execUrl, wasmUrl, cfg }) {
  const post = (type) => (u8) => self.postMessage({ type, data: u8 }, [u8.buffer]);
  try {
    await import(execUrl);
    const go = new globalThis.Go();
    const [response] = await fetchWasm(wasmUrl);
    const { instance } = await WebAssembly.instantiateStreaming(response, go.importObject);
    go.run(instance);
    handle = globalThis.sshRun({
      ...cfg,
      resolve: async (host, port) => {
        const reply = await ask({ type: "resolve", host, port });
        key = reply.key;
        return { publicKey: reply.publicKey, knownHosts: reply.knownHosts };
      },
      dial: async (host, port, recv, eof, verbose) => {
        const reply = await ask({ type: "dial", host, port, verbose });
        reply.port.onmessage = (e) => recv(new Uint8Array(e.data));
        onEof = eof;
        return (u8) => reply.port.postMessage(u8.buffer, [u8.buffer]);
      },
      addKnownHost: (line) => self.postMessage({ type: "addKnownHost", line }),
      sign: async (data) => new Uint8Array(await crypto.subtle.sign("Ed25519", key, data)),
      stdout: post("stdout"),
      stderr: post("stderr"),
      raw: (on) => self.postMessage({ type: "raw", on }),
      exit: (code) => self.postMessage({ type: "exit", code }),
    });
    for (const m of queued.splice(0)) self.onmessage({ data: m });
  } catch (err) {
    self.postMessage({ type: "stderr", data: new TextEncoder().encode(`ssh: ${err?.message ?? err}\n`) });
    self.postMessage({ type: "exit", code: 255 });
  }
}
