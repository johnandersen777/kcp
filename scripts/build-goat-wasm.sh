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

# Builds the bluesky-social/goat CLI (https://github.com/bluesky-social/goat)
# for js/wasm and stages wasm_exec.js.
#
# Upstream sources are not modified. The stock goat main() is used; the browser
# worker starts a fresh Go runtime per invocation with argv/env/cwd from the
# shell and captures stdout/stderr/exit code. indigo (goat's SDK) needs two
# js/wasm adaptations (no DNS; net/http only uses fetch without DialContext),
# so the script copies indigo to a build directory, patches the copy, and
# builds with a generated -modfile. The goat checkout and module cache stay
# untouched. See contrib/wasm/goat/indigo/.
#
# Usage: scripts/build-goat-wasm.sh [outdir]   (default: bin/wasm)
# Env: GOAT_REPO, GOAT_REF (default main), GOAT_WASM_WORK

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
outdir="${1:-${repo_root}/bin/wasm}"
goat_repo="${GOAT_REPO:-https://github.com/bluesky-social/goat}"
goat_ref="${GOAT_REF:-main}"
cli_toolchain="${GO_CLI_TOOLCHAIN:-go1.27.3}"
work="${GOAT_WASM_WORK:-${TMPDIR:-/tmp}/goat-wasm-src}"

mkdir -p "${work}" "${outdir}"
work="$(cd "${work}" && pwd -P)"

if [[ ! -d "${work}/.git" ]]; then
  echo "Cloning ${goat_repo} (${goat_ref})..."
  git clone --depth 1 --branch "${goat_ref}" "${goat_repo}" "${work}"
else
  echo "Updating ${goat_repo} (${goat_ref})..."
  git -C "${work}" fetch --depth 1 origin "${goat_ref}"
  git -C "${work}" reset --hard FETCH_HEAD
fi
git -C "${work}" clean -fdx >/dev/null

echo "Preparing adapted indigo copy for js/wasm..."
indigo_src="$(cd "${work}" && GOTOOLCHAIN="${cli_toolchain}" go list -m -f '{{.Dir}}' github.com/bluesky-social/indigo)"
indigo_dir="${work}/indigo"
rm -rf "${indigo_dir}"
cp -a "${indigo_src}" "${indigo_dir}"
chmod -R u+w "${indigo_dir}"

cp "${repo_root}/contrib/wasm/goat/indigo/ssrf/ssrf_js.go" \
  "${indigo_dir}/util/ssrf/ssrf_js.go"
cp "${repo_root}/contrib/wasm/goat/indigo/identity/handle_fallback_js.go" \
  "${indigo_dir}/atproto/identity/handle_fallback_js.go"
cp "${repo_root}/contrib/wasm/goat/indigo/identity/handle_fallback_other.go" \
  "${indigo_dir}/atproto/identity/handle_fallback_other.go"

python3 - "${indigo_dir}" <<'GOATPY'
import re
import sys

root = sys.argv[1]

ssrf_path = f"{root}/util/ssrf/ssrf.go"
src = open(ssrf_path).read()
if "//go:build !js" not in src:
    src = src.replace("package ssrf", "//go:build !js\n\npackage ssrf", 1)
    open(ssrf_path, "w").write(src)

directory_path = f"{root}/atproto/identity/directory.go"
src = open(directory_path).read()
src = src.replace("\tssrfHTTPDialer := ssrf.PublicOnlyDialer()\n", "", 1)
src, count = re.subn(
    r"(\t+)Transport: &http\.Transport\{.*?\n\t+\},\n",
    r"\1Transport: ssrf.PublicOnlyTransport(),\n",
    src,
    count=1,
    flags=re.DOTALL,
)
if count != 1:
    raise SystemExit("could not replace indigo identity transport")
open(directory_path, "w").write(src)

handle_path = f"{root}/atproto/identity/handle.go"
src = open(handle_path).read()
needle = "\tif nil == httpErr { // if *not* an error\n\t\treturn did, nil\n\t}\n"
addition = needle + (
    "\n\tif fallbackDID, fallbackErr, ok := resolveHandleHTTPFallback(ctx, handle); ok {\n"
    "\t\tif fallbackErr == nil {\n"
    "\t\t\treturn fallbackDID, nil\n"
    "\t\t}\n"
    "\t\thttpErr = fallbackErr\n"
    "\t}\n"
)
if needle not in src:
    raise SystemExit("could not patch indigo ResolveHandle")
src = src.replace(needle, addition, 1)
open(handle_path, "w").write(src)
GOATPY

cp "${work}/go.mod" "${work}/build.mod"
cp "${work}/go.sum" "${work}/build.sum"
echo "replace github.com/bluesky-social/indigo => ${indigo_dir}" >> "${work}/build.mod"

commit="$(git -C "${work}" rev-parse --short HEAD)"
echo "Building goat ${commit} for js/wasm..."
(
  cd "${work}"
  GOTOOLCHAIN="${cli_toolchain}" GOOS=js GOARCH=wasm go build -modfile="${work}/build.mod" \
    -ldflags="-X main.version=goat-${commit}" \
    -o "${outdir}/goat.wasm" .
)

cli_goroot="$(cd "${work}" && GOTOOLCHAIN="${cli_toolchain}" go env GOROOT)"
rm -f "${outdir}/wasm_exec_cli.js"
cp "${cli_goroot}/lib/wasm/wasm_exec.js" "${outdir}/wasm_exec_cli.js"
chmod u+w "${outdir}/wasm_exec_cli.js"
echo "Built ${outdir}/goat.wasm (goat ${commit}, stock main + adapted indigo copy)"
