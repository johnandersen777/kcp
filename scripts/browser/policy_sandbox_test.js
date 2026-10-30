// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import { connect, frameSrcdoc, probe, rewriteNodeImports, runInWorker } from "./policy_sandbox.js";

function assert(cond, message) {
  if (!cond) throw new Error(message);
}
const eq = (a, b, what) => assert(JSON.stringify(a) === JSON.stringify(b), `${what}: ${JSON.stringify(a)} != ${JSON.stringify(b)}`);

const run = (source, extra = {}) => runInWorker({ source, env: {}, timeoutMs: 10_000, ...extra });

// The shape socialweb's expression evaluator generates (expression.go).
Deno.test("expression program prints its value on stdout", async () => {
  const r = await run(`console.log(String((() => {
const inputs = {"mode":"scope"};
return (inputs['mode'] === "scope");
})()))`);
  eq([r.exitCode, r.stdout, r.stderr, r.timedOut], [0, "true\n", "", false], "result");
});

// The parts of socialweb's action shim (action.go actionShim) that touch Deno.
Deno.test("action shim: env override, virtual files, exit, result marker", async () => {
  const r = await run(`const __pe_env = {"GITHUB_OUTPUT":"/gh/out","NAME":"x"};
const __pe_vfs = {};
const __pe_realExit = Deno.exit.bind(Deno);
const __pe_originalWriteTextFileSync = Deno.writeTextFileSync.bind(Deno);
Object.defineProperty(Deno, "env", { configurable: true, value: { get: (k) => __pe_env[k] } });
Deno.writeTextFileSync = (path, data) => {
  if (path === __pe_env.GITHUB_OUTPUT) { __pe_vfs[path] = data; return; }
  return __pe_originalWriteTextFileSync(path, data);
};
const enc = new TextEncoder();
for (const m of ["log", "error"]) console[m] = (...a) => Deno.stderr.writeSync(enc.encode(a.map((v) => typeof v === "string" ? v : Deno.inspect(v)).join(" ") + "\\n"));
class PeExit extends Error { constructor(code) { super("exit"); this.code = code; } }
Deno.exit = (code = 0) => { throw new PeExit(code); };
globalThis.addEventListener("unhandledrejection", (event) => {
  event.preventDefault();
  Deno.stdout.writeSync(enc.encode("__PE_RESULT__" + JSON.stringify({ code: event.reason.code, vfs: __pe_vfs }) + "\\n"));
  __pe_realExit(0);
});
await Promise.resolve();
Deno.writeTextFileSync("/gh/out", "allow=" + Deno.env.get("NAME"));
console.log({ seen: true });
await (async () => { Deno.exit(3); })();
`);
  eq(r.exitCode, 0, "exit");
  eq(r.stdout, `__PE_RESULT__{"code":3,"vfs":{"/gh/out":"allow=x"}}\n`, "stdout");
  eq(r.stderr, `{"seen":true}\n`, "stderr");
});

Deno.test("no filesystem: read fails like deno without --allow-read", async () => {
  const r = await run(`try { Deno.readTextFileSync("/etc/hosts"); console.log("READ"); } catch (e) { console.log(e.name); }
await Deno.readTextFile("/etc/hosts");`);
  eq(r.exitCode, 1, "exit");
  eq(r.stdout, "NotCapable\n", "stdout");
  assert(r.stderr.includes("--allow-read"), r.stderr);
});

Deno.test("uncaught error exits 1 with output kept", async () => {
  const r = await run(`console.log("before"); throw new Error("boom");`);
  eq(r.exitCode, 1, "exit");
  assert(r.stderr.includes("boom"), r.stderr);
});

Deno.test("output is capped at 64 KiB per stream", async () => {
  const r = await run(`for (let i = 0; i < 2000; i++) console.log("x".repeat(100));`);
  eq(r.stdout.length, 64 << 10, "stdout length");
});

Deno.test("an endless program is killed at its deadline and the next run works", async () => {
  const r = await run("for (;;) {}", { timeoutMs: 300 });
  eq([r.exitCode, r.timedOut], [-1, true], "endless");
  const again = await run(`console.log("ok")`);
  eq(again.stdout, "ok\n", "again");
});

Deno.test("kill ends a run early", async () => {
  const pending = run("await new Promise(() => {});", { timeoutMs: 60_000 });
  pending.kill();
  eq((await pending).timedOut, true, "killed");
});

Deno.test("node: imports of the bundled actions resolve to shims", async () => {
  eq(rewriteNodeImports(`import x from "node:crypto";`).startsWith(`import x from "data:text/javascript,`), true, "rewrite");
  const r = await run(`import { Buffer } from "node:buffer";
import crypto from "node:crypto";
import * as nc from "node:crypto";
import * as dns from "node:dns/promises";
const b = Buffer.concat([Buffer.from("ab"), Buffer.from(new Uint8Array([99]))]);
let txt = "resolved";
try { await dns.resolveTxt("_atproto.example.com"); } catch (e) { txt = e.code; }
console.log(b.toString("utf8"), b.length, "webcrypto" in nc, typeof crypto.createHash, crypto.randomBytes(4).length, txt);`);
  eq([r.exitCode, r.stdout], [0, "abc 3 true function 4 ENOTFOUND\n"], "shims");
  const missing = await run(`import fs from "node:fs"; console.log("loaded");`);
  eq(missing.exitCode, 1, "unknown node module fails");
});

Deno.test("frame srcdoc is a sandboxed relay with a CSP", () => {
  const html = frameSrcdoc();
  assert(html.includes(`http-equiv="Content-Security-Policy"`), "csp");
  assert(html.includes("relay(65536)"), "relay call");
  assert(!html.slice(0, -"</script>".length).includes("</script>"), "no early script end");
});

// connect + probe against an in-process frame (MessageChannel): Deno has one
// origin, so storage is visible here and the probe must fail closed.
Deno.test("probe fails closed when the sandbox shares the page's storage", async () => {
  const { port1, port2 } = new MessageChannel();
  port2.onmessage = async ({ data }) => {
    if (data.frame) return port2.postMessage({ id: data.id, origin: "null", parentDocument: "blocked" });
    port2.postMessage({ id: data.id, ...(await runInWorker(data)) });
  };
  const bridge = connect(port1);
  const out = JSON.parse(await bridge.probe());
  port1.close();
  port2.close();
  eq(out.fenced, false, `fenced (${out.message})`);
  assert(/origin=\S+ /.test(out.message) && out.message.includes("timeout=killed") && out.message.includes("rerun=ok"), out.message);
});

Deno.test("probe passes on the facts of a fenced sandbox", async () => {
  const fenced = { origin: "null", shim: "ok", dom: "none", idb: "denied:SecurityError", cache: "denied:SecurityError", opfs: "denied:SecurityError" };
  const out = await probe({
    run: async ({ source, timeoutMs }) => {
      if (source.startsWith("for")) return { exitCode: -1, stdout: "", stderr: "", timedOut: timeoutMs > 0 };
      if (source.includes("console.log(\"ok\")")) return { exitCode: 0, stdout: "ok\n", stderr: "", timedOut: false };
      return { exitCode: 0, stdout: JSON.stringify(fenced) + "\n", stderr: "", timedOut: false };
    },
    frame: async () => ({ origin: "null", parentDocument: "blocked" }),
  });
  eq(out.fenced, true, `fenced (${out.message})`);
});

// The bundled requester-tangled-vouch action under socialweb's action shim,
// with trust seeded as the run reconciler does (cache trust/<counterparty> =
// trust.Snapshot.CacheFiles()). The DIDs do not exist, so the verdicts can
// only come from the seeded snapshot. Needs the build's socialweb mirror
// (scripts/browser-dev/build-page.sh); skipped without it.
const mirror = new URL("../../.work/socialweb-mirror/internal/policyengine/", import.meta.url);
const haveMirror = await Deno.stat(new URL("workflow/action.go", mirror)).then(() => true, () => false);

async function tangledVouch(vouched, mode) {
  const go = await Deno.readTextFile(new URL("workflow/action.go", mirror));
  const shim = go.match(/const actionShim = `([\s\S]*?)`/)[1];
  const footer = go.match(/const actionFooter = `([\s\S]*?)`/)[1];
  const action = await Deno.readTextFile(new URL("bundled-actions/tangy/policy-requester-tangled-vouch/dist/index.js", mirror));
  const self = "did:plc:fenceself00000000000000", operator = "did:plc:fenceoperator000000000000", bidder = "did:plc:fencebidder0000000000000";
  const file = (value) => ({ data: JSON.stringify(value), encoding: "text" });
  const cache = {
    [`trust/${bidder}`]: {
      "operatorOf.json": file({ [self]: self, [bidder]: operator }),
      "vouchedBy.json": file({ [self]: vouched ? [operator] : [] }),
      "trustedOperators.json": file([]),
    },
  };
  const env = {
    GITHUB_OUTPUT: "/gh/output", GITHUB_ENV: "/gh/env", GITHUB_CACHE: "/gh/cache", GITHUB_EVENT_PATH: "/gh/event",
    "INPUT_SELF-DID": self, "INPUT_COUNTERPARTY-DID": bidder, "INPUT_SUBJECT-DID": bidder, "INPUT_ROOT-REQUESTER-DID": self,
    "INPUT_MODE": mode,
  };
  const source = shim.replace("__PE_ENV_JSON__", JSON.stringify(env))
    .replace("__PE_CACHE_JSON__", JSON.stringify(JSON.stringify(cache)))
    .replace("__PE_EVENT_JSON__", JSON.stringify(JSON.stringify({ inputs: {} })))
    .replaceAll("__PE_RESULT_MARKER__", JSON.stringify("__PE_RESULT__")) + "\n" + action + "\n" + footer;
  const r = await runInWorker({ source, env, timeoutMs: 30_000 });
  const line = r.stdout.split("\n").find((l) => l.startsWith("__PE_RESULT__"));
  assert(line, `no result line: ${r.stderr.slice(-500)}`);
  const result = JSON.parse(line.slice("__PE_RESULT__".length));
  return { code: result.code, output: result.vfs["/gh/output"] ?? "", stderr: r.stderr };
}

// scope: the requester's bid filter (accept.go); full: its winner check (runner.go).
for (const mode of ["scope", "full"]) {
  Deno.test({ name: `tangled-vouch ${mode}: a seeded vouch allows`, ignore: !haveMirror, fn: async () => {
    const r = await tangledVouch(true, mode);
    eq(r.code, 0, "code");
    assert(r.output.includes("allow=true"), r.output + r.stderr.slice(-300));
  } });
  Deno.test({ name: `tangled-vouch ${mode}: a seeded snapshot without the vouch denies`, ignore: !haveMirror, fn: async () => {
    const r = await tangledVouch(false, mode);
    eq(r.code, 0, "code");
    assert(r.output.includes("allow=false"), r.output + r.stderr.slice(-300));
  } });
}
