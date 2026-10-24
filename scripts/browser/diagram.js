/*
Copyright 2026 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Builds the Mermaid source for the live Pele diagram from `kubectl get crds
// -o json` and `kubectl get fetches -o json` output.

function label(text) {
  return String(text).replace(/"/g, "'").replace(/\s+/g, " ").trim();
}

export function nodeID(prefix, name) {
  return `${prefix}_${String(name).replace(/[^A-Za-z0-9_]/g, "_")}`;
}

export function buildDiagram(apiServer, crds, fetches) {
  const lines = ["flowchart LR", `  api["kcp-wasm<br/>${label(apiServer)}"]`];
  const crdIDs = new Map();

  for (const item of crds ?? []) {
    const name = item?.metadata?.name;
    if (!name) continue;
    const id = nodeID("crd", name);
    crdIDs.set(name, id);
    lines.push(`  api --> ${id}["CRD ${label(name)}"]`);
  }

  const fetchCRD = crdIDs.get("fetches.fetch.kcp.io") ?? "api";
  const items = fetches ?? [];
  const classes = [];

  for (const item of items.slice(0, 12)) {
    const name = item?.metadata?.name;
    if (!name) continue;
    const id = nodeID("fetch", name);
    const phase = item?.status?.phase ?? "Unresolved";
    const code = item?.status?.statusCode ? ` · ${item.status.statusCode}` : "";
    const body = String(item?.status?.body ?? "").slice(0, 32);
    const detail = body ? `<br/>${label(body)}` : "";
    lines.push(
      `  ${fetchCRD} --> ${id}["Fetch ${label(name)}<br/>${label(phase)}${code}${detail}"]`,
    );
    classes.push(
      `  class ${id} ${
        phase === "Resolved" ? "resolved" : phase === "Error" ? "error" : "pending"
      }`,
    );
  }

  if (items.length > 12) {
    lines.push(`  ${fetchCRD} -.-> more["+${items.length - 12} more Fetches"]`);
  }
  if (items.length === 0) {
    lines.push('  none["no Fetch resources"]');
    if (fetchCRD !== "api") lines.push(`  ${fetchCRD} --> none`);
  }

  lines.push(
    "  classDef resolved fill:#14301d,stroke:#9ece6a,color:#d5dbe5",
    "  classDef error fill:#3a1717,stroke:#ef8b8b,color:#d5dbe5",
    "  classDef pending fill:#332a12,stroke:#e0af68,color:#d5dbe5",
    ...classes,
  );

  return lines.join("\n");
}
