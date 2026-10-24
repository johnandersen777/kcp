// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// ssh_worker.js runs one x/crypto/ssh session (ssh.wasm, contrib/wasm/sshclient)
// in a Web Worker. The page owns the RTCPeerConnection (workers lack it on
// some browsers) and hands over a MessagePort that carries the datachannel's
// raw bytes both ways, plus the non-extractable Ed25519 CryptoKey that signs
// the SSH user auth.
//
//   page -> {type:"start", execUrl, wasmUrl, port, key, cfg} | input | resize | eof
//   page <- {type:"output", data} | status | exit | log

import { fetchWasm } from "./wasm_fetch.js";

const queued = [];
let session = null;
self.onmessage = (event) => {
  if (event.data?.type === "start") void start(event.data);
  else if (session) handle(event.data);
  else queued.push(event.data);
};

function handle(m) {
  if (m.type === "input") session.input(m.data);
  else if (m.type === "resize") session.resize(m.cols, m.rows);
  else if (m.type === "eof") session.eof();
}

async function start({ execUrl, wasmUrl, port, key, cfg }) {
  try {
    await import(execUrl);
    const go = new globalThis.Go();
    const [response] = await fetchWasm(wasmUrl);
    const { instance } = await WebAssembly.instantiateStreaming(response, go.importObject);
    go.run(instance);
    if (typeof globalThis.sshStart !== "function") throw new Error("ssh.wasm did not register sshStart");
    session = globalThis.sshStart({
      ...cfg,
      send: (u8) => port.postMessage(u8.buffer, [u8.buffer]),
      sign: async (data) => new Uint8Array(await crypto.subtle.sign("Ed25519", key, data)),
      output: (u8) => self.postMessage({ type: "output", data: u8 }, [u8.buffer]),
      status: (text) => self.postMessage({ type: "status", text }),
      exit: (error) => self.postMessage({ type: "exit", error }),
    });
    port.onmessage = (e) => session.recv(new Uint8Array(e.data));
    for (const m of queued.splice(0)) handle(m);
  } catch (err) {
    self.postMessage({ type: "exit", error: String(err?.message ?? err) });
  }
}
