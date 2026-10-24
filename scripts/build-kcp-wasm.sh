#!/usr/bin/env bash

# Copyright 2026 The KCP Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

# Builds the kcp WASM shard (contrib/wasm) and stages Go's wasm_exec.js next
# to it, ready to be run by scripts/kcp_wasm.ts.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out_dir="${1:-${repo_root}/bin/wasm}"

mkdir -p "${out_dir}"

echo "Building kcp for js/wasm (the kcp command's wasm build mode)..."
(
  cd "${repo_root}"
  GOOS=js GOARCH=wasm go build -o "${out_dir}/kcp.wasm" ./cmd/kcp
)

# Export Go's mutable globals so the browser worker can hibernate the
# instance into a per-user memory snapshot (scripts/browser/wasm_snapshot.js).
deno run --allow-read --allow-write "${repo_root}/scripts/wasm_globals.js" "${out_dir}/kcp.wasm"

goroot="$(go env GOROOT)"
cp "${goroot}/lib/wasm/wasm_exec.js" "${out_dir}/wasm_exec.js"

# ~282 MB becomes ~43 MB; kubectl_wasm_web.ts and the browser workers
# prefer this sibling.
gzip -9 -n -c "${out_dir}/kcp.wasm" > "${out_dir}/kcp.wasm.gz"

echo "Built:"
ls -lh "${out_dir}/kcp.wasm" "${out_dir}/wasm_exec.js"
