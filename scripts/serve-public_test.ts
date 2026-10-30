// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import { DEFAULT_SCOPE, etagMatches, loadScope } from "./serve-public.ts";

Deno.test("etagMatches handles lists, weak tags and *", () => {
  const etag = '"10-abc"';
  const cases: [string | null, boolean][] = [
    [null, false],
    ['"10-abc"', true],
    ['W/"10-abc"', true],
    ['"x", "10-abc"', true],
    ["*", true],
    ['"10-abd"', false],
  ];
  for (const [header, want] of cases) {
    if (etagMatches(header, etag) !== want) throw new Error(`${header}: want ${want}`);
  }
});

Deno.test("loadScope reads the generated scope file and falls back without it", async () => {
  const dir = await Deno.makeTempDir();
  try {
    const file = `${dir}/requester-scope.txt`;
    if (await loadScope(file) !== DEFAULT_SCOPE) throw new Error("missing file should fall back");
    for (const want of [
      "rpc:chat.atirc.mint?aud=did:web:irc.fedfork.com%23atirc",
      "repo:com.publicdomainrelay.temp.compute.events.guest.agentStart?action=create&action=update",
    ]) {
      if (!DEFAULT_SCOPE.split(" ").includes(want)) throw new Error(`the fallback scope lacks ${want}`);
    }
    await Deno.writeTextFile(file, "atproto\n  repo:a?action=create \n");
    const got = await loadScope(file);
    if (got !== "atproto repo:a?action=create") throw new Error(`read ${got}`);
    let threw = false;
    await loadScope(`${dir}/missing.txt`, true).catch(() => (threw = true));
    if (!threw) throw new Error("a required missing file should throw");
  } finally {
    await Deno.remove(dir, { recursive: true });
  }
});

// The fallback must be the generated scope, token for token, or a host
// without requester-scope.txt serves metadata the PDS rejects tokens against.
const built = new URL("../.work/wasm/requester-scope.txt", import.meta.url);
const haveBuilt = await Deno.stat(built).then(() => true, () => false);
Deno.test({ name: "the fallback scope equals the built requester-scope.txt", ignore: !haveBuilt, fn: async () => {
  const want = (await Deno.readTextFile(built)).trim().split(/\s+/).join(" ");
  if (DEFAULT_SCOPE !== want) throw new Error(`fallback differs from ${built.pathname}:\nwant ${want}\ngot  ${DEFAULT_SCOPE}`);
} });
