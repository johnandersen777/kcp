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
import json, os, sys
out, socialweb, overlay_dir = sys.argv[1:4]

# Go's js/wasm net/http only uses the browser's fetch when the Transport has no
# custom dialer; with one it resolves DNS over UDP, which wasm cannot. Generate
# copies of the files that build such transports without the dialer line, from
# the current source so the copies never go stale. Name/address host maps of a
# market network are not applied in the browser; fetch resolves the names.
gen_dir = os.path.join(os.path.dirname(out), ".overlay-gen")
os.makedirs(gen_dir, exist_ok=True)
no_dialer = {
    "internal/pds/remote/remote.go":
        "\t\t\tDialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive}).DialContext,\n",
    "internal/atproto/pdsclient/remote.go":
        "\t\tDialContext:           (&net.Dialer{Timeout: remoteDialTimeout, KeepAlive: remoteKeepAlive}).DialContext,\n",
    "internal/kit/networkdial/dialer.go":
        "\t\tDialContext:           d.DialContext,\n",
}
generated = {}
for rel, line in no_dialer.items():
    src = f"{socialweb}/{rel}"
    text = open(src).read()
    if line not in text:
        sys.exit(f"prepare-socialweb-wasm-overlay: the dialer line is gone from {src}; update this script")
    dst = os.path.join(gen_dir, rel.replace("/", "_"))
    # Keep the dialer's imports and constants used.
    open(dst, "w").write(text.replace(line, "") + "\nvar _ = net.Dialer{}\n")
    generated[src] = dst

print(json.dumps({"Replace": {
    **generated,
    f"{socialweb}/internal/hostos/lock_js.go": f"{overlay_dir}/hostos/lock_js.go",
    f"{socialweb}/internal/hostos/proc_js.go": f"{overlay_dir}/hostos/proc_js.go",
    f"{socialweb}/internal/portscan/scan_js.go": f"{overlay_dir}/portscan/scan_js.go",
    f"{socialweb}/internal/atproto/repo/sqlite.go": f"{overlay_dir}/atproto/repo_sqlite.go",
    f"{socialweb}/internal/atproto/pds/store.go": f"{overlay_dir}/atproto/pds_store.go",
    f"{socialweb}/internal/app/compose/compose.go": f"{overlay_dir}/compose/compose.go",
}}, indent=2), file=open(out, "w"))
PY

echo "wrote ${out}"
