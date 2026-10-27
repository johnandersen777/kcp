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

# Verifies the browser demo setup:
#   * both WASM modules and the browser workers are served
#   * /config, /demo.yaml and /echo are present
#   * the diagram builder passes its unit tests
#
# The full path (kcp.wasm + kubectl.wasm in Web Workers, xterm.js terminal,
# Pele diagram) needs a browser; run scripts/kubectl_wasm_web.ts and open the
# page to exercise it.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
port="${WEB_PORT:-18086}"
host="${HOST:-127.0.0.1}"
workdir="$(mktemp -d "${TMPDIR:-/tmp}/kubectl-wasm-web-verify.XXXXXX")"
web_pid=""

cleanup() {
  if [[ -n "${web_pid}" ]] && kill -0 "${web_pid}" 2>/dev/null; then
    kill "${web_pid}" 2>/dev/null || true
    wait "${web_pid}" 2>/dev/null || true
  fi
  if [[ "${KEEP_WORKDIR:-false}" == "true" ]]; then
    echo "artifacts kept in ${workdir}"
  else
    rm -rf "${workdir}"
  fi
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  echo "--- web log ---" >&2
  cat "${workdir}/web.log" >&2 2>/dev/null || true
  exit 1
}

for bin in deno curl; do
  command -v "${bin}" >/dev/null || fail "${bin} is required"
done

echo "== building WASM modules =="
"${repo_root}/scripts/build-kcp-wasm.sh" "${workdir}/bin" >/dev/null
"${repo_root}/scripts/build-kubectl-wasm.sh" "${workdir}/bin" >/dev/null
"${repo_root}/scripts/build-goat-wasm.sh" "${workdir}/bin" >/dev/null
"${repo_root}/scripts/build-yq-wasm.sh" "${workdir}/bin" >/dev/null

echo "== starting static demo server on http://${host}:${port} =="
deno run --allow-read --allow-net \
  "${repo_root}/scripts/kubectl_wasm_web.ts" \
  --kcp-wasm="${workdir}/bin/kcp.wasm" \
  --kubectl-wasm="${workdir}/bin/kubectl.wasm" \
  --goat-wasm="${workdir}/bin/goat.wasm" \
  --yq-wasm="${workdir}/bin/yq.wasm" \
  --wasm-exec="${workdir}/bin/wasm_exec.js" \
  --wasm-exec-cli="${workdir}/bin/wasm_exec_cli.js" \
  --host="${host}" --port="${port}" \
  >>"${workdir}/web.log" 2>&1 &
web_pid=$!

for _ in $(seq 1 120); do
  if ! kill -0 "${web_pid}" 2>/dev/null; then
    fail "web server exited during startup"
  fi
  if curl -sf -o /dev/null "http://${host}:${port}/config" 2>/dev/null; then
    break
  fi
  sleep 0.25
done

echo "== checking page and assets =="
page="$(curl -sf "http://${host}:${port}/")"
[[ "${page}" == *"@xterm/xterm"* ]] || fail "page does not reference xterm.js"
[[ "${page}" == *"pele"* ]] || fail "page does not reference Pele"
[[ "${page}" == *"/app.js"* ]] || fail "page does not load app.js"
app_js="$(curl -sf "http://${host}:${port}/app.js")"
[[ "${app_js}" == *"just-bash"* ]] || fail "app.js does not load just-bash"
[[ "${app_js}" == *"cli_worker.js"* ]] || fail "app.js does not load cli_worker.js"

for asset in app.js diagram.js kcp_worker.js kubectl_worker.js cli_worker.js fs_shim.js webrtc_ingress.js wasm_exec.js wasm_exec_cli.js; do
  curl -sf -o /dev/null "http://${host}:${port}/${asset}" || fail "missing ${asset}"
done
for wasm in kcp.wasm kubectl.wasm goat.wasm yq.wasm; do
  size="$(curl -sf -o /dev/null -w '%{size_download}' "http://${host}:${port}/${wasm}")"
  [[ "${size}" -gt 1000000 ]] || fail "${wasm} looks empty (${size} bytes)"
done

echo "== checking ETag revalidation =="
etag="$(curl -sD - -o /dev/null "http://${host}:${port}/kcp.wasm" | tr -d '\r' | awk -F': ' 'tolower($1)=="etag"{print $2}')"
[[ -n "${etag}" ]] || fail "kcp.wasm has no ETag"
code="$(curl -s -o /dev/null -w '%{http_code}' -H "If-None-Match: ${etag}" "http://${host}:${port}/kcp.wasm")"
[[ "${code}" == "304" ]] || fail "conditional kcp.wasm request returned ${code}, want 304"

config="$(curl -sf "http://${host}:${port}/config")"
[[ "${config}" == *'"apiServer"'* ]] || fail "/config missing apiServer"

demo="$(curl -sf "http://${host}:${port}/demo.yaml")"
[[ "${demo}" == *"kind: Fetch"* ]] || fail "/demo.yaml missing Fetch"
[[ "${demo}" == *"url: http://${host}:${port}/echo"* ]] || fail "/demo.yaml points elsewhere"

echo "== running diagram tests =="
deno test "${repo_root}/scripts/browser/diagram_test.js" >/dev/null

echo "PASS: browser demo assets served; open http://${host}:${port} for the just-bash shell with kcp, kubectl, goat and yq"
