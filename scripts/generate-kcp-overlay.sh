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

# Generates a go build -overlay JSON that replaces the apiserver's etcd-only
# storage factory with one that also understands memory://<name> backends.
#
# Usage: scripts/generate-kcp-overlay.sh <output.json>

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="${1:?usage: $0 <output.json>}"

command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }

apiserver_dir="$(cd "${repo_root}" && go list -m -f '{{.Dir}}' k8s.io/apiserver)"
target="${apiserver_dir}/pkg/storage/storagebackend/factory/factory.go"
replacement="${repo_root}/contrib/wasm/_overlay/apiserver/factory.go"

jq -n --arg target "${target}" --arg replacement "${replacement}" \
  '{Replace: {($target): $replacement}}' >"${out}"

echo "wrote ${out}"
