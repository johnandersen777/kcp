// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// review of page-rc130 948ee11ba, fixes 1-5, on the real Request VM flow
// (request_vm.js, which app.js runs) and the page wiring around it.

import { createRequestRunner } from "./request_vm.js";
import { createReadyGate } from "./ready_gate.js";
import { contractTransitions } from "./vmshell.js";

function assertEquals(actual, expected, label = "value") {
  const a = JSON.stringify(actual);
  const e = JSON.stringify(expected);
  if (a !== e) throw new Error(`${label}: expected ${e}, got ${a}`);
}
function assert(condition, message) {
  if (!condition) throw new Error(message);
}
const tick = () => new Promise((r) => setTimeout(r, 0));

// A page: the button is disabled exactly as computeRequestDisabled says
// (busy, or not signed in); refresh() recomputes it.
function page({ kcpUp = true, signedIn = "did:plc:alice" } = {}) {
  const p = {
    busy: false,
    kcp: kcpUp,
    identity: signedIn,
    signingOut: false,
    gen: 0, // identityGeneration
    disabled: false,
    statuses: [],
    logs: [],
    requests: [],
    gate: createReadyGate(),
  };
  p.refresh = () => {
    p.disabled = p.busy || !p.identity;
  };
  p.run = createRequestRunner({
    getBusy: () => p.busy,
    setBusy: (b) => {
      p.busy = b;
    },
    kcpUp: () => p.kcp,
    waitKcp: () => p.gate.wait(1000),
    identityReadyFor: (did, gen) => Boolean(did) && p.identity === did && !p.signingOut && gen === p.gen,
    refresh: p.refresh,
    status: (text, kind) => p.statuses.push(`${kind}: ${text}`),
    log: (level, message) => p.logs.push(message),
  });
  p.click = (opts = {}) =>
    p.run({
      did: p.identity,
      gen: p.gen,
      prepare: async () => opts.prepare ?? { subject: "" },
      request: async () => {
        if (opts.fail) throw new Error(opts.fail);
        p.requests.push(p.identity);
        return true;
      },
    });
  return p;
}

Deno.test("fix 1: an early return or a failure re-enables Request VM on a quiet page", async () => {
  const refused = page();
  assertEquals(await refused.click({ prepare: { error: "Associate DID: that is your own DID" } }), "refused");
  assertEquals([refused.busy, refused.disabled], [false, false], "early return");
  const failed = page();
  assertEquals(await failed.click({ fail: "EphemeralInfra web-x did not publish its record in time" }), "failed");
  assertEquals([failed.busy, failed.disabled], [false, false], "failure (record sync timeout)");
  assertEquals(failed.statuses.at(-1), "err: EphemeralInfra web-x did not publish its record in time");
  const queued = page({ kcpUp: false });
  const pending = queued.click({ prepare: { error: "Associate DID: enter a did:plc: or did:web: DID" } });
  await tick();
  assertEquals(queued.disabled, true, "disabled while queued");
  queued.gate.open("worker");
  assertEquals(await pending, "refused");
  assertEquals(queued.disabled, false, "queued click that returns early");
  const ok = page();
  assertEquals(await ok.click(), "requested");
  assertEquals([ok.busy, ok.disabled], [true, true], "a request that went through stays busy until the feed shows it");
});

Deno.test("fix 2: a Sign out while the click waits for kcp creates nothing", async () => {
  const p = page({ kcpUp: false });
  const pending = p.click();
  await tick();
  p.signingOut = true; // Sign out tapped; it also waits for kcp
  p.gate.open("worker");
  assertEquals(await pending, "failed");
  assertEquals(p.requests, [], "no infra or contract");
  assertEquals(p.statuses.at(-1), "err: signed out while the request waited; sign in and request again");
  const other = page({ kcpUp: false });
  const pending2 = other.run({ did: "did:plc:alice", gen: other.gen, prepare: async () => ({}), request: async () => true });
  other.identity = "did:plc:bob"; // switched account meanwhile
  other.gate.open("worker");
  assertEquals(await pending2, "failed", "another DID");
});

Deno.test("fix 5: a fast double click starts one request", async () => {
  const p = page();
  let release;
  const slowPrepare = new Promise((r) => (release = r)); // guestToolsServed() awaiting
  const first = p.run({ did: p.identity, gen: p.gen, prepare: () => slowPrepare, request: async () => (p.requests.push(1), true) });
  const second = await p.run({ did: p.identity, gen: p.gen, prepare: async () => ({}), request: async () => (p.requests.push(2), true) });
  assertEquals(second, "busy");
  release({ subject: "" });
  assertEquals(await first, "requested");
  assertEquals(p.requests, [1]);
  assert(p.logs.includes("Request VM is already running"), "the second click is logged");
});

Deno.test("fix 4: a phase change seen while kcp boots is flushed once kcp is ready", () => {
  // flushKcpSoon without a worker marks the flush as skipped; spawnKcp
  // flushes once the gate opens. The fingerprint itself still advances.
  let base = null;
  let skipped = false;
  let flushed = 0;
  let kcpWorker = null;
  const flushKcpSoon = () => {
    if (!kcpWorker) {
      skipped = true;
      return;
    }
    flushed++;
  };
  const apply = (phase) => {
    const t = contractTransitions(base, [{ name: "a", phase }]);
    base = t.key;
    if (t.changed) flushKcpSoon();
  };
  apply("Requesting");
  apply("Held"); // while kcp boots (restored feed)
  assertEquals([skipped, flushed], [true, 0]);
  kcpWorker = {}; // kcpGate.open
  if (skipped) {
    skipped = false;
    flushKcpSoon();
  }
  assertEquals(flushed, 1, "flushed after ready");
});

Deno.test("page wiring: fixes 1-5 and the flush ids", async () => {
  const app = await Deno.readTextFile(new URL("./app.js", import.meta.url));
  // 1, 5: app.js runs request_vm.js; the button state is one function.
  assert(/const runRequest = createRequestRunner\(\{/.test(app) && /return runRequest\(\{/.test(app), "requestVM runs the runner");
  assert(/refresh: \(\) => refreshComputeRequest\(\)/.test(app), "the runner recomputes the button");
  assert(/function refreshComputeRequest\(\) \{\s*computeRequestEl\.disabled = computeRequestDisabled\(lastFeedState\);/.test(app), "refresh uses the feed rule");
  const prepare = app.slice(app.indexOf("function requestVM()"), app.indexOf("function requestVM()") + 800);
  assert(prepare.indexOf("guestToolsServed()") > 0, "guestToolsServed is awaited inside prepare, after busy is set");
  // 2: the identity at click, after the gate, in sessionInfra and before the contract.
  assert(/signIn\.step !== "signingOut"/.test(app.slice(app.indexOf("function identityReadyFor"), app.indexOf("function identityReadyFor") + 300)), "signing out counts");
  assert(/sessionInfra\(\(step\) => progress\(step\), did, gen\)/.test(app), "sessionInfra gets the DID");
  assert(/if \(expectDid && !identityReadyFor\(expectDid, expectGen\)\) throw/.test(app), "sessionInfra checks it");
  assert(/if \(!identityReadyFor\(did, gen\)\) \{[\s\S]{0,400}throw new Error\("signed out while the request ran; no contract was created"\);\s*\}\s*progress\("creating the contract"\);/.test(app), "checked again before the contract");
  // 3: the ready flag after every listener.
  const init = app.slice(app.indexOf("function initSocialwebUI"), app.indexOf("// --- compute ---"));
  assert(init.indexOf("initComputeUI();") < init.indexOf("socialwebUIReady = true;"), "flag after initComputeUI");
  assert(init.indexOf("socialwebUIReady = true;") < init.lastIndexOf("renderIdentity();"), "identity rendered with the flag");
  // 4: skipped flushes run after the gate opens.
  assert(/kcpGate\.open\(worker\);\s*if \(kcpFlushSkipped\) \{\s*kcpFlushSkipped = false;\s*void flushKcpSoon\(/.test(app), "flush after ready");
  // 6: flush answers by id; the worker echoes it.
  assert(/persistFlushWaiters\.get\(data\.id\)\?\.\(/.test(app), "answers resolve their own flush");
  const worker = await Deno.readTextFile(new URL("./kcp_worker.js", import.meta.url));
  assert(/type: "persist-flushed", id: flushId/.test(worker), "worker echoes the id");
  assert(/kcp did not finish starting in \$\{REQUEST_KCP_WAIT_MS \/ 60_000\} min; reload the page and request again/.test(app), "a softer message after 10 min");
});

// Re-review A: signIn.step "signingOut" shows only for GRACE_MS (15 s); the
// identity generation, bumped on the tap, does not reset.
Deno.test("fix A: a Sign out tapped while kcp boots for more than 15 s aborts the queued request", async () => {
  const p = page({ kcpUp: false });
  const pending = p.click();
  await tick();
  p.gen++; // signOutIdentity, synchronously on the tap
  p.signingOut = true;
  p.signingOut = false; // > 15 s later renderIdentity resets the step; still Ready (kcp not up)
  p.gate.open("worker");
  assertEquals(await pending, "failed");
  assertEquals(p.requests, [], "nothing for the identity being signed out");
  const app = await Deno.readTextFile(new URL("./app.js", import.meta.url));
  for (const fn of ["async function startSignIn", "async function signOutIdentity", "async function switchAccount"]) {
    const body = app.slice(app.indexOf(fn), app.indexOf(fn) + 80);
    assert(/\{\s*identityGeneration\+\+;/.test(body), `${fn} bumps the generation first`);
  }
  assert(/signIn\.step !== "signingOut" && gen === identityGeneration/.test(app), "identityReadyFor needs the generation unchanged");
  assert(/const gen = identityGeneration;\s*return runRequest\(\{\s*did,\s*gen,/.test(app), "captured with the DID at click");
  assert(/sessionInfra\(\(step\) => progress\(step\), did, gen\)/.test(app) && /identityReadyFor\(expectDid, expectGen\)/.test(app), "checked in sessionInfra");
  assert(/socialwebCall\("socialwebSignOut", \{ workspace: "root", name: "requester" \}, 30_000, REQUEST_KCP_WAIT_MS\)/.test(app), "the sign out waits as long as the queued click");
});

Deno.test("fix B: after a take over the button stays disabled", async () => {
  const app = await Deno.readTextFile(new URL("./app.js", import.meta.url));
  const rule = app.slice(app.indexOf("function computeRequestDisabled"), app.indexOf("function computeRequestDisabled") + 500);
  assert(/kcpGate\.state === "failed"/.test(rule), "a failed kcp gate disables Request VM");
});

Deno.test("fix C: the last identity check runs before the key, tokens and vault are written", async () => {
  const app = await Deno.readTextFile(new URL("./app.js", import.meta.url));
  const body = app.slice(app.indexOf("async function requestVMFor"), app.indexOf("async function requestVMFor") + 5000);
  const check = body.indexOf('throw new Error("signed out while the request ran; nothing was created")');
  assert(check > 0, "a check before the writes");
  for (const write of ["webrtcGuest.newSSHKey()", "webrtcGuest.saveSSHKey(", "localStorage.setItem(computePrefs.transport", "socialwebVaultPut"]) {
    assert(body.indexOf(write) > check, `${write} after the check`);
  }
  const last = body.slice(body.indexOf("if (!identityReadyFor(did, gen)) {"));
  assert(/deleteSSHKey\(name\)[\s\S]{0,200}removeItem\(computePrefs\.transport\(name\)\)[\s\S]{0,100}removeItem\(computePrefs\.token\(name\)\)/.test(last), "a sign out in the last moments cleans up");
});
