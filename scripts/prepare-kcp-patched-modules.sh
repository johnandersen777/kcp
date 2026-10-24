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

# Prepares a -modfile for building kcp with the memory storage backend.
#
# Mode "native" only replaces the apiserver storage factory. Mode "js" also
# replaces unix-only dependencies (systemd journal, mount-utils, probe,
# moby/term, kube securitycontext) with js/wasm implementations.
#
# Usage: KCP_PATCH_MODE=native|js scripts/prepare-kcp-patched-modules.sh [workdir] [modfile]

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mode="${KCP_PATCH_MODE:-js}"
case "${mode}" in
  native|js) ;;
  *) echo "KCP_PATCH_MODE must be 'native' or 'js', got '${mode}'" >&2; exit 1 ;;
esac
tmp_root="${TMPDIR:-/tmp}"
tmp_root="${tmp_root%/}"
default_work="${tmp_root}/kcp-patched-modules-${mode}"
if [[ "${mode}" == "native" ]]; then
  # Shared with earlier manual builds so the Go build cache stays warm.
  default_work="${tmp_root}/kcp-mem-build"
fi
work="${1:-${default_work}}"
work="${work%/}"
modfile="${2:-${repo_root}/.go.memory.mod}"
sumfile="${modfile%.mod}.sum"

mkdir -p "${work}"
# KCP_SOURCE_MODFILE lets a nested module (the combined socialweb wasm main)
# seed its own go.mod while reusing the patched-module replaces.
source_modfile="${KCP_SOURCE_MODFILE:-${repo_root}/go.mod}"
source_sumfile="${source_modfile%.mod}.sum"
cp "${source_modfile}" "${modfile}"
if [[ -f "${source_sumfile}" ]]; then
  cp "${source_sumfile}" "${sumfile}"
else
  : > "${sumfile}"
fi

# A nested module's modfile may use paths relative to its own directory. The
# kcp staging modules always live under the kcp repo root; normalize both
# spellings to an absolute path so the modfile works from any directory.
python3 - "${modfile}" "${repo_root}" <<'PY'
import re, sys
path, root = sys.argv[1:3]
src = open(path).read()
src = re.sub(r'=>\s+(?:\.\./|\./)staging/', f'=> {root}/staging/', src)
open(path, 'w').write(src)
PY

set_replace() {
  local mod="$1" target="$2"
  python3 - "${modfile}" "${mod}" "${target}" <<'PY'
import re, sys

path, mod, target = sys.argv[1:4]
src = open(path).read()
pattern = re.compile(r"^(\s*)" + re.escape(mod) + r"\s+=>\s+.*$", re.MULTILINE)
if pattern.search(src):
    src = pattern.sub(lambda m: f"{m.group(1)}{mod} => {target}", src, count=1)
else:
    if not src.endswith("\n"):
        src += "\n"
    src += f"\nreplace {mod} => {target}\n"
open(path, "w").write(src)
PY
}

copy_module() {
  local module="$1" dest="$2"
  if [[ ! -d "${dest}" ]]; then
    local dir
    dir="$(cd "${repo_root}" && go list -m -f '{{.Dir}}' "${module}")"
    cp -a "${dir}" "${dest}"
    chmod -R u+w "${dest}"
  fi
  set_replace "${module}" "${dest}"
}

# replace_in <file> <old> <new> applies a literal source patch (backslash
# escapes such as \n and \t are expanded). It fails when <old> is missing so a
# dependency bump cannot silently drop a patch, and is a no-op on files that
# are already patched (copy_module keeps existing copies).
replace_in() {
  python3 - "$@" <<'PY'
import sys
path, old, new = (sys.argv[1],) + tuple(a.encode().decode("unicode_escape") for a in sys.argv[2:4])
src = open(path).read()
if old in src:
    open(path, "w").write(src.replace(old, new))
elif new not in src:
    sys.exit(f"{path}: patch target {old!r} not found")
PY
}

overlay="${repo_root}/contrib/wasm/_overlay"

# The storagebackend factory gains a memory:// backend.
copy_module k8s.io/apiserver "${work}/apiserver"
cp "${overlay}/apiserver/factory.go" \
  "${work}/apiserver/pkg/storage/storagebackend/factory/factory.go"
if [[ "${mode}" == "js" ]]; then
  cp "${overlay}/apiserver/serving_unix.go" \
    "${work}/apiserver/pkg/server/options/serving_unix.go"
  cp "${overlay}/apiserver/serving_with_loopback.go" \
    "${work}/apiserver/pkg/server/options/serving_with_loopback.go"
  # Admission plugins hold each write up to 10s while their informers sync,
  # then reject it as not ready. During browser bootstrap that stalls RBAC
  # and kube-system creation for the full timeout per request; fail fast and
  # let the bootstrap retry loops come back once the caches are warm.
  replace_in "${work}/apiserver/pkg/admission/handler.go" \
    'timeToWaitForReady = 10 * time.Second' 'timeToWaitForReady = 1 * time.Second'
fi

if [[ "${mode}" == "js" ]]; then
  # Systemd journal logging is replaced by stderr logging.
  copy_module github.com/coreos/go-systemd/v22 "${work}/go-systemd"
  cp "${overlay}/go-systemd/journal_unix.go" \
    "${work}/go-systemd/journal/journal_unix.go"

  # mount-utils' /proc and uname usage is replaced with stubs.
  copy_module k8s.io/mount-utils "${work}/mount-utils"
  cp "${overlay}/mount-utils/mount_helper_unix.go" \
    "${work}/mount-utils/mount_helper_unix.go"

  # kubelet-side probe dialer and cgroup CPU discovery.
  copy_module k8s.io/kubernetes "${work}/kubernetes"
  cp "${overlay}/probe/dialer_others.go" \
    "${work}/kubernetes/pkg/probe/dialer_others.go"
  cp "${overlay}/securitycontext/util_js.go" \
    "${work}/kubernetes/pkg/securitycontext/util_js.go"

  # etcd's flock-based file lock is a no-op in a single-instance wasm runtime.
  copy_module go.etcd.io/etcd/client/pkg/v3 "${work}/etcd-client-pkg"
  cp "${overlay}/etcd-fileutil/lock_flock.go" \
    "${work}/etcd-client-pkg/fileutil/lock_flock.go"

  # TTY ioctl usage in moby/term is replaced with no-ops.
  copy_module github.com/moby/term "${work}/moby-term"
  cp "${overlay}/moby-term/term_unix.go" \
    "${work}/moby-term/term_unix.go"
  cp "${overlay}/moby-term/termios_unix.go" \
    "${work}/moby-term/termios_unix.go"
  cp "${overlay}/moby-term/termios_nonbsd.go" \
    "${work}/moby-term/termios_nonbsd.go"

  # The loopback and serving certificates are self-signed on every boot, each
  # with an RSA-2048 CA and leaf key. RSA prime search is slow under wasm;
  # P-256 keys are generated in about a millisecond.
  copy_module k8s.io/client-go "${work}/client-go"
  cert_go="${work}/client-go/util/cert/cert.go"
  replace_in "${cert_go}" '"crypto/rsa"\n' '"crypto/ecdsa"\n\t"crypto/elliptic"\n'
  replace_in "${cert_go}" 'rsa.GenerateKey(cryptorand.Reader, 2048)' 'ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)'
  replace_in "${cert_go}" \
    '\tif err := pem.Encode(&keyBuffer, &pem.Block{Type: keyutil.RSAPrivateKeyBlockType, Bytes: x509.MarshalPKCS1PrivateKey(priv)}); err != nil {' \
    '\tkeyDER, err := x509.MarshalECPrivateKey(priv)\n\tif err != nil {\n\t\treturn nil, nil, err\n\t}\n\tif err := pem.Encode(&keyBuffer, &pem.Block{Type: keyutil.ECPrivateKeyBlockType, Bytes: keyDER}); err != nil {'

  # Informers start before their CRDs exist and their reflectors back off
  # 800ms doubling to 30s, so a resource that appears a moment later is only
  # listed tens of seconds after. A single in-process apiserver has no
  # thundering herd to protect against: retry from 100ms, capped at 1s. Both
  # client-go's reflector and kcp's cluster-aware fork carry the constants;
  # the fork lives in-tree, so refresh its copy on every run.
  rm -rf "${work}/kcp-apimachinery"
  copy_module github.com/kcp-dev/apimachinery/v2 "${work}/kcp-apimachinery"
  for reflector in "${work}/client-go/tools/cache/reflector.go" \
    "${work}/kcp-apimachinery/third_party/reflector/reflector.go"; do
    replace_in "${reflector}" 'defaultBackoffInit = 800 * time.Millisecond' 'defaultBackoffInit = 100 * time.Millisecond'
    replace_in "${reflector}" 'defaultBackoffMax  = 30 * time.Second' 'defaultBackoffMax  = 1 * time.Second'
  done

  # The CRD handler builds a fresh scale scheme (autoscaling, extensions and
  # apps conversions) for every served CRD version: ~10 MiB of heap in the
  # browser shard. The scheme is read-only once built; share one.
  rm -rf "${work}/apiextensions-apiserver"
  copy_module k8s.io/apiextensions-apiserver "${work}/apiextensions-apiserver"
  replace_in "${work}/apiextensions-apiserver/pkg/apiserver/customresource_handler.go" \
    'scaleConverter := scale.NewScaleConverter()' 'scaleConverter := sharedScaleConverter(scale.NewScaleConverter)'
  # The proto models (ToProtoModelsV3, MiBs per CRD) only serve strategic
  # merge patch, which CRDs accept only for client-go scheme groups. Skip
  # them for every other CRD; the SSA type converter does not use them.
  replace_in "${work}/apiextensions-apiserver/pkg/apiserver/customresource_handler.go" \
    '} else if openAPIModels != nil {' \
    '} else if openAPIModels != nil && clientgoscheme.Scheme.IsGroupRegistered(crd.Spec.Group) {'
  cat > "${work}/apiextensions-apiserver/pkg/apiserver/scale_shared.go" <<'GO'
package apiserver

import (
	"sync"

	"k8s.io/client-go/scale"
)

var (
	scaleConverterOnce sync.Once
	scaleConverter     *scale.ScaleConverter
)

func sharedScaleConverter(newConverter func() *scale.ScaleConverter) *scale.ScaleConverter {
	scaleConverterOnce.Do(func() { scaleConverter = newConverter() })
	return scaleConverter
}
GO
fi

echo "prepared ${modfile} (mode=${mode})"
