#!/usr/bin/env bash
# airlift VPS bootstrap — idempotent. Usage: bootstrap.sh <public_url>
#   e.g. bootstrap.sh https://projects.sujaykumar.dev/airlift
# Creates the service user + state dir, ensures an 0600 config with that
# public_url and a generated admin_token (preserved on re-runs), seeds the
# projects hub page if absent, and installs Caddy. The systemd unit, Caddyfile
# and hub source are placed by `make vps-bootstrap`.
set -euo pipefail
PUBLIC_URL="${1:?usage: bootstrap.sh <public_url>}"

id airlift &>/dev/null || useradd --system --home-dir /var/lib/airlift --shell /usr/sbin/nologin airlift
mkdir -p /var/lib/airlift/.airlift

CFG=/var/lib/airlift/.airlift/config
if [ -f "$CFG" ]; then
  grep -q '^public_url' "$CFG" \
    && sed -i "s|^public_url = .*|public_url = $PUBLIC_URL|" "$CFG" \
    || echo "public_url = $PUBLIC_URL" >> "$CFG"
  echo "updated public_url; kept the existing admin_token"
else
  cat > "$CFG" <<EOF
# airlift tower configuration (managed on the VPS; holds the admin token).
public_url = $PUBLIC_URL
admin_token = $(openssl rand -hex 24)
EOF
  echo "wrote $CFG with a fresh admin_token"
fi
chmod 600 "$CFG"
chown -R airlift:airlift /var/lib/airlift
chmod 750 /var/lib/airlift

# projects hub — seed a placeholder only; never clobber a customised page
mkdir -p /var/www/projects
if [ ! -f /var/www/projects/index.html ] && [ -f /tmp/airlift-landing.html ]; then
  mv /tmp/airlift-landing.html /var/www/projects/index.html
  echo "seeded the projects hub placeholder"
fi
rm -f /tmp/airlift-landing.html

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
