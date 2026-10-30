// Browser check for the fenced policy sandbox (scripts/browser/policy_sandbox.js,
// plan 0037), part of the policy suite (scripts/browser-dev/policy-suite.sh):
// serves the module and a bare page, loads it in headless Chrome (no kcp, no
// sign-in), plants a secret in the page's IndexedDB, starts the sandbox frame,
// runs programs that try to reach the page (fetch, IndexedDB, parent DOM,
// storage), then the fence probe. Exits 1 unless every scenario passes.
//   deno run -A scripts/browser-dev/policy-sandbox-check.ts [chrome path]
import puppeteer from "npm:puppeteer-core@23.11.1";

const chrome = Deno.args[0] ?? Deno.env.get("CHROME") ??
  (Deno.build.os === "darwin" ? "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" : "/usr/bin/google-chrome");
const moduleURL = new URL("../browser/policy_sandbox.js", import.meta.url);

const page = `<!doctype html><body><script type="module">
import { connect, connectFrame, startFrame } from "/policy_sandbox.js";
const secret = "kcp-page-secret-" + crypto.randomUUID();
await new Promise((resolve, reject) => {
  const r = indexedDB.open("kcp-page-secret");
  r.onupgradeneeded = () => r.result.createObjectStore("s");
  r.onsuccess = () => { const tx = r.result.transaction("s", "readwrite"); tx.objectStore("s").put(secret, "k"); tx.oncomplete = resolve; tx.onerror = reject; };
  r.onerror = reject;
});
localStorage.setItem("kcp-page-secret", secret);
const bridge = connect(connectFrame(await startFrame()));
const run = async (source) => JSON.parse(await bridge.run(JSON.stringify({ id: crypto.randomUUID(), source, env: {}, timeoutMs: 10000 })));
window.__result = {
  trivial: await run('console.log("ok")'),
  node: await run('import { Buffer } from "node:buffer"; console.log(Buffer.from("ok").toString())'),
  // The page's own (http) origin is unreachable from the sandbox.
  http: await run('try { await fetch("' + location.origin + '/"); console.log("FETCHED"); } catch (e) { console.log(e.name); }'),
  idb: await run('try { console.log(await new Promise((res, rej) => { const r = indexedDB.open("kcp-page-secret"); r.onsuccess = () => { try { const g = r.result.transaction("s").objectStore("s").get("k"); g.onsuccess = () => res(String(g.result)); g.onerror = () => rej(g.error); } catch (e) { rej(e); } }; r.onerror = () => rej(r.error); })); } catch (e) { console.log(e.name); }'),
  dom: await run('console.log([typeof parent, typeof top, typeof document, typeof localStorage, typeof window].join()); try { console.log(parent.document.body.innerHTML); } catch (e) { console.log(e.name); }'),
  secret,
  probe: JSON.parse(await bridge.probe()),
};
</script>`;

const server = Deno.serve({ hostname: "127.0.0.1", port: 0, onListen() {} }, async (req) => {
  const { pathname } = new URL(req.url);
  if (pathname === "/policy_sandbox.js") {
    return new Response(await Deno.readTextFile(moduleURL), { headers: { "content-type": "text/javascript" } });
  }
  return new Response(page, { headers: { "content-type": "text/html" } });
});

const browser = await puppeteer.launch({ executablePath: chrome, headless: true, args: ["--no-sandbox"] });
let ok = false;
try {
  const tab = await browser.newPage();
  tab.on("console", (m) => console.log(`console ${m.type()}: ${m.text()}`));
  tab.on("pageerror", (e) => console.log(`pageerror: ${e}`));
  await tab.goto(`http://127.0.0.1:${server.addr.port}/`);
  const result = await tab.waitForFunction(() => (window as any).__result, { timeout: 60_000 }).then((h) => h.jsonValue()) as any;
  console.log(JSON.stringify(result, null, 2));
  const facts = Object.fromEntries(result.probe.message.split(" ").map((kv: string) => kv.split("=")));
  const checks: [string, boolean][] = [
    ["sandbox-runs (trivial program)", result.trivial.exitCode === 0 && result.trivial.stdout === "ok\n"],
    ["sandbox-runs (node: shim)", result.node.exitCode === 0 && result.node.stdout === "ok\n"],
    ["fence-fetch-page-origin-deny", result.http.stdout === "TypeError\n"],
    ["fence-page-idb-deny", !result.idb.stdout.includes(result.secret) && /Error\n$/.test(result.idb.stdout)],
    ["fence-parent-dom-deny", result.dom.stdout === "undefined,undefined,undefined,undefined,undefined\nReferenceError\n"],
    ["fence-origin-null", facts.origin === "null" && facts.frameOrigin === "null"],
    ["fence-storage-denied (idb, cache, opfs)", ["idb", "cache", "opfs"].every((k) => String(facts[k]).startsWith("denied:"))],
    ["fence-broadcast-silent", facts.bc === "silent"],
    ["fence-parent-blocked", facts.parent === "blocked"],
    ["timeout-deny (endless program killed)", facts.timeout === "killed"],
    ["fence-rerun-ok", facts.rerun === "ok"],
    ["sandbox-fenced (probe verdict)", result.probe.fenced === true],
  ];
  for (const [name, pass] of checks) console.log(`${pass ? "ok  " : "FAIL"} ${name}`);
  ok = checks.every(([, pass]) => pass);
} finally {
  await browser.close();
  await server.shutdown();
}
console.log(ok ? "PASS" : "FAIL");
Deno.exit(ok ? 0 : 1);
