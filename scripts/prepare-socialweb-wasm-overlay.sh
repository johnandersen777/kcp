#!/usr/bin/env bash
set -euo pipefail

# Writes the Go -overlay file for browser-only socialweb files. The read-only
# socialweb tree is never modified; the overlay adds js-tagged files where the
# original tree only has unix/windows implementations.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
socialweb="${SOCIALWEB_KCP:-${repo_root}/../../linux-kcp/socialweb-computer-kcp}"
overlay_dir="${repo_root}/socialweb-wasm/overlays"
out="${1:-${repo_root}/socialweb-wasm/.go.wasm.overlay.json}"

socialweb="$(cd "${socialweb}" && pwd)"

python3 - "${out}" "${socialweb}" "${overlay_dir}" <<'PY'
import json, sys
out, socialweb, overlay_dir = sys.argv[1:4]

print(json.dumps({"Replace": {
    f"{socialweb}/internal/hostos/lock_js.go": f"{overlay_dir}/hostos/lock_js.go",
    f"{socialweb}/internal/hostos/proc_js.go": f"{overlay_dir}/hostos/proc_js.go",
    f"{socialweb}/internal/portscan/scan_js.go": f"{overlay_dir}/portscan/scan_js.go",
    f"{socialweb}/internal/atproto/repo/sqlite.go": f"{overlay_dir}/atproto/repo_sqlite.go",
    f"{socialweb}/internal/atproto/pds/store.go": f"{overlay_dir}/atproto/pds_store.go",
    f"{socialweb}/internal/app/compose/compose.go": f"{overlay_dir}/compose/compose.go",
}}, indent=2), file=open(out, "w"))
PY

echo "wrote ${out}"
