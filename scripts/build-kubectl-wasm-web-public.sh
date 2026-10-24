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

# Builds ./public, the static browser demo that scripts/kubectl_wasm_web.ts
# serves: the page, worker scripts, WASM modules and wasm_exec scripts. Serve
# the directory with any static file server, for example:
#
#   python3 -m http.server -d public 8090
#
# /config is written as a static JSON file. The Deno server's dynamic /echo
# and /demo.yaml routes are not part of the bundle; the page does not request
# them at runtime.
#
# Usage: scripts/build-kubectl-wasm-web-public.sh [outdir] [artifacts]
#   outdir     default: <repo>/public
#   artifacts  default: <repo>/bin/wasm (run the build-*-wasm targets first)

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out_dir="${1:-${repo_root}/public}"
artifacts="${2:-${repo_root}/bin/wasm}"

if [[ "${out_dir}" == "/" || "${out_dir}" == "${repo_root}" ]]; then
  echo "refusing to replace ${out_dir}" >&2
  exit 1
fi

assets=(
  app.html
  app.js
  diagram.js
  kcp_worker.js
  kubectl_worker.js
  cli_worker.js
  fs_shim.js
)
modules=(
  kcp.wasm
  kubectl.wasm
  goat.wasm
  yq.wasm
  wasm_exec.js
  wasm_exec_cli.js
)

for file in "${modules[@]}"; do
  if [[ ! -f "${artifacts}/${file}" ]]; then
    echo "missing ${artifacts}/${file}" >&2
    echo "run: make build-kcp-wasm build-kubectl-wasm build-goat-wasm build-yq-wasm" >&2
    exit 1
  fi
done

rm -rf "${out_dir}"
mkdir -p "${out_dir}"

for file in "${assets[@]}"; do
  cp "${repo_root}/scripts/browser/${file}" "${out_dir}/${file}"
done
# The Deno server maps both "/" and "/app.html" to the page; a static server
# needs index.html for "/".
cp "${out_dir}/app.html" "${out_dir}/index.html"

for file in "${modules[@]}"; do
  cp "${artifacts}/${file}" "${out_dir}/${file}"
done

printf '%s\n' '{"apiServer":"http://kcp.local","intervalMs":1500}' \
  > "${out_dir}/config"

echo "Built ${out_dir}:"
ls -lh "${out_dir}"
