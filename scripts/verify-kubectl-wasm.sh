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

# Verifies upstream kubectl compiled to js/wasm against the kcp WASM shard:
#   * kubectl runs in a Deno Web Worker, separate from the kcp worker
#   * its Go net/http transport performs requests with the worker's fetch()
#   * discovery, CRD CRUD and Table output work end to end
#   * stdin (`kubectl create -f -`) goes through the Deno fs shim
#
# No native kubectl is involved.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
port="${PORT:-18082}"
host="${HOST:-127.0.0.1}"
workdir="$(mktemp -d "${TMPDIR:-/tmp}/kubectl-wasm-verify.XXXXXX")"
server_pid=""

cleanup() {
  if [[ -n "${server_pid}" ]] && kill -0 "${server_pid}" 2>/dev/null; then
    kill "${server_pid}" 2>/dev/null || true
    wait "${server_pid}" 2>/dev/null || true
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
  echo "--- server log ---" >&2
  cat "${workdir}/server.log" >&2 2>/dev/null || true
  exit 1
}

assert_contains() {
  local haystack="$1" needle="$2" what="$3"
  if [[ "${haystack}" != *"${needle}"* ]]; then
    fail "${what}: expected to find '${needle}' in:\n${haystack}"
  fi
}

for bin in deno curl; do
  command -v "${bin}" >/dev/null || fail "${bin} is required"
done

echo "== building kcp-wasm =="
"${repo_root}/scripts/build-kcp-wasm.sh" "${workdir}/bin" >/dev/null

echo "== building kubectl-wasm =="
"${repo_root}/scripts/build-kubectl-wasm.sh" "${workdir}/bin" >/dev/null

state_file="${workdir}/state.json"
server_log="${workdir}/server.log"

echo "== starting Deno kcp host on http://${host}:${port} =="
deno run --allow-read --allow-net --allow-env --allow-write \
  "${repo_root}/scripts/kcp_wasm.ts" \
  --wasm="${workdir}/bin/kcp.wasm" \
  --wasm-exec="${workdir}/bin/wasm_exec.js" \
  --state="${state_file}" \
  --host="${host}" --port="${port}" \
  >>"${server_log}" 2>&1 &
server_pid=$!

ready=false
for _ in $(seq 1 120); do
  if ! kill -0 "${server_pid}" 2>/dev/null; then
    fail "Deno kcp host exited during startup"
  fi
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://${host}:${port}/readyz" 2>/dev/null || true)"
  if [[ "${code}" == "200" ]]; then
    ready=true
    break
  fi
  sleep 0.25
done
[[ "${ready}" == "true" ]] || fail "kcp-wasm did not become ready"

kubectl_wasm() {
  deno run --allow-read --allow-net \
    "${repo_root}/scripts/kubectl_wasm.ts" \
    --wasm="${workdir}/bin/kubectl.wasm" \
    --wasm-exec="${workdir}/bin/wasm_exec.js" \
    "$@"
}

echo "== kubectl-wasm: discovery =="
out="$(kubectl_wasm -- get crds --server="http://${host}:${port}" 2>&1)"
assert_contains "${out}" "fetches.fetch.kcp.io" "CRD discovery"

echo "== kubectl-wasm: create from stdin =="
cat >"${workdir}/fetch.yaml" <<EOF
apiVersion: fetch.kcp.io/v1alpha1
kind: Fetch
metadata:
  name: demo
spec:
  url: http://${host}:${port}/echo?msg=wasm-kubectl
EOF
out="$(kubectl_wasm --stdin-file="${workdir}/fetch.yaml" \
  -- create -f - --server="http://${host}:${port}" 2>&1)"
assert_contains "${out}" "fetch.fetch.kcp.io/demo created" "create"

echo "== kubectl-wasm: read back =="
resolved=false
for _ in $(seq 1 120); do
  out="$(kubectl_wasm -- get fetch demo -o yaml --server="http://${host}:${port}" 2>&1)"
  if [[ "${out}" == *"phase: Resolved"* ]]; then
    resolved=true
    break
  fi
  sleep 0.25
done
assert_contains "${out}" "fetched-by=deno" "controller body"
assert_contains "${out}" "wasm-kubectl" "controller body"
[[ "${resolved}" == "true" ]] || fail "Fetch did not become Resolved: ${out}"

echo "== kubectl-wasm: table output =="
out="$(kubectl_wasm -- get fetches --server="http://${host}:${port}" 2>&1)"
assert_contains "${out}" "demo" "table row"
assert_contains "${out}" "Resolved" "table phase"

echo "PASS: upstream kubectl (js/wasm, in a Deno worker) used the kcp WASM shard over HTTP"
