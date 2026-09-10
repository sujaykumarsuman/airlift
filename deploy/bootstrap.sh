#!/usr/bin/env bash
# airlift VPS bootstrap — idempotent. Usage: bootstrap.sh <public_url>
#   e.g. bootstrap.sh https://projects.sujaykumar.dev/airlift
# Creates the service user + state dir, ensures an 0600 config with that
# public_url and an 0600 /etc/default/airlift env file holding a generated admin
# token (both preserved on re-runs), ensures the hub webroot exists, and installs
# Caddy. The systemd unit and Caddyfile are
# placed by `make vps-bootstrap`; the hub page at /var/www/projects lives in the
# sujaykumarsuman.github.io repo and is deployed separately.
set -euo pipefail
PUBLIC_URL="${1:?usage: bootstrap.sh <public_url>}"

id airlift &>/dev/null || useradd --system --home-dir /var/lib/airlift --shell /usr/sbin/nologin airlift
mkdir -p /var/lib/airlift/.airlift

CFG=/var/lib/airlift/.airlift/config
if [ -f "$CFG" ]; then
  grep -q '^public_url' "$CFG" \
    && sed -i "s|^public_url = .*|public_url = $PUBLIC_URL|" "$CFG" \
    || echo "public_url = $PUBLIC_URL" >> "$CFG"
  echo "updated public_url in $CFG"
else
  cat > "$CFG" <<EOF
# airlift tower configuration (managed on the VPS). The admin token lives in the
# systemd EnvironmentFile /etc/default/airlift, not here.
public_url = $PUBLIC_URL
EOF
  echo "wrote $CFG"
fi
chmod 600 "$CFG"

# The admin token is an environment variable (AIRLIFT_ADMIN_TOKEN) the systemd
# unit loads from /etc/default/airlift (0600, root-owned; env wins over the config
# file). Keep an existing token, else migrate one from an older config, else
# generate a fresh one — so re-runs never rotate a live token.
ENVFILE=/etc/default/airlift
if [ -f "$ENVFILE" ] && grep -q '^AIRLIFT_ADMIN_TOKEN=' "$ENVFILE"; then
  echo "kept the existing admin token in $ENVFILE"
else
  TOKEN="$(sed -n 's/^admin_token = //p' "$CFG" 2>/dev/null | head -n1)"
  [ -n "$TOKEN" ] || TOKEN="$(openssl rand -hex 24)"
  cat > "$ENVFILE" <<EOF
# airlift tower environment (managed on the VPS; holds the admin token).
AIRLIFT_ADMIN_TOKEN=$TOKEN
EOF
  echo "wrote $ENVFILE with the admin token"
fi
chmod 600 "$ENVFILE"
chown root:root "$ENVFILE"

# The token now lives only in the env file; drop any copy left in the config.
sed -i '/^admin_token/d' "$CFG"

chown -R airlift:airlift /var/lib/airlift
chmod 750 /var/lib/airlift

# projects hub webroot — Caddy serves it at /; the page source lives in the
# sujaykumarsuman.github.io repo (projects/index.html) and is deployed separately.
mkdir -p /var/www/projects

if ! command -v caddy &>/dev/null; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get install -y -q debian-keyring debian-archive-keyring apt-transport-https curl gnupg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' > /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -q
  apt-get install -y -q caddy
fi

systemctl daemon-reload
systemctl enable caddy airlift >/dev/null 2>&1 || true
echo "bootstrap complete: airlift at $PUBLIC_URL"
