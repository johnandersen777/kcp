// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// Plan 0034 browser requester: SSH to the guest over a WebRTC datachannel.
//
//   guest publicKey + onNetwork {kind webrtc, address "sha-256 AB:.."} + hostKeys
//   page: typage X25519 identity (memory only) -> outbound publicKey
//   page: RTCPeerConnection (Cloudflare STUN only, non-trickle) -> {sdp,
//         replyTo: our recipient} sealed to the guest key -> outbound offer
//   guest: {sdp} sealed to replyTo -> inbound signalWebRTC answer
//   page: trial-open answers, check a=fingerprint against the onNetwork address,
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

/**
 * The first of the candidate ciphertexts that opens with our identity, or
 * undefined. Answers carry no recipient (the guest seals each to the offer's
 * replyTo), so every answer for the tag is trial-opened; typage throws for
 * one sealed to someone else.
 */
export async function firstOpened(ciphertexts, open) {
  for (const c of ciphertexts) {
    try {
      return await open(c);
    } catch { /* sealed to another identity */ }
  }
  return undefined;
}

/** Inbound ContractEvents for a contract, oldest first. */
export function inboundEvents(state, contract, nsid, tag = SSH_TAG, kind) {
  return (state?.contractEvents ?? [])
    .filter((e) => e.contract === contract && e.direction === "inbound" && e.nsid === nsid && e.tag === tag &&
      (kind === undefined || e.kind === kind))
    .sort((a, b) => String(a.createdAt).localeCompare(String(b.createdAt)) || a.name.localeCompare(b.name));
}

// Non-trickle: one offer with every candidate. Chrome can sit in "gathering"
// for tens of seconds after its srflx candidate arrives (an interface whose
// STUN never answers), so a quiet period after a srflx candidate also ends
// gathering. ponytail: fixed 500 ms settle; make it adaptive if candidates
// are seen arriving later on real networks.
function gatheringComplete(pc, timeoutMs = 15_000, settleMs = 500) {
  if (pc.iceGatheringState === "complete") return Promise.resolve();
  return new Promise((resolve) => {
    let settle;
    const timer = setTimeout(done, timeoutMs);
    function done() {
      clearTimeout(timer);
      clearTimeout(settle);
      pc.removeEventListener("icegatheringstatechange", check);
      pc.removeEventListener("icecandidate", candidate);
      resolve();
    }
    function check() {
      if (pc.iceGatheringState === "complete") done();
    }
    function candidate() {
      if (!/ typ srflx /.test(pc.localDescription?.sdp ?? "")) return;
      clearTimeout(settle);
      settle = setTimeout(done, settleMs);
    }
    pc.addEventListener("icegatheringstatechange", check);
    pc.addEventListener("icecandidate", candidate);
    candidate();
  });
}

/**
 * The kcp-webrtc proxy: opens one datachannel (label = tag) to the guest and
 * returns {port, close}. port is a MessagePort carrying the channel's raw TCP
 * bytes both ways (ArrayBuffers); onEof runs when the channel closes.
 * signaling: {guest(), publishKey(recipient), exchange(offerCiphertext, open)
 * -> the opened answer}. A missing answer or a channel that does not open in
 * time throws an error with connectivity: true (callers may fall back to
 * another shell); a fingerprint mismatch does not.
 */
export async function dialGuest({
  signaling, tag = SSH_TAG, status = () => {}, onEof = () => {}, answerTimeoutMs = 60_000, openTimeoutMs = 15_000,
}) {
  const within = (promise, ms, message) => {
    let timer;
    const timeout = new Promise((_, reject) => {
      timer = setTimeout(() => reject(Object.assign(new Error(message), { connectivity: true })), ms);
    });
    return Promise.race([promise, timeout]).finally(() => clearTimeout(timer));
  };
  status("waiting for the guest's publicKey and fingerprint");
  const guest = await signaling.guest();
  const me = await newAgeIdentity();
  await signaling.publishKey(me.recipient);

  const pc = new RTCPeerConnection({ iceServers: ICE_SERVERS });
  try {
    const dc = pc.createDataChannel(tag);
    dc.binaryType = "arraybuffer";
    await pc.setLocalDescription(await pc.createOffer());
    status("gathering ICE candidates");
    await gatheringComplete(pc);
    status("offer sealed; waiting for the answer");
    // Sealed bodies are JSON {sdp, replyTo}; replyTo (offers only) is the
    // recipient the guest seals its answer to.
    const offer = JSON.stringify({ sdp: pc.localDescription.sdp, replyTo: me.recipient });
    const opened = await within(signaling.exchange(await seal(offer, guest.key), (c) => me.open(c)),
      answerTimeoutMs, `no answer from the guest agent within ${answerTimeoutMs / 1000} s`);
    const answer = String(JSON.parse(opened)?.sdp ?? "");
    if (!fingerprintMatches(answer, guest.fingerprint)) {
      throw new Error("answer a=fingerprint does not match the guest's onNetwork address");
    }
    await pc.setRemoteDescription({ type: "answer", sdp: answer });
    status("connecting datachannel");
    await within(new Promise((resolve, reject) => {
      dc.onopen = resolve;
      pc.onconnectionstatechange = () => {
        if (pc.connectionState === "failed") {
          reject(Object.assign(new Error("WebRTC connection failed (symmetric NAT?)"), { connectivity: true }));
        }
      };
    }), openTimeoutMs, `the datachannel did not open within ${openTimeoutMs / 1000} s`);
    status("datachannel open");
    const { port1, port2 } = new MessageChannel();
    dc.onmessage = (e) => port1.postMessage(e.data, [e.data]);
    port1.onmessage = (e) => dc.readyState === "open" && dc.send(e.data);
    dc.onclose = () => onEof();
    return {
      port: port2,
      close() {
        port1.close();
        pc.close();
      },
    };
  } catch (err) {
    pc.close();
    throw err;
  }
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
    exchange: async (ciphertext, open) => await open((await call("/offer", { ciphertext })).ciphertext),
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
    // The generic handshake; the answer path does not depend on it.
    publishKey: (key) => create(contractEventManifest({
      name: `${contract}-pk-${suffix()}`, contract, nsid: NSID_PUBLIC_KEY, kind: "age-x25519", tag, key,
    })),
    async exchange(ciphertext, open) {
      await create(contractEventManifest({
        name: `${contract}-offer-${suffix()}`, contract, nsid: NSID_SIGNAL, kind: "offer", tag, ciphertext,
      }));
      const tried = new Set();
      for (;;) {
        const fresh = await watch((state) => {
          const list = inboundEvents(state, contract, NSID_SIGNAL, tag, "answer").filter((e) => !tried.has(e.name));
          return list.length > 0 && list;
        });
        for (const e of fresh) tried.add(e.name);
        const opened = await firstOpened(fresh.map((e) => e.ciphertext).filter(Boolean), open);
        if (opened !== undefined) return opened;
      }
    },
  };
}
