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

# Copies go-isatty and makes its js/wasm IsTerminal() honor KCP_TTY=1 for
# fd 1/2. Native CLIs (yq) use isatty to decide whether to colorize output;
# upstream hardcodes false under wasm. The browser shell sets KCP_TTY per
# command when stdout really ends at the terminal.
#
# Usage: scripts/prepare-cli-isatty.sh <dest-dir> <isatty-module-dir>
# Prints the patched module directory.

dest="$1"
src="$2"

rm -rf "${dest}"
cp -a "${src}" "${dest}"
chmod -R u+w "${dest}"

python3 - "${dest}/isatty_others.go" <<'ISATTY'
import sys

path = sys.argv[1]
src = open(path).read()
src = src.replace(
    "package isatty\n",
    'package isatty\n\nimport "os"\n',
    1,
)
src = src.replace(
    """func IsTerminal(fd uintptr) bool {
	return false
}""",
    """func IsTerminal(fd uintptr) bool {
	if fd == 1 || fd == 2 {
		return os.Getenv("KCP_TTY") == "1"
	}
	return false
}""",
    1,
)
src = src.replace(
    """func IsCygwinTerminal(fd uintptr) bool {
	return false
}""",
    """func IsCygwinTerminal(fd uintptr) bool {
	return IsTerminal(fd)
}""",
    1,
)
open(path, "w").write(src)
ISATTY

echo "${dest}"
