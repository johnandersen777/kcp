// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// Page half of the policy suite (run all of it: scripts/browser-dev/policy-suite.sh).
// Test names start with the scenario names of socialweb's policy tests.
//
// The bundled requester-tangled-vouch action runs under socialweb's action
// shim (workflow/action.go) in the sandbox Worker, with trust seeded as the
// run reconciler does (cache trust/<counterparty> = trust.Snapshot.CacheFiles(),
// whose vouchedBy[self] is the union of the trusted operators' sets, rc124).
// The DIDs do not exist, so verdicts come only from the seeded snapshot.
// verdict() is the Go side's reading: allow only on exit 0 with a result line
// and a strict allow=true output; anything else (throw, timeout, kill) denies.
// Needs the build's socialweb mirror; without it every case FAILS (no skip:
// a skip knob on a security gate is a bypass).

import { rewriteNodeImports, runInWorker } from "./policy_sandbox.js";

function assert(cond, message) {
  if (!cond) throw new Error(message);
}

const mirror = new URL("../../.work/socialweb-mirror/internal/policyengine/", import.meta.url);
const haveMirror = await Deno.stat(new URL("workflow/action.go", mirror)).then(() => true, () => false);

const self = "did:plc:suiteself0000000000000000", selfOp = "did:plc:suiteselfoperator000000000";
const bidder = "did:plc:suitebidder00000000000000", bidderOp = "did:plc:suitebidderoperator0000000";

// trust.Snapshot.CacheFiles (ActionVouchedBy merges the trusted operators'
// sets into the first one, self).
function cacheFiles({ operatorOf, vouchedBy, trustedOperators }) {
  const merged = { ...vouchedBy };
  merged[trustedOperators[0]] = [...new Set(trustedOperators.flatMap((op) => vouchedBy[op] ?? []))].sort();
  const file = (value) => ({ data: JSON.stringify(value), encoding: "text" });
  return { "operatorOf.json": file(operatorOf), "vouchedBy.json": file(merged), "trustedOperators.json": file(trustedOperators) };
}

// spy: every program the runner receives
let runs = 0;

async function tangledVouch(mode, { trust, prelude = "", timeoutMs = 30_000, kill = false }) {
  if (!haveMirror) {
    throw new Error(`no socialweb mirror at ${mirror.pathname}: copy .work from a built worktree (cp -a <worktree>/.work .) or run scripts/browser-dev/build-page.sh`);
  }
  const go = await Deno.readTextFile(new URL("workflow/action.go", mirror));
  const shim = go.match(/const actionShim = `([\s\S]*?)`/)[1];
  const footer = go.match(/const actionFooter = `([\s\S]*?)`/)[1];
  const action = await Deno.readTextFile(new URL("bundled-actions/tangy/policy-requester-tangled-vouch/dist/index.js", mirror));
  const cache = trust ? { [`trust/${bidder}`]: trust } : {};
  const env = {
    GITHUB_OUTPUT: "/gh/output", GITHUB_ENV: "/gh/env", GITHUB_CACHE: "/gh/cache", GITHUB_EVENT_PATH: "/gh/event",
    "INPUT_SELF-DID": self, "INPUT_COUNTERPARTY-DID": bidder, "INPUT_SUBJECT-DID": bidder, "INPUT_ROOT-REQUESTER-DID": self,
    "INPUT_MODE": mode,
  };
  const source = shim.replace("__PE_ENV_JSON__", JSON.stringify(env))
    .replace("__PE_CACHE_JSON__", JSON.stringify(JSON.stringify(cache)))
    .replace("__PE_EVENT_JSON__", JSON.stringify(JSON.stringify({ inputs: {} })))
    .replaceAll("__PE_RESULT_MARKER__", JSON.stringify("__PE_RESULT__")) + "\n" + prelude + "\n" + action + "\n" + footer;
  runs++;
  const pending = runInWorker({ source, env, timeoutMs });
  if (kill) setTimeout(() => pending.kill(), 200);
  return verdict(await pending);
}

// ran: "decision" (the action wrote an allow output and exited 0), "threw"
// (the action failed; the shim's exit handler reported code != 0), "fetched"
// (the action reached the network spy), "unanswered" (killed; a killed
// Worker's output is lost, so the only evidence is the runner spy).
function verdict(r) {
  const line = r.stdout.split("\n").find((l) => l.startsWith("__PE_RESULT__"));
  const result = line ? JSON.parse(line.slice("__PE_RESULT__".length)) : undefined;
  const allows = [...String(result?.vfs?.["/gh/output"] ?? "").matchAll(/^allow=(.*)$/gm)].map((m) => m[1]);
  const allow = !r.timedOut && result?.code === 0 && allows.length > 0 && allows.every((a) => a === "true");
  const ran = new Set();
  if (result?.code === 0 && allows.length > 0) ran.add("decision");
  if (result && result.code !== 0) ran.add("threw");
  if (r.stderr.includes("__PE_SPY_FETCH__")) ran.add("fetched");
  if (r.timedOut) ran.add("unanswered");
  return { allow, ran, timedOut: r.timedOut, code: result?.code, why: `${r.stdout.slice(-300)} ${r.stderr.slice(-300)}` };
}

const vouched = (vouchedBy, operatorOf = { [self]: self, [bidder]: bidderOp }, trustedOperators = [self]) =>
  cacheFiles({ operatorOf, vouchedBy, trustedOperators });

const spyFetch = (answer) => `globalThis.fetch = (url) => { console.error("__PE_SPY_FETCH__ " + url); return ${answer}; };`;
const offline = spyFetch(`Promise.reject(new TypeError("offline"))`);
// A stall after the trust check: the action decides from a valid snapshot,
// then its output write never returns, so only the deadline or a kill ends
// the run (the action makes no network call once the host seeds trust).
const stalledOutput = `{ const write = Deno.writeTextFile; Deno.writeTextFile = (path, ...rest) =>
  String(path) === "/gh/output" ? new Promise(() => {}) : write(path, ...rest); }`;

// [scenario, want allow, evidence the policy ran, opts]
const cases = [
  // self vouches the bidder itself; neither side has an operator.
  ["vouch-direct-allow", true, ["decision"], { trust: vouched({ [self]: [bidder] }, { [self]: self }) }],
  // self vouches the bidder's verified operator.
  ["vouch-direct-allow (operator of bidder)", true, ["decision"], { trust: vouched({ [self]: [bidderOp] }) }],
  // operator union (rc124): self vouches nobody, its own operator vouches the
  // bidder's operator; the seeding gives self that set.
  ["vouch-operator-allow", true, ["decision"], {
    trust: vouched({ [self]: [], [selfOp]: [bidderOp], [bidderOp]: [] }, { [self]: selfOp, [bidder]: bidderOp }, [self, selfOp]),
  }],
  ["unvouched-deny", false, ["decision"], { trust: vouched({ [self]: [], [bidderOp]: [] }) }],
  // the bidder claims bidderOp (vouched by self) but the claim did not
  // verify, so the snapshot has no operator for it.
  ["vouch-unverified-operator-deny", false, ["decision"], { trust: vouched({ [self]: [bidderOp] }, { [self]: self }) }],
  // PT-1: only the reverse vouch exists (the bidder's verified operator
  // vouches self); that is no reason to trust the bidder.
  ["counterparty-operator-vouches-self-only", false, ["decision"], { trust: vouched({ [self]: [], [bidderOp]: [self] }) }],
  // PT-3: self vouched the bidder DID itself; the bidder also has a verified
  // operator (that self does not vouch).
  ["vouched-directly-with-verified-operator", true, ["decision"], { trust: vouched({ [self]: [bidder], [bidderOp]: [] }) }],
  // PT-4: the bidder's verified operator is self.
  ["counterparty-operated-by-self", true, ["decision"], { trust: vouched({ [self]: [] }, { [self]: self, [bidder]: self }) }],
  // fail-closed: a garbage snapshot makes the action throw (exit 1).
  ["trust-host-error-deny (fail-closed: malformed snapshot)", false, ["threw"], {
    trust: { ...vouched({ [self]: [bidder] }), "vouchedBy.json": { data: "{not json", encoding: "text" } },
  }],
  // fail-closed/no-trust-snapshot (socialweb B2): trust comes only from the
  // host snapshot; without a whole one the action denies at once and never
  // touches the network (the offline spy records any fetch).
  ["no-trust-snapshot-deny (fail-closed: no snapshot, no fetch)", false, ["decision", "!fetched"], { prelude: offline }],
  ["no-trust-snapshot-deny (fail-closed: half a snapshot, no fetch)", false, ["decision", "!fetched"], {
    prelude: offline, trust: { "operatorOf.json": vouched({ [self]: [bidder] })["operatorOf.json"] },
  }],
  // fail-closed/deadline and /cancelled: an allowing snapshot, but the action
  // stalls after deciding; killed at the deadline or cancelled, nothing is
  // sent (no result line) and nothing fetched, so the run denies.
  ["deadline-deny (fail-closed: stalled after an allow, killed at the deadline)", false, ["unanswered", "!decision", "!fetched"], {
    prelude: offline + stalledOutput, trust: vouched({ [self]: [bidder] }, { [self]: self }), timeoutMs: 1_000,
  }],
  ["cancelled-deny (fail-closed: stalled after an allow, run cancelled)", false, ["unanswered", "!decision", "!fetched"], {
    prelude: offline + stalledOutput, trust: vouched({ [self]: [bidder] }, { [self]: self }), timeoutMs: 60_000, kill: true,
  }],
];

for (const mode of ["scope", "full"]) {
  for (const [name, want, evidence, opts] of cases) {
    Deno.test({ name: `always-run/page/requester-tangled-vouch/${mode}/${name}`, fn: async () => {
      const before = runs;
      const r = await tangledVouch(mode, opts);
      assert(runs === before + 1, "the runner did not receive the policy program");
      // "x" must be in ran, "!x" must not.
      assert(evidence.every((e) => e.startsWith("!") ? !r.ran.has(e.slice(1)) : r.ran.has(e)), `ran=${[...r.ran]}, want ${evidence}: ${r.why}`);
      assert(r.allow === want, `allow=${r.allow}, want ${want} (code=${r.code} timedOut=${r.timedOut}): ${r.why}`);
    } });
  }
}

Deno.test("fail-closed/verdict: every non-answer denies (malformed-output-deny, missing-allow-output-deny)", () => {
  const ok = { stdout: `__PE_RESULT__{"code":0,"vfs":{"/gh/output":"allow=true\\n"}}\n`, stderr: "", timedOut: false };
  assert(verdict(ok).allow, "strict allow=true allows");
  for (const [what, r] of [
    ["no result line", { ...ok, stdout: "allow=true\n" }],
    ["exit 1", { ...ok, stdout: ok.stdout.replace(`"code":0`, `"code":1`) }],
    ["timed out", { ...ok, timedOut: true }],
    ["no allow output", { ...ok, stdout: ok.stdout.replace("allow=true", "violations=[]") }],
    ["allow not strict", { ...ok, stdout: ok.stdout.replace("allow=true", "allow=yes") }],
    ["one allow=false", { ...ok, stdout: ok.stdout.replace("allow=true\\n", "allow=true\\nallow=false\\n") }],
  ]) assert(!verdict(r).allow, what);
});

// WebKit denies static data: imports in the opaque-origin Worker, so after
// the rewrite a bundled action must have no static import of node: or data:
// left (Deno would accept one, so the runs above do not show it).
Deno.test("always-run/page/bundled actions keep no static node: or data: import after the rewrite", async () => {
  if (!haveMirror) throw new Error(`no socialweb mirror at ${mirror.pathname}`);
  for (const name of ["policy-requester-tangled-vouch", "policy-requester-mutuals", "policy-bidder-mutuals"]) {
    const src = await Deno.readTextFile(new URL(`bundled-actions/tangy/${name}/dist/index.js`, mirror));
    const out = rewriteNodeImports(src);
    const statics = [...out.matchAll(/\b(import|export)\s*[\w${*][^;\n(]{0,200}?\bfrom\s*["'](node|data):/g), ...out.matchAll(/\bimport\s*["'](node|data):/g)];
    if (statics.length) throw new Error(`${name}: static import left: ${statics[0][0].slice(0, 80)}`);
    if (/["']node:/.test(out)) throw new Error(`${name}: a node: specifier was not rewritten`);
  }
});
