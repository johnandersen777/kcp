// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import { etagMatches } from "./serve-public.ts";

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
