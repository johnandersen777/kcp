#!/usr/bin/env bash
set -euo pipefail

# Builds the combined real-kcp + socialweb js/wasm module. The real kcp server
# uses memory:// storage and an in-process net.Pipe listener; the browser
# worker supplies the HTTP RPC surface.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
module_dir="${repo_root}/socialweb-wasm"
work="${KCP_WASM_WORK:-/tmp/kcp-patched-modules-js}"
modfile="${module_dir}/.go.wasm.mod"
out="${1:-${repo_root}/bin/wasm/kcp-real-socialweb.wasm}"
socialweb_src="${SOCIALWEB_KCP:-${repo_root}/../../linux-kcp/socialweb-computer-kcp}"
mirror="${SOCIALWEB_KCP_MIRROR:-/tmp/linux-kcp-mirror}"

mkdir -p "$(dirname "${out}")"

# The read-only linux-kcp mount intermittently returns stale NFS handles.
# Build against a stable mirror produced from git objects instead.
if [[ "${SOCIALWEB_KCP_REFRESH:-0}" == "1" || ! -f "${mirror}/go.mod" ]]; then
  echo "mirroring socialweb source to ${mirror}..."
  rm -rf "${mirror}"
  mkdir -p "${mirror}"
  git -C "${socialweb_src}" -c safe.directory="${socialweb_src}" archive HEAD | tar -x -C "${mirror}"
fi

KCP_PATCH_MODE=js \
KCP_SOURCE_MODFILE="${module_dir}/go.mod" \
  "${repo_root}/scripts/prepare-kcp-patched-modules.sh" "${work}" "${modfile}"

(
  cd "${module_dir}"
  go mod edit -modfile="${modfile}" -replace="github.com/johnandersen777/socialweb-computer-kcp=${mirror}"
)

SOCIALWEB_KCP="${mirror}" "${repo_root}/scripts/prepare-socialweb-wasm-overlay.sh"

echo "building combined kcp + socialweb wasm..."
(
  cd "${module_dir}"
  GOOS=js GOARCH=wasm go build \
    -modfile="${modfile}" \
    -overlay="${module_dir}/.go.wasm.overlay.json" \
    -tags kcprealserver \
    -o "${out}" \
    ./cmd/kcp-wasm
)

cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$(dirname "${out}")/wasm_exec.js"
# The page server and the browser workers prefer this sibling; gzip -9 takes
# ~269 MB to ~41 MB (browsers can only inflate gzip, not brotli/zstd).
gzip -9 -n -c "${out}" > "${out}.gz"
ls -lh "${out}" "${out}.gz" "$(dirname "${out}")/wasm_exec.js"
