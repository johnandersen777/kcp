// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import { createReadyGate } from "./ready_gate.js";
import { schemaHasAgent } from "./vmshell.js";

function assertEquals(actual, expected, label = "value") {
  const a = JSON.stringify(actual);
  const e = JSON.stringify(expected);
  if (a !== e) throw new Error(`${label}: expected ${e}, got ${a}`);
}
function assert(condition, message) {
  if (!condition) throw new Error(message);
}
const tick = () => new Promise((r) => setTimeout(r, 0));

Deno.test("ready gate: waiters pass once open; later waits pass at once", async () => {
  const gate = createReadyGate();
  const got = [];
  const a = gate.wait().then((w) => got.push(`a:${w}`));
  await tick();
  assertEquals(got, [], "nothing before ready");
  gate.open("worker");
  await a;
  assertEquals(await gate.wait(), "worker");
  assertEquals(got, ["a:worker"]);
});

Deno.test("ready gate: a take over fails the waiters; a timeout says kcp is still starting", async () => {
  const gate = createReadyGate();
  const waiting = gate.wait().then(() => "ok", (e) => e.message);
  gate.fail(new Error("taken over"));
  assertEquals(await waiting, "taken over");
  const slow = createReadyGate();
  assertEquals(await slow.wait(20).then(() => "ok", (e) => e.message), "kcp is still starting (waited 0 s)");
});

// The boot race (session 6av8ic): identity Ready from a restored feed before
// kcp is ready. The page's kcp calls wait on the gate, so the feature probe
// and the GC run once kcp is up instead of failing for the session.
Deno.test("identity Ready before kcp ready: features turn on and the GC runs once kcp is ready", async () => {
  const gate = createReadyGate();
  const calls = [];
  const kcpREST = async (method, path) => {
    await gate.wait(); // kcpPageRequest -> kcpWorkerReady()
    calls.push(`${method} ${path}`);
    if (path.includes("apiresourceschemas")) {
      return { spec: { versions: [{ schema: { properties: { spec: { properties: { agent: { type: "object" } } } } } }] } };
    }
    if (path.endsWith("/ephemeralinfras")) return { items: [] };
    return { spec: { resources: [{ name: "computecontracts", schema: "v1.computecontracts" }] } };
  };
  // What applyComputeState / applyState start on identity Ready:
  const agent = kcpREST("GET", "/apiexports/computecontracts")
    .then(() => kcpREST("GET", "/apiresourceschemas/v1.computecontracts"))
    .then((schema) => schemaHasAgent(schema));
  const gc = kcpREST("GET", "/ephemeralinfras").then(() => "gc ran");
  await tick();
  assertEquals(calls, [], "nothing reaches kcp before it is ready");
  gate.open("worker");
  assertEquals(await agent, true, "agent feature on");
  assertEquals(await gc, "gc ran");
});

Deno.test("page wiring: kcp calls wait for ready; probes retry; Request VM never a silent no-op", async () => {
  const app = await Deno.readTextFile(new URL("./app.js", import.meta.url));
  assert(/async function kcpPageRequest\(request, timeoutMs = 60_000\) \{\s*const worker = await kcpWorkerReady\(\);/.test(app), "kcpREST waits");
  assert(/async function socialwebCall\(fn, payload, timeoutMs = 30_000\) \{\s*const worker = await kcpWorkerReady\(\);/.test(app), "socialwebCall waits");
  assert(/kcpWorker = worker;\s*kcpGate\.open\(worker\);/.test(app), "gate opens with the worker");
  assert(/if \(lastFeedState\) queueMicrotask\(\(\) => applyState\(lastFeedState\)\)/.test(app), "identity handler reruns on ready");
  assert(/kcpGate\.fail\(new Error\(TAKEN_OVER_MESSAGE\)\)/.test(app), "a take over fails the waiters");
  assert(/if \(schema === undefined\) \{\s*r2StartServed = null;/.test(app), "a failed R2 probe is asked again");
  assert(!/stay off: /.test(app), "no permanent off");
  const req = app.slice(app.indexOf("async function requestVM"), app.indexOf("async function requestVM") + 1200);
  assert(req.indexOf('addLog("info", "Request VM clicked")') >= 0, "click logged");
  assert(/kcp is still starting; the request runs once it is up/.test(req) && /await kcpWorkerReady\(\)/.test(req), "click before ready is queued with a message");
});

// Session 9xc9tc: Request VM was enabled by the feed before its listener was
// bound (initSocialwebUI ran at the end of boot), and a click vanished.
Deno.test("page wiring: compute and identity listeners are bound before kcp boots", async () => {
  const app = await Deno.readTextFile(new URL("./app.js", import.meta.url));
  // Bound at module level before start(): no boot step (throwing or hanging)
  // can skip it, and it runs before the kcp lock and kcp boot.
  const tail = app.slice(app.lastIndexOf('setStatus("starting…", "running");'));
  assert(/try \{\s*initSocialwebUI\(\);\s*\} catch/.test(tail), "init guarded at module level");
  assert(tail.indexOf("initSocialwebUI();") < tail.indexOf("start().catch("), "listeners bound before start()");
  const start = app.slice(app.indexOf("async function start()"), app.indexOf("async function start()") + 4000);
  assert(!start.includes("initSocialwebUI();"), "not inside start(), where a failing step could skip it");
  assert((app.match(/^\s*initSocialwebUI\(\);/gm) ?? []).length === 1, "initialised once");
  const ui = app.slice(app.indexOf("function initSocialwebUI"), app.indexOf("// --- compute ---"));
  assert(/identitySignInEl\.addEventListener\("click"/.test(ui) && /initComputeUI\(\);/.test(ui), "identity and compute listeners in init");
  const compute = app.slice(app.indexOf("function initComputeUI"), app.indexOf("function initComputeUI") + 300);
  assert(/computeRequestEl\.addEventListener\("click", \(\) => \{[\s\S]*?void requestVM\(\);/.test(compute), "Request VM click goes to requestVM");
  assert(/computeRequestEl\.disabled = !socialwebUIReady \|\|/.test(app), "Request VM stays disabled until the UI is initialised");
  assert(/identitySignInEl\.disabled = !socialwebUIReady \|\|/.test(app), "Sign in stays disabled until the UI is initialised");
});

// A click that lands before kcp is ready reaches requestVM, which logs it and
// queues it behind kcp ready (the gate), with the visible message.
Deno.test("a Request VM click before kcp ready is logged and queued, then runs", async () => {
  const gate = createReadyGate();
  const log = [];
  let status = "";
  const requestVM = async () => {
    log.push("Request VM clicked");
    status = "kcp is still starting; the request runs once it is up";
    await gate.wait();
    log.push("requesting ComputeContract");
  };
  const listeners = [];
  const button = { addEventListener: (_t, fn) => listeners.push(fn), click: () => listeners.forEach((fn) => fn()) };
  button.addEventListener("click", () => void requestVM()); // initComputeUI, at page init
  button.click(); // the user, before kcp is ready
  await tick();
  assertEquals(log, ["Request VM clicked"]);
  assertEquals(status, "kcp is still starting; the request runs once it is up");
  gate.open("worker");
  await tick();
  assertEquals(log, ["Request VM clicked", "requesting ComputeContract"]);
});

Deno.test("page wiring: Request VM shows its progress while the infra and record take time", async () => {
  const app = await Deno.readTextFile(new URL("./app.js", import.meta.url));
  const req = app.slice(app.indexOf("async function requestVM"), app.indexOf("async function requestVM") + 4000);
  const steps = ['progress("preparing this tab\'s infra")', "sessionInfra((step) => progress(step))", 'progress("creating the contract")', "await setInfraContract("];
  let at = 0;
  for (const step of steps) {
    const i = req.indexOf(step, at);
    assert(i >= at, `${step} in order`);
    at = i;
  }
  assert(/requesting a VM: \$\{step\}…/.test(req), "progress text");
  const infra = app.slice(app.indexOf("async function sessionInfra"), app.indexOf("async function sessionInfra") + 3500);
  assert(/onStep\("syncing its record"\);\s*const synced = await waitInfraRecord/.test(infra), "record sync step");
});
