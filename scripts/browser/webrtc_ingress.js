// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// webrtc_ingress.js is the browser half of the requester's WebRTC ingress.
// Go offers one RTCPeerConnection per (bidder, offer session) during
// submitRfp; the bidder answers in the same XRPC reply, then opens one
// datachannel per XRPC call (label "xrpc"). Each channel's bytes go to Go
// through __socialwebWebRTCData and Go's response bytes come back through
// send(); the service-auth token on every request is the authority, never the
// channel.
//
// The locally created "control" channel keeps the SCTP association in the
// offer; the answerer opens the request channels after the connection is up.
// Sessions are keyed by the per-offer nonce Go generates, so an overlapping
// offer for the same bidder can never close or answer the other one.

export const ICE_SERVERS = [{ urls: "stun:stun.cloudflare.com:3478" }];
const GATHER_TIMEOUT_MS = 10_000;

let iceServers = ICE_SERVERS;
const sessions = new Map();

export function setIceServers(urls) {
  if (!Array.isArray(urls) || urls.length === 0) return;
  iceServers = urls.map((url) => ({ urls: url }));
}

const call = (name, ...args) => {
  const fn = globalThis[`__socialwebWebRTC${name}`];
  if (typeof fn === "function") return fn(...args);
};

const toBase64 = (buffer) => {
  const bytes = new Uint8Array(buffer);
  let text = "";
  for (let i = 0; i < bytes.length; i += 1) text += String.fromCharCode(bytes[i]);
  return btoa(text);
};

const fromBase64 = (text) => {
  const raw = atob(text);
  const bytes = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i += 1) bytes[i] = raw.charCodeAt(i);
  return bytes;
};

export function supported() {
  return typeof RTCPeerConnection === "function";
}

function gathered(pc) {
  if (pc.iceGatheringState === "complete") return Promise.resolve();
  return new Promise((resolve) => {
    const done = () => {
      if (pc.iceGatheringState !== "complete") return;
      pc.removeEventListener("icegatheringstatechange", done);
      resolve();
    };
    pc.addEventListener("icegatheringstatechange", done);
    setTimeout(resolve, GATHER_TIMEOUT_MS);
  });
}

export function offer(session, peer, callback) {
  if (!supported()) {
    callback("RTCPeerConnection is not available in this worker");
    return;
  }
  close(session);
  let pc;
  let record;
  try {
    pc = new RTCPeerConnection({ iceServers });
    record = { session, peer, pc, channels: new Map(), seq: 0 };
    sessions.set(session, record);
    pc.onconnectionstatechange = () => {
      call("State", session, pc.connectionState, "");
      if (pc.connectionState === "failed" || pc.connectionState === "closed") close(session);
    };
    pc.ondatachannel = (event) => {
      const channel = event.channel;
      const id = String((record.seq += 1));
      channel.binaryType = "arraybuffer";
      record.channels.set(id, channel);
      channel.onopen = () => call("Open", session, id);
      channel.onmessage = (message) => call("Data", session, id, toBase64(message.data));
      channel.onclose = () => {
        record.channels.delete(id);
        call("Close", session, id);
      };
      channel.onerror = () => call("State", session, "channel-error", "");
      if (channel.readyState === "open") call("Open", session, id);
    };
    pc.createDataChannel("control", { ordered: true });
  } catch (err) {
    close(session);
    callback(err instanceof Error ? err.message : String(err));
    return;
  }
  pc.createOffer()
    .then((description) => pc.setLocalDescription(description))
    .then(() => gathered(pc))
    .then(() => {
      if (!pc.localDescription) throw new Error("no local description after gathering");
      callback(null, { type: "offer", sdp: pc.localDescription.sdp });
    })
    .catch((err) => {
      close(session);
      callback(err instanceof Error ? err.message : String(err));
    });
}

export function accept(session, sdp) {
  const record = sessions.get(session);
  if (!record) return;
  record.pc.setRemoteDescription({ type: "answer", sdp }).catch((err) => {
    call("State", session, "answer-error", err instanceof Error ? err.message : String(err));
  });
}

export function send(session, id, text) {
  const channel = sessions.get(session)?.channels.get(id);
  if (!channel || channel.readyState !== "open") return;
  try {
    channel.send(fromBase64(text));
  } catch (err) {
    call("State", session, "send-error", err instanceof Error ? err.message : String(err));
  }
}

export function closeChannel(session, id) {
  const record = sessions.get(session);
  const channel = record?.channels.get(id);
  if (!channel) return;
  record.channels.delete(id);
  try {
    channel.close();
  } catch {
    return;
  }
}

export function close(session) {
  const record = sessions.get(session);
  if (!record) return;
  sessions.delete(session);
  for (const channel of record.channels.values()) {
    try {
      channel.close();
    } catch {
      continue;
    }
  }
  record.channels.clear();
  try {
    record.pc.close();
  } catch {
    return;
  }
}
