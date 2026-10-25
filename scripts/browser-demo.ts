// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

/**
 * Drive the browser page through the VM demo and record it.
 *
 * Needs the dev server started with --debug-log=<log> and
 * --debug-control=<control> (scripts/kubectl_wasm_web.ts). Run this, then load
 * the page (on a phone, say): the program attaches to the first page that
 * connects its debug control after it started, and then
 *
 *   waits for kcp, the signed-in MarketIdentity and the requester ingress,
 *   releases VMs left from earlier runs,
 *   taps Request VM (the page itself runs kubectl get computecontracts),
 *   waits for Held, runs ssh <vm> (over WebRTC) in the page terminal,
 *   types each --cmd and waits for its output,
 *   exits, releases the VM and runs kubectl until it shows Released.
 *
 * Every step waits for the page's own output (the debug log), never a fixed
 * sleep. The page's lines, a step timeline and the terminal transcript are
 * written to --out/<time>-<session>.{log,timeline.txt,term.txt}.
 *
 *   deno run --allow-read --allow-write scripts/browser-demo.ts \
 *     --log=/tmp/claude-kcpwasm/debug.log --control=/tmp/claude-kcpwasm/control.ndjson \
 *     --out=demos [--ua=iPhone] [--session=<id>] [--cmd='uname -a' ...]
 */

const args = new Map<string, string[]>();
for (const a of Deno.args) {
  const m = a.match(/^--([^=]+)=(.*)$/s);
  if (m) args.set(m[1], [...(args.get(m[1]) ?? []), m[2]]);
}
const opt = (k: string, d = "") => args.get(k)?.[0] ?? d;
const logPath = opt("log", "/tmp/claude-kcpwasm/debug.log");
const controlPath = opt("control", "/tmp/claude-kcpwasm/control.ndjson");
const outDir = opt("out", "demos");
const uaFilter = opt("ua", "");
const fixedSession = opt("session", "");
const commands = args.get("cmd") ?? ['PS1="$ "', "ip a", "uname -a"];
// What each default command's output looks like; other commands wait for
// their echo plus a quiet moment.
const expect: Record<string, RegExp> = {
  "ip a": /inet \d+\.\d+\.\d+\.\d+/,
  "uname -a": /^Linux /,
};

interface Line {
  t: string;
  session: string;
  ua: string;
  level: string;
  message: string;
}
const lineRE = /^(\S+) \[(\S+) ?([^\]]*)\] (\S+) (.*)$/;

// Follow the debug log from its current end, polling every 50 ms.
async function* follow(path: string): AsyncGenerator<Line> {
  let offset = (await Deno.stat(path)).size;
  let partial = "";
  while (true) {
    const size = (await Deno.stat(path)).size;
    if (size < offset) offset = 0;
    if (size > offset) {
      const f = await Deno.open(path);
      await f.seek(offset, Deno.SeekMode.Start);
      const buf = new Uint8Array(size - offset);
      let n = 0;
      while (n < buf.length) {
        const r = await f.read(buf.subarray(n));
        if (r === null) break;
        n += r;
      }
      f.close();
      offset += n;
      const parts = (partial + new TextDecoder().decode(buf.subarray(0, n))).split("\n");
      partial = parts.pop() ?? "";
      for (const p of parts) {
        const m = p.match(lineRE);
        if (m) {
          yield {
            t: m[1],
            session: m[2],
            ua: m[3],
            level: m[4],
            message: m[5].replaceAll("\\n", "\n"),
          };
        }
      }
    }
    await new Promise((r) => setTimeout(r, 50));
  }
}

const started = Date.now();
const stamp = new Date().toISOString().replace(/[:.]/g, "-");
let session = fixedSession;
const seen: Line[] = [];
const waiters: { pred: (l: Line) => boolean; resolve: (l: Line) => void }[] = [];
const timeline: string[] = [];
const transcript: string[] = [];

const tail = follow(logPath);
(async () => {
  for await (const l of tail) {
    if (!session) {
      if (/debug control connected for session/.test(l.message) && (!uaFilter || l.ua.includes(uaFilter))) {
        session = l.session;
        note(`attached to session ${session} (${l.ua})`);
      } else continue;
    }
    if (l.session !== session) continue;
    seen.push(l);
    if (l.level === "term") transcript.push(l.message);
    for (const w of [...waiters]) {
      if (w.pred(l)) {
        waiters.splice(waiters.indexOf(w), 1);
        w.resolve(l);
      }
    }
  }
})();

function note(text: string) {
  const line = `${new Date().toISOString().slice(11, 23)}  +${
    ((Date.now() - started) / 1000).toFixed(1).padStart(6)
  }s  ${text}`;
  timeline.push(line);
  console.log(line);
}

// The next page line (after now) that pred accepts.
function next(pred: (l: Line) => boolean, timeoutMs: number, what: string): Promise<Line> {
  return new Promise((resolve, reject) => {
    const w = { pred, resolve: (l: Line) => (clearTimeout(timer), resolve(l)) };
    const timer = setTimeout(() => {
      waiters.splice(waiters.indexOf(w), 1);
      reject(new Error(`timed out after ${timeoutMs / 1000}s waiting for ${what}`));
    }, timeoutMs);
    waiters.push(w);
  });
}

// A page line already seen or still to come.
function have(pred: (l: Line) => boolean, timeoutMs: number, what: string): Promise<Line> {
  const hit = seen.find(pred);
  return hit ? Promise.resolve(hit) : next(pred, timeoutMs, what);
}

let cmdSeq = 0;
// Run js (an async function body with the page's debug handle d) in the page.
async function page(js: string, timeoutMs = 30_000): Promise<string> {
  const id = `demo${Date.now().toString(36)}${cmdSeq++}`;
  const done = next((l) => l.level === "result" && l.message.startsWith(`${id} `), timeoutMs, `result ${id}`);
  await Deno.writeTextFile(controlPath, JSON.stringify({ session, id, js, ts: new Date().toISOString() }) + "\n", {
    append: true,
  });
  const r = (await done).message.slice(id.length + 1);
  if (r.startsWith("error ")) throw new Error(`page: ${r.slice(6)}`);
  return r.replace(/^ok /, "");
}

const msg = (re: RegExp) => (l: Line) => re.test(l.message);
const term = (re: RegExp) => (l: Line) => l.level === "term" && re.test(l.message);
const API = "/apis/socialweb.computer/v1alpha1/computecontracts";

async function main() {
  await Deno.mkdir(outDir, { recursive: true });
  if (!session) note(`waiting for a page to connect${uaFilter ? ` (${uaFilter})` : ""}: load it now`);
  while (!session) await new Promise((r) => setTimeout(r, 100));

  await have(msg(/kcp ready in/), 300_000, "kcp ready");
  note("kcp ready");
  const id = await have(
    msg(/MarketIdentity (Ready|SessionExpired|AwaitingAuthorization|Failed)/),
    300_000,
    "the identity",
  );
  if (!/MarketIdentity Ready/.test(id.message)) {
    note("not signed in: sign in on the page; waiting");
    await next(msg(/MarketIdentity Ready/), 600_000, "a signed-in identity");
  }
  note("signed in");
  await have(msg(/ingress .* registered at/), 300_000, "the requester ingress");
  note("ingress registered");

  const left = JSON.parse(
    await page(
      `const act = (d.state()?.contracts ?? []).filter((c) => !c.release && !["Released", "Failed"].includes(c.phase));
     for (const c of act) await d.rest("PATCH", "${API}/" + c.name, {spec: {release: true}}, "application/merge-patch+json");
     return act.map((c) => c.name);`,
    ),
  );
  for (const name of left) {
    note(`releasing ${name} from an earlier run`);
    await have(msg(new RegExp(`ComputeContract ${name} Released`)), 300_000, `${name} Released`);
  }

  const req = next(msg(/requesting ComputeContract vmshell-/), 30_000, "the request");
  const clicked = await page(
    `const b = document.querySelector("#compute-request");
     if (b.disabled) return "disabled: " + d.text("#compute-status");
     b.click(); return "clicked";`,
  );
  if (clicked !== "clicked") throw new Error(`Request VM: ${clicked}`);
  const vm = (await req).message.match(/ComputeContract (vmshell-[a-z0-9]+)/)![1];
  note(`Request VM: ${vm}`);
  await next(term(new RegExp(vm)), 60_000, "kubectl in the terminal");
  note("kubectl get computecontracts shows it in flight");
  await have(msg(new RegExp(`ComputeContract ${vm} Held`)), 600_000, `${vm} Held`);
  note(`${vm} Held`);

  const shell = next(term(/root@|did not open|Could not|refused|denied/), 90_000, "an ssh prompt");
  await page(`d.run("ssh ${vm}"); return "sent";`);
  note(`ssh ${vm}`);
  const got = await shell;
  if (!/root@/.test(got.message)) {
    note(`ssh failed: ${got.message}`);
  } else {
    note("logged in on the guest");
    for (const cmd of commands) {
      const echoed = next(term(new RegExp(cmd.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"))), 20_000, `echo of ${cmd}`);
      const out = expect[cmd] ? next(term(expect[cmd]), 20_000, `output of ${cmd}`) : null;
      await page(`d.type(${JSON.stringify(cmd + "\r")}); return "typed";`);
      await echoed;
      if (out) await out;
      else await new Promise((r) => setTimeout(r, 400));
      note(`$ ${cmd}`);
    }
    const local = next(term(/logout|Connection to|closed|\/home\/user \$/), 20_000, "the local prompt");
    await page(`d.type("exit\\r"); return "typed";`);
    await local.catch(() => {});
    note("exit");
  }

  await page(
    `await d.rest("PATCH", "${API}/${vm}", {spec: {release: true}}, "application/merge-patch+json"); return "asked";`,
  );
  note(`release ${vm}`);
  for (let i = 0; i < 30; i++) {
    const row = next(term(new RegExp(`^${vm}\\s`)), 30_000, "the kubectl row");
    await page(`d.run("kubectl get computecontracts"); return "sent";`);
    const line = (await row).message.replace(/\s+/g, " ");
    note(`kubectl: ${line}`);
    if (/ Released /.test(line)) break;
    await next(msg(new RegExp(`ComputeContract ${vm} `)), 15_000, `${vm} status change`).catch(() => {});
  }
  note("done");
}

let failed = false;
try {
  await main();
} catch (err) {
  failed = true;
  note(`FAILED: ${err instanceof Error ? err.message : err}`);
}
const base = `${outDir}/${stamp}-${session || "nosession"}`;
await Deno.writeTextFile(`${base}.timeline.txt`, timeline.join("\n") + "\n");
await Deno.writeTextFile(`${base}.term.txt`, transcript.join("\n") + "\n");
await Deno.writeTextFile(`${base}.log`, seen.map((l) => `${l.t} ${l.level} ${l.message}`).join("\n") + "\n");
console.log(`wrote ${base}.{timeline.txt,term.txt,log}`);
Deno.exit(failed ? 1 : 0);
