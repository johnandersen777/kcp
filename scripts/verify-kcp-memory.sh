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

# Verifies that kcp's real generic apiserver runs on the in-memory
# storage.Interface backend (memory://main) instead of etcd:
#   * kcp starts and bootstraps without an etcd server
#   * CRDs and custom resources are served through the memory backend
#
# The js/wasm build uses the same factory and backend; this native run
# isolates the storage integration from the wasm runtime.
#
# Usage: scripts/verify-kcp-memory.sh

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
port="${PORT:-6445}"
workdir="$(mktemp -d "${TMPDIR:-/tmp}/kcp-memory-verify.XXXXXX")"
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
  echo "--- kcp log (tail) ---" >&2
  tail -100 "${workdir}/kcp.log" >&2 2>/dev/null || true
  exit 1
}

assert_contains() {
  local haystack="$1" needle="$2" what="$3"
  if [[ "${haystack}" != *"${needle}"* ]]; then
    fail "${what}: expected '${needle}' in:\n${haystack}"
  fi
}

command -v kubectl >/dev/null || fail "kubectl is required"
command -v go >/dev/null || fail "go is required"

echo "== preparing patched modules and building kcp =="
KCP_PATCH_MODE=native "${repo_root}/scripts/prepare-kcp-patched-modules.sh" >/dev/null
(
  cd "${repo_root}"
  go build -modfile="${repo_root}/.go.memory.mod" -o "${workdir}/kcp" ./cmd/kcp
)

echo "== starting kcp with memory://main storage on port ${port} =="
KCP_MEMORY_STORAGE=main "${workdir}/kcp" start \
  --root-directory="${workdir}/root" \
  --secure-port="${port}" \
  --bind-address=127.0.0.1 \
  >"${workdir}/kcp.log" 2>&1 &
server_pid=$!

for _ in $(seq 1 480); do
  kill -0 "${server_pid}" 2>/dev/null || fail "kcp exited during startup"
  [[ -f "${workdir}/root/admin.kubeconfig" ]] && break
  sleep 0.5
done
[[ -f "${workdir}/root/admin.kubeconfig" ]] || fail "admin.kubeconfig was not written"
export KUBECONFIG="${workdir}/root/admin.kubeconfig"

ready=false
for _ in $(seq 1 480); do
  if kubectl get --raw /readyz >/dev/null 2>&1; then
    ready=true
    break
  fi
  kill -0 "${server_pid}" 2>/dev/null || fail "kcp exited while waiting for readiness"
  sleep 0.5
done
[[ "${ready}" == "true" ]] || fail "kcp did not become ready"

echo "== apiserver is serving from memory =="
kubectl get --raw /version
kubectl get workspaces.kcp.io 2>/dev/null || true

echo "== applying the Go-defined Fetch CRD =="
(
  cd "${repo_root}"
  go run ./contrib/wasm/crdgen
) | kubectl apply -f -

crd="$(kubectl get crds fetches.fetch.kcp.io -o jsonpath='{.metadata.name}')"
[[ "${crd}" == "fetches.fetch.kcp.io" ]] || fail "CRD not found (got '${crd}')"

echo "== creating and reading Fetch custom resources =="
kubectl apply -f - <<'EOF'
apiVersion: fetch.kcp.io/v1alpha1
kind: Fetch
metadata:
  name: demo
spec:
  url: https://example.com/
EOF

url="$(kubectl get fetch demo -o jsonpath='{.spec.url}')"
[[ "${url}" == "https://example.com/" ]] || fail "Fetch spec.url = '${url}'"
list="$(kubectl get fetches)"
assert_contains "${list}" "demo" "Fetch list"
kubectl get fetch demo -o yaml

echo "== deleting through the memory backend =="
kubectl delete fetch demo
if kubectl get fetch demo >/dev/null 2>&1; then
  fail "Fetch still exists after delete"
fi

echo
echo "PASS: kcp's generic apiserver bootstrapped and served CRDs/CRs from memory://main (no etcd)."
