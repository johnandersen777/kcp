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

# Builds upstream kubectl (k8s.io/kubectl/pkg/cmd) for js/wasm, plus
# kubectl-slim.wasm (-tags kubectl_slim: get/apply/create/patch/delete/wait/
# describe/version/api-resources/api-versions only) for the page's low-memory mode,
# and stages the matching wasm_exec.js.
#
# Usage: scripts/build-kubectl-wasm.sh [outdir]   (default: bin/wasm)

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
outdir="${1:-${repo_root}/bin/wasm}"
module_dir="${repo_root}/contrib/wasm/kubectl"
work="${KUBECTL_WASM_WORK:-${TMPDIR:-/tmp}/kubectl-wasm-modules}"

mkdir -p "${outdir}"

echo "Preparing js/wasm replacements for unix-only modules..."
"${repo_root}/scripts/prepare-kubectl-wasm-modules.sh" "${work}" >/dev/null
work="$(cd "${work}" && pwd -P)"

# The slim build stubs two cli-runtime files. The go-template printer runs
# text/template, whose reflect.Value.MethodByName makes the linker keep every
# exported method of every reachable type (~40 MiB here); -k pulls kustomize.
slim_overlay="${module_dir}/_overlay/slim"
cat > "${work}/slim-overlay.json" <<EOF
{"Replace": {
  "${work}/cli-runtime/pkg/printers/template.go": "${slim_overlay}/cli-runtime-printers-template.go",
  "${work}/cli-runtime/pkg/resource/kustomizevisitor.go": "${slim_overlay}/cli-runtime-resource-kustomizevisitor.go"
}}
EOF

echo "Building upstream kubectl for js/wasm..."
(
  cd "${module_dir}"
  GOOS=js GOARCH=wasm go build -modfile="${work}/go.mod" -o "${outdir}/kubectl.wasm" .
  echo "Building slim kubectl for js/wasm..."
  GOOS=js GOARCH=wasm go build -modfile="${work}/go.mod" -tags kubectl_slim \
    -overlay="${work}/slim-overlay.json" -o "${outdir}/kubectl-slim.wasm" .
)
for file in kubectl.wasm kubectl-slim.wasm; do
  gzip -9 -n -c "${outdir}/${file}" > "${outdir}/${file}.gz"
done

install -m 0644 "$(go env GOROOT)/lib/wasm/wasm_exec.js" "${outdir}/wasm_exec.js"
echo "Built ${outdir}/kubectl.wasm ${outdir}/kubectl-slim.wasm"
