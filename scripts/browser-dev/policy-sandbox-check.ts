// Browser check for the fenced policy sandbox (scripts/browser/policy_sandbox.js,
// plan 0037): serves the module and a bare page, loads it in headless Chrome
// (no kcp, no sign-in), starts the sandbox frame, runs a trivial program and
// one with a node: import, then the fence probe. Exits 1 unless all pass.
//   deno run -A scripts/browser-dev/policy-sandbox-check.ts [chrome path]
import puppeteer from "npm:puppeteer-core@23.11.1";

const chrome = Deno.args[0] ?? Deno.env.get("CHROME") ??
  (Deno.build.os === "darwin" ? "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" : "/usr/bin/google-chrome");
const moduleURL = new URL("../browser/policy_sandbox.js", import.meta.url);

const page = `<!doctype html><body><script type="module">
import { connect, connectFrame, startFrame } from "/policy_sandbox.js";
const bridge = connect(connectFrame(await startFrame()));
const run = async (source) => JSON.parse(await bridge.run(JSON.stringify({ id: crypto.randomUUID(), source, env: {}, timeoutMs: 10000 })));
window.__result = {
  trivial: await run('console.log("ok")'),
  node: await run('import { Buffer } from "node:buffer"; console.log(Buffer.from("ok").toString())'),
  // The page's own (http) origin is unreachable from the sandbox.
  http: await run('try { await fetch("' + location.origin + '/"); console.log("FETCHED"); } catch (e) { console.log(e.name); }'),
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
  ok = result.trivial.exitCode === 0 && result.trivial.stdout === "ok\n" &&
    result.node.exitCode === 0 && result.node.stdout === "ok\n" && result.http.stdout === "TypeError\n" &&
    result.probe.fenced === true;
} finally {
  await browser.close();
  await server.shutdown();
}
console.log(ok ? "PASS" : "FAIL");
Deno.exit(ok ? 0 : 1);
