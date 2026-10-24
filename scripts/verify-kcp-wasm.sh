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

# Builds the kcp WASM shard, runs it through Deno, and verifies the Fetch CRD
# end to end with kubectl:
#   * the CRD is served by the WASM kcp
#   * a Fetch starts Unresolved
#   * adding a URL makes the controller fetch it via Deno's fetch()
#   * the response body shows up in `kubectl get fetches` / `-o yaml`

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
port="${PORT:-18080}"
host="${HOST:-127.0.0.1}"
workdir="$(mktemp -d "${TMPDIR:-/tmp}/kcp-wasm-verify.XXXXXX")"
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

for bin in deno kubectl curl; do
  command -v "${bin}" >/dev/null || fail "${bin} is required"
done

echo "== building kcp-wasm =="
"${repo_root}/scripts/build-kcp-wasm.sh" "${workdir}/bin" >/dev/null

state_file="${workdir}/state.json"

start_server() {
  echo "== starting Deno host on http://${host}:${port} =="
  deno run --allow-read --allow-net --allow-env --allow-write \
    "${repo_root}/scripts/kcp_wasm.ts" \
    --wasm="${workdir}/bin/kcp.wasm" \
    --wasm-exec="${workdir}/bin/wasm_exec.js" \
    --state="${state_file}" \
    --host="${host}" --port="${port}" --verbose \
    >>"${workdir}/server.log" 2>&1 &
  server_pid=$!

  local ready=false
  for _ in $(seq 1 120); do
    if ! kill -0 "${server_pid}" 2>/dev/null; then
      fail "Deno host exited during startup"
    fi
    code="$(curl -s -o /dev/null -w '%{http_code}' "http://${host}:${port}/readyz" 2>/dev/null || true)"
    if [[ "${code}" == "200" ]]; then
      ready=true
      break
    fi
    sleep 0.25
  done
  [[ "${ready}" == "true" ]] || fail "kcp-wasm did not become ready"
}

stop_server() {
  if [[ -n "${server_pid}" ]] && kill -0 "${server_pid}" 2>/dev/null; then
    kill "${server_pid}" 2>/dev/null || true
    wait "${server_pid}" 2>/dev/null || true
  fi
  server_pid=""
}

start_server

cat >"${workdir}/kubeconfig" <<EOF
apiVersion: v1
kind: Config
clusters:
- name: kcp-wasm
  cluster:
    server: http://${host}:${port}
contexts:
- name: kcp-wasm
  context:
    cluster: kcp-wasm
    user: kcp-wasm
current-context: kcp-wasm
users:
- name: kcp-wasm
  user: {}
EOF
export KUBECONFIG="${workdir}/kubeconfig"

echo "== server version =="
kubectl version 2>/dev/null | tee "${workdir}/version.txt"
assert_contains "$(cat "${workdir}/version.txt")" "Server Version" "server version"

echo "== CRD is served by the WASM kcp =="
crds="$(kubectl get crds)"
echo "${crds}"
assert_contains "${crds}" "fetches.fetch.kcp.io" "CRD discovery"

echo "== apply a Fetch with a URL; it should resolve and store the body =="
cat >"${workdir}/demo.yaml" <<EOF
apiVersion: fetch.kcp.io/v1alpha1
kind: Fetch
metadata:
  name: demo
spec:
  url: http://${host}:${port}/echo?msg=hello-from-kcp-wasm
EOF
kubectl apply -f "${workdir}/demo.yaml"

phase=""
for _ in $(seq 1 100); do
  phase="$(kubectl get fetch demo -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  [[ "${phase}" == "Resolved" ]] && break
  sleep 0.1
done
[[ "${phase}" == "Resolved" ]] || fail "demo did not resolve (phase=${phase})"

body="$(kubectl get fetch demo -o jsonpath='{.status.body}')"
code="$(kubectl get fetch demo -o jsonpath='{.status.statusCode}')"
[[ "${code}" == "200" ]] || fail "demo statusCode=${code}, want 200"
assert_contains "${body}" "fetched-by=deno" "body came from Deno fetch"
assert_contains "${body}" "msg=hello-from-kcp-wasm" "body query parameter"

echo "== kubectl get fetches (body column) =="
table="$(kubectl get fetches)"
echo "${table}"
assert_contains "${table}" "Resolved" "table phase"
assert_contains "${table}" "hello-from-kcp-wasm" "table body"

echo "== apply an updated URL; the controller refetches =="
sed -i "s/msg=hello-from-kcp-wasm/msg=hello-again/" "${workdir}/demo.yaml"
kubectl apply -f "${workdir}/demo.yaml"
updated_body=""
for _ in $(seq 1 100); do
  updated_body="$(kubectl get fetch demo -o jsonpath='{.status.body}' 2>/dev/null || true)"
  [[ "${updated_body}" == *"msg=hello-again"* ]] && break
  sleep 0.1
done
assert_contains "${updated_body}" "msg=hello-again" "body after apply update"

echo "== a Fetch without a URL stays Unresolved until it gets one =="
cat >"${workdir}/later.yaml" <<EOF
apiVersion: fetch.kcp.io/v1alpha1
kind: Fetch
metadata:
  name: later
spec: {}
EOF
kubectl create -f "${workdir}/later.yaml"

phase="$(kubectl get fetch later -o jsonpath='{.status.phase}')"
[[ "${phase}" == "Unresolved" ]] || fail "later initial phase=${phase}, want Unresolved"

kubectl patch fetch later --type=merge \
  -p "{\"spec\":{\"url\":\"http://${host}:${port}/echo?msg=patched-later\"}}"

phase=""
for _ in $(seq 1 100); do
  phase="$(kubectl get fetch later -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  [[ "${phase}" == "Resolved" ]] && break
  sleep 0.1
done
[[ "${phase}" == "Resolved" ]] || fail "later did not resolve after patch (phase=${phase})"

late_body="$(kubectl get fetch later -o jsonpath='{.status.body}')"
assert_contains "${late_body}" "msg=patched-later" "late body"

echo "== restart Deno; snapshots are persisted by the host and restored =="
persisted=false
for _ in $(seq 1 100); do
  if [[ -f "${state_file}" ]] && grep -q 'hello-again' "${state_file}"; then
    persisted=true
    break
  fi
  sleep 0.1
done
[[ "${persisted}" == "true" ]] || fail "state file ${state_file} was not written"

stop_server
start_server

restored_body="$(kubectl get fetch demo -o jsonpath='{.status.body}')"
assert_contains "${restored_body}" "msg=hello-again" "restored demo body"
restored_phase="$(kubectl get fetch demo -o jsonpath='{.status.phase}')"
[[ "${restored_phase}" == "Resolved" ]] || fail "restored demo phase=${restored_phase}, want Resolved"
count="$(kubectl get fetches -o jsonpath='{.items[*].metadata.name}')"
assert_contains "${count}" "later" "restored object list"

echo "== final state =="
kubectl get fetches
kubectl get fetch demo -o yaml

echo
echo "PASS: kcp-wasm serves the Fetch CRD; Go's http client fetched through Deno and stored the body; Deno persisted and restored the snapshot."
