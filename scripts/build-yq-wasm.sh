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

# Builds mikefarah/yq (https://github.com/mikefarah/yq) for js/wasm and stages
# wasm_exec.js. Upstream sources are unmodified; the browser worker runs a
# fresh Go runtime per invocation.
#
# Usage: scripts/build-yq-wasm.sh [outdir]   (default: bin/wasm)
# Env: YQ_REPO, YQ_REF (default master), YQ_WASM_WORK

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
outdir="${1:-${repo_root}/bin/wasm}"
yq_repo="${YQ_REPO:-https://github.com/mikefarah/yq}"
yq_ref="${YQ_REF:-master}"
cli_toolchain="${GO_CLI_TOOLCHAIN:-go1.27.3}"
work="${YQ_WASM_WORK:-${TMPDIR:-/tmp}/yq-wasm-src}"

mkdir -p "${work}" "${outdir}"
work="$(cd "${work}" && pwd -P)"

if [[ ! -d "${work}/.git" ]]; then
  echo "Cloning ${yq_repo} (${yq_ref})..."
  git clone --depth 1 --branch "${yq_ref}" "${yq_repo}" "${work}"
else
  echo "Updating ${yq_repo} (${yq_ref})..."
  git -C "${work}" fetch --depth 1 origin "${yq_ref}"
  git -C "${work}" reset --hard FETCH_HEAD
fi
git -C "${work}" clean -fdx >/dev/null

isatty_dir="$("${repo_root}/scripts/prepare-cli-isatty.sh" "${work}/go-isatty" \
  "$(cd "${work}" && GOTOOLCHAIN="${cli_toolchain}" go list -m -f '{{.Dir}}' github.com/mattn/go-isatty)")"
cp "${work}/go.mod" "${work}/build.mod"
cp "${work}/go.sum" "${work}/build.sum"
echo "replace github.com/mattn/go-isatty => ${isatty_dir}" >> "${work}/build.mod"

commit="$(git -C "${work}" rev-parse --short HEAD)"
echo "Building yq ${commit} for js/wasm..."
(
  cd "${work}"
  GOTOOLCHAIN="${cli_toolchain}" GOOS=js GOARCH=wasm go build \
    -modfile="${work}/build.mod" \
    -ldflags="-X main.version=yq-${commit}" \
    -o "${outdir}/yq.wasm" .
)

cli_goroot="$(cd "${work}" && GOTOOLCHAIN="${cli_toolchain}" go env GOROOT)"
rm -f "${outdir}/wasm_exec_cli.js"
cp "${cli_goroot}/lib/wasm/wasm_exec.js" "${outdir}/wasm_exec_cli.js"
chmod u+w "${outdir}/wasm_exec_cli.js"
echo "Built ${outdir}/yq.wasm (yq ${commit})"
