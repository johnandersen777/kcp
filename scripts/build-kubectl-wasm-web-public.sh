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
# serves: the page, worker scripts, WASM modules, wasm_exec scripts and the
# socialweb OAuth popup shell. Serve the directory with:
#
#   deno run --allow-read --allow-net scripts/serve-public.ts --dir=public
#
# or with any static file server, for example:
#
#   python3 -m http.server -d public 8090
#
# /config is written as a static JSON file. serve-public.ts also generates it
# when missing and serves /oauth-client-metadata.json from the request Host
# header; the Deno server's dynamic /echo and /demo.yaml routes are not part of
# the bundle.
#
# The WASM modules ship only as gzip -9 `<module>.wasm.gz` files, which the
# workers inflate with DecompressionStream. Static hosts such as wisp.place
# cap files at 200 MB raw (kcp.wasm is ~282 MB) and sites at 300 MB, and never
# gzip application/wasm themselves; the four .gz files total ~78 MB.
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
  oauth_refresh_worker.js
  kubectl_worker.js
  cli_worker.js
  fs_shim.js
  oauth-callback.html
  oauth.js
  identity_ui.js
  state_store.js
  wasm_fetch.js
  ingress_ws.js
  ingress_manager.js
  vmshell.js
  webrtc_guest.js
  ssh_worker.js
  ssh_cmd.js
  wasm_snapshot.js
  kcp_hibernate.js
  debug_link.js
)
wasm_modules=(
  kcp.wasm
  kubectl.wasm
  kubectl-slim.wasm
  goat.wasm
  yq.wasm
  ssh.wasm
)
modules=(
  "${wasm_modules[@]}"
  wasm_exec.js
  wasm_exec_cli.js
)

for file in "${modules[@]}"; do
  if [[ ! -f "${artifacts}/${file}" ]]; then
    echo "missing ${artifacts}/${file}" >&2
    echo "run: make build-kcp-wasm build-kubectl-wasm build-goat-wasm build-yq-wasm build-sshclient-wasm" >&2
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

cp "${artifacts}/wasm_exec.js" "${artifacts}/wasm_exec_cli.js" "${out_dir}/"
pids=()
for file in "${wasm_modules[@]}"; do
  gzip -9 -n -c "${artifacts}/${file}" > "${out_dir}/${file}.gz" &
  pids+=("$!")
done
for pid in "${pids[@]}"; do
  wait "${pid}"
done

# serve-public.ts serves the OAuth client metadata scope from this file.
if [[ -f "${artifacts}/requester-scope.txt" ]]; then
  cp "${artifacts}/requester-scope.txt" "${out_dir}/requester-scope.txt"
fi

printf '%s\n' '{"apiServer":"http://kcp.local","intervalMs":1500}' \
  > "${out_dir}/config"

echo "Built ${out_dir}:"
ls -lh "${out_dir}"
