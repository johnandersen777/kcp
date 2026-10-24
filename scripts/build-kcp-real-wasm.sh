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

# Builds the real kcp server path (cmd/kcp with the kcprealserver tag) for
# js/wasm, with unix-only dependencies patched for the wasm runtime.
#
# This is experimental: the resulting binary compiles the full control plane
# and uses the memory:// storage backend, but wasm has no listener; see
# contrib/wasm/README.md.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out_dir="${1:-${repo_root}/bin/wasm}"
mkdir -p "${out_dir}"

"${repo_root}/scripts/prepare-kcp-patched-modules.sh"

echo "Building the real kcp server for js/wasm..."
(
  cd "${repo_root}"
  GOOS=js GOARCH=wasm go build \
    -modfile="${repo_root}/.go.memory.mod" \
    -tags kcprealserver \
    -o "${out_dir}/kcp-real.wasm" \
    ./cmd/kcp
)

cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" "${out_dir}/wasm_exec.js"
echo "Built ${out_dir}/kcp-real.wasm"
