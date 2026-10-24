// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

import { buildDiagram } from "./diagram.js";

function assertEquals(actual, expected) {
  if (actual !== expected) {
    throw new Error(`expected:\n${expected}\nactual:\n${actual}`);
  }
}

Deno.test("buildDiagram maps CRDs and Fetch statuses", () => {
  const mermaid = buildDiagram(
    "http://kcp.local",
    [{ metadata: { name: "fetches.fetch.kcp.io" } }],
    [
      {
        metadata: { name: "demo" },
        status: { phase: "Resolved", statusCode: 200, body: "hello world" },
      },
      { metadata: { name: "pending" }, status: {} },
    ],
  );

  assertEquals(
    mermaid,
    [
      "flowchart LR",
      '  api["kcp-wasm<br/>http://kcp.local"]',
      '  api --> crd_fetches_fetch_kcp_io["CRD fetches.fetch.kcp.io"]',
      '  crd_fetches_fetch_kcp_io --> fetch_demo["Fetch demo<br/>Resolved · 200<br/>hello world"]',
      '  crd_fetches_fetch_kcp_io --> fetch_pending["Fetch pending<br/>Unresolved"]',
      "  classDef resolved fill:#14301d,stroke:#9ece6a,color:#d5dbe5",
      "  classDef error fill:#3a1717,stroke:#ef8b8b,color:#d5dbe5",
      "  classDef pending fill:#332a12,stroke:#e0af68,color:#d5dbe5",
      "  class fetch_demo resolved",
      "  class fetch_pending pending",
    ].join("\n"),
  );
});

Deno.test("buildDiagram handles an empty cluster", () => {
  const mermaid = buildDiagram("http://kcp.local", [], []);
  assertEquals(
    mermaid,
    [
      "flowchart LR",
      '  api["kcp-wasm<br/>http://kcp.local"]',
      '  none["no Fetch resources"]',
      "  classDef resolved fill:#14301d,stroke:#9ece6a,color:#d5dbe5",
      "  classDef error fill:#3a1717,stroke:#ef8b8b,color:#d5dbe5",
      "  classDef pending fill:#332a12,stroke:#e0af68,color:#d5dbe5",
    ].join("\n"),
  );
});
