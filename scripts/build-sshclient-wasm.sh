#!/usr/bin/env bash

# Copyright 2026 The KCP Authors.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

# Builds the browser SSH client (contrib/wasm/sshclient, x/crypto/ssh over a
# WebRTC datachannel) for js/wasm, plus ssh.wasm.gz for wasm_fetch.js. It uses
# the kcp build's toolchain so the page's /wasm_exec.js matches.
#
# Usage: scripts/build-sshclient-wasm.sh [outdir]   (default: bin/wasm)

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
outdir="${1:-${repo_root}/bin/wasm}"
mkdir -p "${outdir}"
cd "${repo_root}/contrib/wasm/sshclient"
GOTOOLCHAIN="${GO_SSH_TOOLCHAIN:-go1.26.9}" GOOS=js GOARCH=wasm \
  go build -trimpath -ldflags="-s -w" -o "${outdir}/ssh.wasm" .
gzip -9 -k -f "${outdir}/ssh.wasm"
ls -l "${outdir}/ssh.wasm" "${outdir}/ssh.wasm.gz"
