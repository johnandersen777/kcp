#!/usr/bin/env bash
# The policy suite, page half (socialweb's half: linux-kcp-policytests). Scenario
# names match socialweb's (matrix/<policy>/<mode>/<side>/<case>, always-run/...,
# *-deny fail-closed cases). Needs .work/socialweb-mirror and
# .work/kcp-patched-modules-js from scripts/browser-dev/build-page.sh.
#   scripts/browser-dev/policy-suite.sh [chrome path]
set -euo pipefail
cd "$(dirname "$0")/../.."
W=$PWD
export TMPDIR=$HOME/.tmp GOTOOLCHAIN=${GOTOOLCHAIN:-go1.26.9}
(cd scripts/browser && deno test --allow-read --allow-net policy_suite_test.js policy_sandbox_test.js vmshell_test.js)
deno run -A scripts/browser-dev/policy-sandbox-check.ts "$@"
mod=$(mktemp -d "$TMPDIR/policy-suite.XXXXXX")/go.wasm.mod
cp socialweb-wasm/.go.wasm.mod "$mod"
cp socialweb-wasm/.go.wasm.sum "${mod%.mod}.sum"
sed -i.bak -e "s#=> /tmp/linux-kcp-mirror#=> $W/.work/socialweb-mirror#" \
  -e "s#/tmp/kcp-patched-modules-js#$W/.work/kcp-patched-modules-js#g" \
  -e "s#=> /[^ ]*/staging/src/#=> $W/staging/src/#" \
  -e "s#kcp-dev/kcp => \.\./#kcp-dev/kcp => $W/#" "$mod"
cd socialweb-wasm
PATH="$(go env GOROOT)/lib/wasm:$PATH" GOOS=js GOARCH=wasm go test -v -modfile="$mod" ./internal/browsersandbox
