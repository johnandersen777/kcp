#!/usr/bin/env bash
# usage: phone.sh <session|*> '<js body, d = debug handle>' [timeout_s]
# Sends one command to a page over the dev server's --debug-control file and
# prints its result line from the debug log.
set -euo pipefail
session=$1; js=$2; timeout=${3:-30}
id=$(date +%s%N | tail -c 9)
jq -cn --arg s "$session" --arg id "$id" --arg js "$js" --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '{session:$s,id:$id,js:$js,ts:$ts}' >> ${KCP_DEBUG_DIR:-/tmp/claude-kcpwasm}/control.ndjson
end=$((SECONDS + timeout))
while (( SECONDS < end )); do
  line=$(grep -F " result $id " ${KCP_DEBUG_DIR:-/tmp/claude-kcpwasm}/debug.log | tail -1 || true)
  if [[ -n "$line" ]]; then printf '%s\n' "$line" | cut -d' ' -f5- | sed 's/\\n/\n/g'; exit 0; fi
  sleep 0.5
done
echo "no result for $id within ${timeout}s" >&2; exit 1
