// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// deno test --allow-net scripts/browser/webrtc_guest_test.js

import {
  agentStartEvent,
  authorizedKey,
  contractEventManifest,
  dialGuest,
  fingerprintMatches,
  firstOpened,
  guestFromState,
  newAgeIdentity,
  NSID_PUBLIC_KEY,
  NSID_SIGNAL,
  seal,
} from "./webrtc_guest.js";

function assert(cond, message) {
  if (!cond) throw new Error(message);
}

Deno.test("fingerprintMatches requires every a=fingerprint to be the onNetwork one", () => {
  const fp = "sha-256 AB:CD:EF";
  const sdp = "v=0\r\na=fingerprint:sha-256 ab:cd:ef\r\nm=application\r\na=fingerprint:sha-256 AB:CD:EF\r\n";
  assert(fingerprintMatches(sdp, fp), "matching fingerprints, any case");
  assert(!fingerprintMatches(sdp + "a=fingerprint:sha-256 00:11\r\n", fp), "one extra fingerprint");
  assert(!fingerprintMatches("v=0\r\n", fp), "no fingerprint");
  assert(!fingerprintMatches(sdp, ""), "no expected value");
});

Deno.test("authorizedKey is the ssh-ed25519 wire form", () => {
  const line = authorizedKey(new Uint8Array(32), "c");
  // 00 00 00 0b "ssh-ed25519" 00 00 00 20 + 32 zero bytes
  assert(line === "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA c", line);
});

Deno.test("guestFromState needs key, webrtc fingerprint and host keys", () => {
  const state = {
    contracts: [{ name: "c", networks: [{ tag: "ssh", kind: "webrtc", address: "sha-256 AA" }], hostKeys: ["ssh-ed25519 X"] }],
    contractEvents: [
      { name: "a", contract: "c", direction: "inbound", nsid: NSID_PUBLIC_KEY, tag: "ssh", key: "age1old", createdAt: "1" },
      { name: "b", contract: "c", direction: "inbound", nsid: NSID_PUBLIC_KEY, tag: "ssh", key: "age1new", createdAt: "2" },
      { name: "o", contract: "c", direction: "outbound", nsid: NSID_PUBLIC_KEY, tag: "ssh", key: "age1mine", createdAt: "3" },
    ],
  };
  const guest = guestFromState(state, "c");
  assert(guest?.key === "age1new" && guest.fingerprint === "sha-256 AA", JSON.stringify(guest));
  assert(guestFromState({ ...state, contractEvents: [] }, "c") === null, "no publicKey yet");
});

Deno.test("contractEventManifest", () => {
  const m = contractEventManifest({ name: "n", contract: "c", nsid: NSID_SIGNAL, kind: "offer", ciphertext: "x" });
  assert(m.kind === "ContractEvent" && m.spec.contractRef.name === "c" && m.spec.direction === "outbound", "shape");
  assert(m.spec.tag === "ssh" && m.spec.ciphertext === "x" && !("key" in m.spec), "fields");
});

Deno.test("agentStartEvent is an outbound agentStart with no kind, tag or payload", () => {
  const m = agentStartEvent("c", "s1");
  assert(m.kind === "ContractEvent" && m.metadata.name === "c-agent-s1", "name");
  assert(JSON.stringify(m.spec) === JSON.stringify({
    contractRef: { name: "c" },
    direction: "outbound",
    nsid: "com.publicdomainrelay.temp.compute.events.guest.agentStart",
  }), JSON.stringify(m.spec));
});

Deno.test("seal and open round trip (typage from esm.sh)", async () => {
  const me = await newAgeIdentity();
  assert(me.recipient.startsWith("age1"), me.recipient);
  const armored = await seal("v=0 offer", me.recipient);
  assert(armored.startsWith("-----BEGIN AGE ENCRYPTED FILE-----") && !armored.includes("v=0"), "armored");
  assert(await me.open(armored) === "v=0 offer", "opened");
});

Deno.test("firstOpened skips answers sealed to another identity", async () => {
  const me = await newAgeIdentity();
  const other = await newAgeIdentity();
  const theirs = await seal('{"sdp":"theirs"}', other.recipient);
  const mine = await seal('{"sdp":"mine"}', me.recipient);
  assert(await firstOpened([theirs, mine], me.open) === '{"sdp":"mine"}', "mine");
  assert(await firstOpened([theirs], me.open) === undefined, "none");
});

// A stand-in RTCPeerConnection: gathering is complete at once and the
// datachannel never opens, so only dialGuest's own timeouts end a dial.
class FakePeerConnection {
  iceGatheringState = "complete";
  localDescription = { sdp: "v=0 offer" };
  createDataChannel() {
    return {};
  }
  createOffer() {
    return Promise.resolve({ type: "offer", sdp: "v=0 offer" });
  }
  setLocalDescription() {
    return Promise.resolve();
  }
  setRemoteDescription() {
    return Promise.resolve();
  }
  addEventListener() {}
  removeEventListener() {}
  close() {}
}

Deno.test("dialGuest: no answer or no open channel is a connectivity error", async () => {
  const saved = globalThis.RTCPeerConnection;
  globalThis.RTCPeerConnection = FakePeerConnection;
  try {
    const guest = await newAgeIdentity();
    const fingerprint = "sha-256 AB:CD";
    const base = {
      guest: () => Promise.resolve({ key: guest.recipient, fingerprint, hostKeys: [] }),
      publishKey: () => Promise.resolve(),
    };
    const dial = async (exchange) => {
      try {
        await dialGuest({ signaling: { ...base, exchange }, answerTimeoutMs: 50, openTimeoutMs: 50 });
      } catch (err) {
        return err;
      }
      throw new Error("dial should fail");
    };
    const silent = await dial(() => new Promise(() => {}));
    assert(silent.connectivity && /no answer/.test(silent.message), silent.message);
    const answer = JSON.stringify({ sdp: `v=0\r\na=fingerprint:${fingerprint}\r\n` });
    const closed = await dial(() => Promise.resolve(answer));
    assert(closed.connectivity && /did not open/.test(closed.message), closed.message);
    const wrong = await dial(() => Promise.resolve(JSON.stringify({ sdp: "a=fingerprint:sha-256 00:11\r\n" })));
    assert(!wrong.connectivity && /fingerprint/.test(wrong.message), "a fingerprint mismatch is not a fallback case");
  } finally {
    globalThis.RTCPeerConnection = saved;
  }
});
