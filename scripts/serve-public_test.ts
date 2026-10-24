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
