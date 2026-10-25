#!/bin/bash
# Build the browser page's kcp.wasm (kcp + socialweb) against a socialweb
# commit and, with DEPLOY=1, copy it into the main checkout's bin/wasm (which
# the dev server serves). See AGENTS.md "Build and deploy".
#   REV=<socialweb commit> [DEPLOY=1] scripts/browser-dev/build-page.sh
set -euo pipefail
W=$(cd "$(dirname "$0")/../.." && pwd)
SRC=${SOCIALWEB_SRC:-$HOME/src/kcp/socialweb-computer-kcp}
MAIN=${MAIN_CHECKOUT:-$HOME/src/kcp/kcp-wasm}
export TMPDIR=${TMPDIR:-$HOME/.tmp}
mkdir -p "$TMPDIR"
REV=$(git -c safe.directory='*' -C "$SRC" rev-parse "${REV:?set REV to a socialweb commit}")
cd "$W"
if [[ "$(cat .work/socialweb-mirror/.mirror-commit 2>/dev/null)" != "$REV" ]]; then
  old=$(cat .work/socialweb-mirror/.mirror-commit 2>/dev/null || true)
  # The compose.go overlay (Config.Rest) is 3-way merged onto the new upstream.
  if [[ -n "$old" ]]; then
    t=$(mktemp -d)
    git -c safe.directory='*' -C "$SRC" show "$old:internal/app/compose/compose.go" > "$t/base.go"
    git -c safe.directory='*' -C "$SRC" show "$REV:internal/app/compose/compose.go" > "$t/theirs.go"
    git merge-file -p socialweb-wasm/overlays/compose/compose.go "$t/base.go" "$t/theirs.go" > "$t/merged.go" \
      || { echo "compose.go overlay conflicts: resolve $t/merged.go" >&2; exit 1; }
    cp "$t/merged.go" socialweb-wasm/overlays/compose/compose.go
  fi
  rm -rf .work/socialweb-mirror && mkdir -p .work/socialweb-mirror
  git -c safe.directory='*' -C "$SRC" archive "$REV" | tar -x -C .work/socialweb-mirror
  echo "$REV" > .work/socialweb-mirror/.mirror-commit
  ./scripts/sync-socialweb-manifests.sh .work/socialweb-mirror
fi
export GOTOOLCHAIN=go1.26.9
export KCP_WASM_WORK=$W/.work/kcp-patched-modules-js
export SOCIALWEB_KCP=$SRC SOCIALWEB_KCP_MIRROR=$W/.work/socialweb-mirror SOCIALWEB_KCP_REFRESH=0
mkdir -p .work/wasm
rm -f .work/wasm/wasm_exec.js .work/wasm/kcp.wasm.gz
./scripts/build-socialweb-kcp-wasm.sh "$W/.work/wasm/kcp.wasm"
git checkout -- socialweb-wasm/.go.wasm.mod socialweb-wasm/.go.wasm.overlay.json 2>/dev/null || true
if [[ "${DEPLOY:-0}" == "1" ]]; then
  cp .work/wasm/{kcp.wasm,kcp.wasm.gz,wasm_exec.js,requester-scope.txt} "$MAIN/bin/wasm/"
  echo "deployed to $MAIN/bin/wasm"
fi
