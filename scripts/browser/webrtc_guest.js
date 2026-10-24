// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// Plan 0034 browser requester: SSH to the guest over a WebRTC datachannel.
//
//   guest publicKey + onNetwork {kind webrtc, address "sha-256 AB:.."} + hostKeys
//   page: typage X25519 identity (memory only) -> outbound publicKey
//   page: RTCPeerConnection (Cloudflare STUN only, non-trickle) -> offer sealed
//         to the guest key -> outbound signalWebRTC offer
//   guest: answer sealed to our key -> inbound signalWebRTC answer
//   page: open, check a=fingerprint against the onNetwork address,
//         setRemoteDescription; the datachannel carries raw TCP to sshd :22
//   ssh_worker.js: x/crypto/ssh (ssh.wasm) over the datachannel, xterm.js
//
// Nothing secret leaves memory: the age identity is a closure variable, the
// SSH key is a non-extractable WebCrypto Ed25519 CryptoKey, and SDPs are only
// ever handled as plaintext between seal/open and the RTCPeerConnection.

export const NSID_PUBLIC_KEY = "com.publicdomainrelay.temp.compute.events.guest.publicKey";
export const NSID_SIGNAL = "com.publicdomainrelay.temp.compute.events.guest.signalWebRTC";
export const ICE_SERVERS = [{ urls: "stun:stun.cloudflare.com:3478" }];
export const SSH_TAG = "ssh";
export const SSH_PORT = 22;
export const SSH_USER = "root";
const AGE_URL = "https://esm.sh/age-encryption@0.3.1";

let agePromise;
const loadAge = () => (agePromise ??= import(AGE_URL));

/** An in-memory age X25519 identity: seal to `recipient`, open with `open`. */
export async function newAgeIdentity() {
  const age = await loadAge();
  const identity = await age.generateX25519Identity();
  const recipient = await age.identityToRecipient(identity);
  return {
    recipient,
    async open(armored) {
      const d = new age.Decrypter();
      d.addIdentity(identity);
      return await d.decrypt(age.armor.decode(armored), "text");
    },
  };
}

export async function seal(text, recipient) {
  const age = await loadAge();
  const e = new age.Encrypter();
  e.addRecipient(recipient);
  return age.armor.encode(await e.encrypt(text));
}

const normFingerprint = (s) => {
  const [algo, value] = String(s ?? "").trim().split(/\s+/);
  return algo && value ? `${algo.toLowerCase()} ${value.toUpperCase()}` : "";
};

/** Every a=fingerprint in the SDP, normalized "sha-256 AB:CD:..". */
export function sdpFingerprints(sdp) {
  return [...String(sdp).matchAll(/^a=fingerprint:(.+)$/gm)].map((m) => normFingerprint(m[1]));
}

/** True when the SDP has fingerprints and all of them are the expected one. */
export function fingerprintMatches(sdp, expected) {
  const want = normFingerprint(expected);
  const got = sdpFingerprints(sdp);
  return want !== "" && got.length > 0 && got.every((f) => f === want);
}

function base64(bytes) {
  let s = "";
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s);
}

/** "ssh-ed25519 AAAA..." for 32 raw public key bytes. */
export function authorizedKey(raw, comment = "") {
  const type = new TextEncoder().encode("ssh-ed25519");
  const blob = new Uint8Array(4 + type.length + 4 + raw.length);
  const view = new DataView(blob.buffer);
  view.setUint32(0, type.length);
  blob.set(type, 4);
  view.setUint32(4 + type.length, raw.length);
  blob.set(raw, 8 + type.length);
  return `ssh-ed25519 ${base64(blob)}${comment ? ` ${comment}` : ""}`;
}

/** A non-extractable Ed25519 key pair plus its public half in both forms. */
export async function newSSHKey() {
  const pair = await crypto.subtle.generateKey({ name: "Ed25519" }, false, ["sign", "verify"]);
  const raw = new Uint8Array(await crypto.subtle.exportKey("raw", pair.publicKey));
  return { privateKey: pair.privateKey, raw, authorized: authorizedKey(raw, "kcp-wasm") };
}

// The SSH key per contract survives a reload as a non-extractable CryptoKey
// in IndexedDB; the bytes of the private half are never readable.
function keyStore(mode, fn) {
  return new Promise((resolve, reject) => {
    const open = indexedDB.open("socialweb-vmshell", 1);
    open.onupgradeneeded = () => open.result.createObjectStore("sshKeys");
    open.onerror = () => reject(open.error);
    open.onsuccess = () => {
      const tx = open.result.transaction("sshKeys", mode);
      const req = fn(tx.objectStore("sshKeys"));
      tx.oncomplete = () => resolve(req.result);
      tx.onerror = () => reject(tx.error);
    };
  });
}
export const saveSSHKey = (name, key) => keyStore("readwrite", (s) => s.put(key, name));
export const loadSSHKey = (name) => keyStore("readonly", (s) => s.get(name));
export const deleteSSHKey = (name) => keyStore("readwrite", (s) => s.delete(name));

export function contractManifest({ name, bidders, ownerDid, sshAuthorizedKey }) {
  const spec = {
    vmName: name,
    bidderDids: bidders,
    sshAuthorizedKeys: [sshAuthorizedKey],
    tunnels: [{ tag: SSH_TAG, port: SSH_PORT, transport: "webrtc" }],
  };
  if (ownerDid) spec.ownerDid = ownerDid;
  return { apiVersion: "socialweb.computer/v1alpha1", kind: "ComputeContract", metadata: { name }, spec };
}

export function contractEventManifest({ name, contract, nsid, kind, tag = SSH_TAG, key, ciphertext }) {
  const spec = { contractRef: { name: contract }, direction: "outbound", nsid, kind, tag };
  if (key) spec.key = key;
  if (ciphertext) spec.ciphertext = ciphertext;
  return { apiVersion: "socialweb.computer/v1alpha1", kind: "ContractEvent", metadata: { name }, spec };
}

/**
 * The guest material for a tag from a status-feed snapshot, or null while any
 * part is missing: the latest inbound publicKey, the onNetwork webrtc
 * fingerprint and the pinned host keys.
 */
export function guestFromState(state, contract, tag = SSH_TAG) {
  const c = (state?.contracts ?? []).find((x) => x.name === contract);
  const network = (c?.networks ?? []).find((n) => n.tag === tag && n.kind === "webrtc");
  const key = inboundEvents(state, contract, NSID_PUBLIC_KEY, tag).at(-1)?.key;
  if (!network?.address || !key || !(c?.hostKeys?.length > 0)) return null;
  return { key, fingerprint: network.address, hostKeys: c.hostKeys };
}

/** Inbound ContractEvents for a contract, oldest first. */
export function inboundEvents(state, contract, nsid, tag = SSH_TAG, kind) {
  return (state?.contractEvents ?? [])
    .filter((e) => e.contract === contract && e.direction === "inbound" && e.nsid === nsid && e.tag === tag &&
      (kind === undefined || e.kind === kind))
    .sort((a, b) => String(a.createdAt).localeCompare(String(b.createdAt)) || a.name.localeCompare(b.name));
}

function gatheringComplete(pc, timeoutMs = 15_000) {
  if (pc.iceGatheringState === "complete") return Promise.resolve();
  return new Promise((resolve) => {
    const timer = setTimeout(done, timeoutMs);
    function done() {
      clearTimeout(timer);
      pc.removeEventListener("icegatheringstatechange", check);
      resolve();
    }
    function check() {
      if (pc.iceGatheringState === "complete") done();
    }
    pc.addEventListener("icegatheringstatechange", check);
  });
}

/**
 * Runs one SSH session over WebRTC. signaling: {guest(), publishKey(recipient),
 * exchange(offerCiphertext) -> answerCiphertext}. Returns {close()}.
 */
export async function connectShell({ signaling, sshKey, user = SSH_USER, term, status, onClose, tag = SSH_TAG }) {
  status("waiting for the guest's publicKey and fingerprint");
  const guest = await signaling.guest();
  const me = await newAgeIdentity();
  await signaling.publishKey(me.recipient);

  const pc = new RTCPeerConnection({ iceServers: ICE_SERVERS });
  const dc = pc.createDataChannel(tag);
  dc.binaryType = "arraybuffer";
  await pc.setLocalDescription(await pc.createOffer());
  status("gathering ICE candidates");
  await gatheringComplete(pc);
  status("offer sealed; waiting for the answer");
  const answerSealed = await signaling.exchange(await seal(pc.localDescription.sdp, guest.key));
  const answer = await me.open(answerSealed);
  if (!fingerprintMatches(answer, guest.fingerprint)) {
    pc.close();
    throw new Error("answer a=fingerprint does not match the guest's onNetwork address");
  }
  await pc.setRemoteDescription({ type: "answer", sdp: answer });
  status("connecting datachannel");
  await new Promise((resolve, reject) => {
    dc.onopen = resolve;
    pc.onconnectionstatechange = () => {
      if (pc.connectionState === "failed") reject(new Error("WebRTC connection failed (symmetric NAT?)"));
    };
  });

  const worker = new Worker(new URL("./ssh_worker.js", import.meta.url), { type: "module" });
  const { port1, port2 } = new MessageChannel();
  dc.onmessage = (e) => port1.postMessage(e.data, [e.data]);
  port1.onmessage = (e) => dc.readyState === "open" && dc.send(e.data);
  let closed = false;
  const close = () => {
    if (closed) return;
    closed = true;
    worker.terminate();
    dc.close();
    pc.close();
    for (const d of disposers) d.dispose();
    onClose?.();
  };
  dc.onclose = () => worker.postMessage({ type: "eof" });
  const disposers = [
    term.onData((data) => worker.postMessage({ type: "input", data: new TextEncoder().encode(data) })),
    term.onResize(({ cols, rows }) => worker.postMessage({ type: "resize", cols, rows })),
  ];
  worker.onmessage = (e) => {
    const m = e.data;
    if (m.type === "output") term.write(m.data);
    else if (m.type === "status") status(m.text);
    else if (m.type === "exit") {
      status(m.error ? `ssh: ${m.error}` : "ssh session ended");
      close();
    } else if (m.type === "log") console[m.level === "error" ? "error" : "log"](m.message);
  };
  worker.postMessage({
    type: "start",
    execUrl: new URL("/wasm_exec.js", location.href).href,
    wasmUrl: new URL("/ssh.wasm", location.href).href,
    port: port2,
    key: sshKey.privateKey,
    cfg: { user, publicKey: sshKey.raw, hostKeys: guest.hostKeys, cols: term.cols, rows: term.rows },
  }, [port2]);
  return { close };
}

/** Test signaling against contrib/wasm/sshclient/testpeer over loopback HTTP. */
export function httpSignaling(base, sshAuthorizedKey) {
  const call = async (path, body) => {
    const res = await fetch(new URL(path, base), body === undefined ? {} : {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!res.ok) throw new Error(`${path}: HTTP ${res.status} ${await res.text()}`);
    return await res.json();
  };
  return {
    guest: () => call("/guest"),
    publishKey: (key) => call("/publicKey", { key, sshAuthorizedKey }),
    exchange: async (ciphertext) => (await call("/offer", { ciphertext })).ciphertext,
  };
}

/**
 * Signaling through ContractEvents in the browser kcp. create(manifest) runs
 * kubectl create; watch(fn) calls fn with each status-feed snapshot until fn
 * returns a value, and resolves with it.
 */
export function contractEventSignaling({ contract, create, watch, tag = SSH_TAG }) {
  const suffix = () => Math.random().toString(36).slice(2, 8);
  return {
    guest: () => watch((state) => guestFromState(state, contract, tag)),
    publishKey: (key) => create(contractEventManifest({
      name: `${contract}-pk-${suffix()}`, contract, nsid: NSID_PUBLIC_KEY, kind: "age-x25519", tag, key,
    })),
    async exchange(ciphertext) {
      let seen;
      await watch((state) => {
        seen = new Set(inboundEvents(state, contract, NSID_SIGNAL, tag, "answer").map((e) => e.name));
        return true;
      });
      await create(contractEventManifest({
        name: `${contract}-offer-${suffix()}`, contract, nsid: NSID_SIGNAL, kind: "offer", tag, ciphertext,
      }));
      return await watch((state) =>
        inboundEvents(state, contract, NSID_SIGNAL, tag, "answer").find((e) => !seen.has(e.name))?.ciphertext);
    },
  };
}
