#!/usr/bin/env bash
set -euo pipefail

# Dev loop for the combined kcp + socialweb wasm module.
#
#   ./scripts/dev-socialweb.sh            # build if needed, boot with saved state
#   ./scripts/dev-socialweb.sh --build    # rebuild first
#   FORCE_APPLY=1 ./scripts/dev-socialweb.sh
#
# The state file makes the provider manifests and the composed app survive a
# restart. Once every root APIBinding is Ready a boot skips the manifest apply
# pass; set FORCE_APPLY=1 (or delete the state) after changing manifests or
# schemas.
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
port="${PORT:-18080}"
state="${STATE:-/tmp/kcp-socialweb-dev.state.json}"
wasm="${WASM:-${repo_root}/bin/wasm/kcp-real-socialweb.wasm}"
build=0
for arg in "$@"; do
  case "${arg}" in
    --build) build=1 ;;
    *) echo "unknown argument: ${arg}" >&2; exit 2 ;;
  esac
done

if [[ "${build}" == 1 || ! -f "${wasm}" ]]; then
  "${repo_root}/scripts/build-socialweb-kcp-wasm.sh" "${wasm}"
fi

args=(
  --wasm="${wasm}"
  --wasm-exec="${repo_root}/bin/wasm/wasm_exec.js"
  --state="${state}"
  --port="${port}"
  --socialweb-origin="http://127.0.0.1:${port}"
)
if [[ "${FORCE_APPLY:-0}" == 1 || ! -f "${state}" ]]; then
  args+=(--force-apply)
fi

cd "${repo_root}"
exec deno run --allow-read --allow-net --allow-write --allow-env \
  scripts/kcp_wasm.ts "${args[@]}"
