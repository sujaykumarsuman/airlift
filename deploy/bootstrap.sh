#!/usr/bin/env bash
# airlift VPS bootstrap — idempotent. Creates the service user + state dir, an
# 0600 config with a generated admin_token (kept on re-runs), and installs Caddy.
# The systemd unit and Caddyfile are placed by `make vps-bootstrap`. Usage:
#   bootstrap.sh <public-hostname>
set -euo pipefail
DOMAIN="${1:?usage: bootstrap.sh <public-hostname>}"

id airlift &>/dev/null || useradd --system --home-dir /var/lib/airlift --shell /usr/sbin/nologin airlift
mkdir -p /var/lib/airlift/.airlift

CFG=/var/lib/airlift/.airlift/config
if [ ! -f "$CFG" ]; then
  cat > "$CFG" <<EOF
# airlift tower configuration (managed on the VPS; holds the admin token).
public_url = https://$DOMAIN
admin_token = $(openssl rand -hex 24)
EOF
  echo "wrote $CFG with a fresh admin_token"
else
  echo "keeping the existing $CFG (admin_token preserved)"
fi
chmod 600 "$CFG"
chown -R airlift:airlift /var/lib/airlift
chmod 750 /var/lib/airlift

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
echo "bootstrap complete for https://$DOMAIN — run 'make deploy' to push the binary"
