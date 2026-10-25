#!/usr/bin/env bash
set -euo pipefail

# Builds the static browser kcp + socialweb shell (scripts/build-kubectl-wasm-web-public.sh)
# and serves it with Caddy on the socialweb.computer host at https://${ORIGIN}.
# The site gets its own /etc/caddy/sites/${ORIGIN}.caddy, so the host's other
# sites (socialweb.computer, k8s., kcp., ssh.) are left alone.
#
# The OAuth client is the page origin: oauth-client-metadata.json is written
# for ${ORIGIN} with the scope the kcp.wasm build wrote (requester-scope.txt).
# The dev server's /debug/* hooks are not part of the bundle; the page notices
# the 404/405 and keeps debug logging local.
#
# Usage: scripts/deploy.sh
#   SKIP_BUILD=1   ship the existing ./public (built for the same ORIGIN)

REPO="$(cd "$(dirname "$0")/.." && pwd)"

SSH_TARGET="${SSH_TARGET:-root@socialweb.computer}"
SSH_PORT="${SSH_PORT:-1997}"
SSH_KEY="${SSH_KEY:-}"
ORIGIN="${ORIGIN:-shell.socialweb.computer}"
PUBLIC_DIR="${PUBLIC_DIR:-${REPO}/public}"
ARTIFACTS="${ARTIFACTS:-${REPO}/bin/wasm}"
SSH_OPTS=(-p "$SSH_PORT" -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=20
  -o ControlMaster=auto -o "ControlPath=${HOME}/.ssh/cm-shell-%r@%h:%p" -o ControlPersist=10m -o ServerAliveInterval=15)
if [ -n "$SSH_KEY" ]; then
  SSH_OPTS+=(-i "$SSH_KEY" -o IdentitiesOnly=yes)
fi

say() {
  echo "deploy: $*"
}

q() {
  printf "%q" "$1"
}

# ── Build ────────────────────────────────────────────────────────────────────
if [ "${SKIP_BUILD:-}" != "1" ]; then
  say "building ${PUBLIC_DIR} from ${ARTIFACTS}"
  "$REPO/scripts/build-kubectl-wasm-web-public.sh" "$PUBLIC_DIR" "$ARTIFACTS"
fi
[ -f "$PUBLIC_DIR/index.html" ] || { say "no build at $PUBLIC_DIR"; exit 1; }
[ -f "$PUBLIC_DIR/requester-scope.txt" ] || { say "no $PUBLIC_DIR/requester-scope.txt: the socialweb kcp.wasm build writes it"; exit 1; }

# ── OAuth client metadata for the production origin ──────────────────────────
SERVE_PUBLIC="file://${REPO}/scripts/serve-public.ts" SCOPE_FILE="$PUBLIC_DIR/requester-scope.txt" ORIGIN="$ORIGIN" deno eval '
const { buildClientMetadata, loadScope } = await import(Deno.env.get("SERVE_PUBLIC"));
const scope = await loadScope(Deno.env.get("SCOPE_FILE"), true);
console.log(JSON.stringify(buildClientMetadata(`https://${Deno.env.get("ORIGIN")}`, scope), null, 2));
' > "$PUBLIC_DIR/oauth-client-metadata.json"
say "client_id https://${ORIGIN}/oauth-client-metadata.json"

# ── Ship ─────────────────────────────────────────────────────────────────────
STAGE="/var/www/.stage-${ORIGIN}"
say "shipping $(du -sh "$PUBLIC_DIR" | cut -f1) to ${SSH_TARGET}:${STAGE}"
tar cf - -C "$PUBLIC_DIR" . | ssh "${SSH_OPTS[@]}" "$SSH_TARGET" \
  "rm -rf $(q "$STAGE") && mkdir -p $(q "$STAGE") && tar xf - -C $(q "$STAGE")"

# ── Remote: swap the site in, Caddy site block ───────────────────────────────
ssh "${SSH_OPTS[@]}" "$SSH_TARGET" "ORIGIN=$(q "$ORIGIN") STAGE=$(q "$STAGE") bash -s" <<'REMOTE_EOF'
set -euo pipefail

say() {
  echo "remote: $*"
}

command -v caddy >/dev/null || { say "no caddy on this host"; exit 1; }

ROOT="/var/www/${ORIGIN}"
chown -R caddy:caddy "$STAGE"
rm -rf "${ROOT}.old"
if [ -d "$ROOT" ]; then
  mv "$ROOT" "${ROOT}.old"
fi
mv "$STAGE" "$ROOT"
rm -rf "${ROOT}.old"

# Nothing in the bundle is content-hashed, so every file revalidates. The
# *.wasm.gz modules are served as application/gzip and inflated by the
# workers; encode leaves them alone.
install -d -m 0755 /etc/caddy/sites
cat > "/etc/caddy/sites/${ORIGIN}.caddy" <<SITE
${ORIGIN} {
  root * ${ROOT}
  encode gzip
  header Cache-Control "no-cache"
  @config path /config
  header @config Content-Type application/json
  rewrite /oauth/callback/* /oauth-callback.html
  file_server
}
SITE
if ! grep -qE '^[[:space:]]*import[[:space:]]+/etc/caddy/sites/\*\.caddy' /etc/caddy/Caddyfile; then
  cp /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.bak.$(date +%s)"
  printf '\nimport /etc/caddy/sites/*.caddy\n' >> /etc/caddy/Caddyfile
fi
caddy validate --config /etc/caddy/Caddyfile >/dev/null
systemctl reload caddy || systemctl restart caddy
say "https://${ORIGIN} -> ${ROOT}"
REMOTE_EOF

# ── Check ────────────────────────────────────────────────────────────────────
for _ in 1 2 3 4 5 6; do
  if curl -fsS "https://${ORIGIN}/oauth-client-metadata.json" | grep -q "\"client_id\": \"https://${ORIGIN}/oauth-client-metadata.json\""; then
    say "done: https://${ORIGIN}"
    exit 0
  fi
  sleep 5
done
say "https://${ORIGIN}/oauth-client-metadata.json does not answer with its client_id yet (certificate still issuing?)"
exit 1
