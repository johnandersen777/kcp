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

# Prepares a -modfile for building upstream kubectl for js/wasm.
#
# k8s.io/kubectl and moby/term use unix-only APIs (syscall.SIGHUP, unix.Umask,
# TTY ioctls). This copies those modules to a writable directory, swaps in the
# js/wasm implementations from contrib/wasm/kubectl/_overlay (and the shared
# moby-term overlay), and writes a modfile with replace directives.
#
# Usage: scripts/prepare-kubectl-wasm-modules.sh [workdir] [modfile]

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
module_dir="${repo_root}/contrib/wasm/kubectl"
work="${1:-${TMPDIR:-/tmp}/kubectl-wasm-modules}"

mkdir -p "${work}"
# Normalize TMPDIR's trailing slash: "//" in a replace target is a go.mod
# comment and would truncate the path.
work="$(cd "${work}" && pwd -P)"
modfile="${2:-${work}/go.mod}"
sumfile="${modfile%.mod}.sum"

cp "${module_dir}/go.mod" "${modfile}"
cp "${module_dir}/go.sum" "${sumfile}"

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
    dir="$(cd "${module_dir}" && go mod download "${module}" && go list -m -f '{{.Dir}}' "${module}")"
    cp -a "${dir}" "${dest}"
    chmod -R u+w "${dest}"
  fi
  set_replace "${module}" "${dest}"
}

overlay="${module_dir}/_overlay"
term_overlay="${repo_root}/contrib/wasm/_overlay/moby-term"

copy_module k8s.io/kubectl "${work}/kubectl"
cp "${overlay}/kubectl/interrupt.go" \
  "${work}/kubectl/pkg/util/interrupt/interrupt.go"
cp "${overlay}/kubectl/umask.go" \
  "${work}/kubectl/pkg/util/umask.go"
cp "${overlay}/kubectl/resizeevents.go" \
  "${work}/kubectl/pkg/util/term/resizeevents.go"

# Plugin dispatch uses syscall.Exec, which js/wasm does not define. The wasm
# entrypoint passes no PluginHandler, so this path is never taken.
python3 - "${work}/kubectl/pkg/cmd/plugin.go" <<'PY'
import sys

path = sys.argv[1]
src = open(path).read()
if 'syscall.Exec' in src:
    src = src.replace('\t"syscall"\n', '')
    src = src.replace(
        'return syscall.Exec(executablePath, append([]string{executablePath}, cmdArgs...), environment)',
        'return fmt.Errorf("exec plugins are not supported on js/wasm")',
    )
    open(path, 'w').write(src)
PY

# Unpatched copy: build-kubectl-wasm.sh overlays files in it for the slim
# kubectl (go build -overlay cannot replace files in the module cache).
copy_module k8s.io/cli-runtime "${work}/cli-runtime"

copy_module github.com/moby/term "${work}/moby-term"
cp "${term_overlay}/term_unix.go" "${work}/moby-term/term_unix.go"
cp "${term_overlay}/termios_unix.go" "${work}/moby-term/termios_unix.go"
cp "${term_overlay}/termios_nonbsd.go" "${work}/moby-term/termios_nonbsd.go"

echo "prepared ${modfile}"
