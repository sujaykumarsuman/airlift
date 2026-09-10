# Hosting airlift on a VPS

The tower runs as a plain-HTTP service on `127.0.0.1:8443`, with **Caddy** in
front terminating TLS and reverse-proxying to it (ADR 0012). Caddy obtains and
renews the certificate from Let's Encrypt automatically. This is the Phase 8
deployment; the live host is `projects.sujaykumar.dev`, a **projects hub** where
each project lives under a path prefix — airlift is at
**`https://projects.sujaykumar.dev/airlift`** (admin at `…/airlift/admin`). Caddy
strips the `/airlift` prefix before proxying, so the tower's router stays rooted;
`public_url` carries the prefix and the tower injects `<base href="/airlift/">`.

## What runs where

| Piece | Location | Notes |
| --- | --- | --- |
| `airlift` binary | `/usr/local/bin/airlift` | static Linux/amd64, web UI embedded |
| tower service | `systemd` unit `airlift.service` | runs as the unprivileged `airlift` user, `AIRLIFT_HOME=/var/lib/airlift/.airlift` |
| config | `/var/lib/airlift/.airlift/config` | mode `0600`, owned by `airlift`; holds `admin_token`; `public_url` carries the `/airlift` prefix |
| session data | `/var/lib/airlift/.airlift/data/` | emptied on every start (memory-only sessions, ADR 0005) |
| TLS + proxy | `caddy.service`, `/etc/caddy/Caddyfile` | listens on `80`/`443`; strips `/airlift` → `127.0.0.1:8443`; serves the hub at `/` |
| projects hub | `/var/www/projects/index.html` | the landing page at `/`; source in the `sujaykumarsuman.github.io` repo (`projects/index.html`), deployed separately |
| certificates | Caddy's data dir | auto-provisioned/renewed from Let's Encrypt |

The tower sees the real client address because Caddy (on `127.0.0.1`, in the
default `trusted_proxies`) forwards it in `X-Forwarded-For`, which the tower reads
right-to-left (ADR 0012).

## The projects hub

`/` is served by Caddy from `/var/www/projects/`. Its page is **not** part of this
repo — it lives in `sujaykumarsuman.github.io` (`projects/index.html`). Deploy an
update to the VPS from that repo:

```
scp projects/index.html airlift-vps:/var/www/projects/index.html
```

## Adding another project

Mount it under its own prefix in `/etc/caddy/Caddyfile` (next to airlift's block)
and add a link to the hub page (in the `sujaykumarsuman.github.io` repo):

```
handle_path /careerdock/* {
	reverse_proxy 127.0.0.1:9000   # that project's local port
}
```

## First deploy

Prerequisites: an ssh host alias for the VPS (e.g. `airlift-vps` in
`~/.ssh/config`) with a key, and a DNS **A record** for your hostname pointing at
the VPS. Then, from the repo:

```
make vps-bootstrap VPS=airlift-vps DOMAIN=projects.sujaykumar.dev PREFIX=/airlift
make deploy        VPS=airlift-vps
```

`vps-bootstrap` (idempotent) installs the systemd unit and the Caddyfile (with
`{{DOMAIN}}`/`{{PREFIX}}` substituted), creates the `airlift` user and an `0600`
config with `public_url=https://<domain><prefix>` and a freshly generated
`admin_token`, seeds the hub page if absent, and installs Caddy. `deploy` builds
the Linux binary and rolls it out with a rename + `systemctl restart` (no downtime
for the binary swap). Caddy issues the certificate as soon as the A record
resolves; verify with `curl https://<domain><prefix>/api/info`.

`VPS`, `DOMAIN`, `PREFIX` and `PUBLIC_URL` (= `https://$(DOMAIN)$(PREFIX)`) default
to the live host in the `Makefile`.

## Updating

Push a new build with `make deploy`. The web UI is embedded in the binary, so
there is nothing else to copy. Sessions are memory-only, so a restart drops any
in-flight transfer (expected — see the non-goals in `CLAUDE.md`).

## Configuration

Every setting lives in `/var/lib/airlift/.airlift/config` (flat `key = value`;
`docs/API.md` and `internal/config/registry.go` list them). Restart-only keys —
`public_url`, `listen`, `admin_token`, `data_dir`, `trusted_proxies` — need an
`airlift.service` restart. The rest are live-editable from `/admin` → Settings,
which writes `/var/lib/airlift/.airlift/overrides` (atomic, `0600`) and applies
them without a restart (ADR 0014). Precedence is flag > env `AIRLIFT_<KEY>` >
overrides > config file > default.

## The admin surface

`/admin` is gated by `admin_token` (never logged, masked in the config dump). Sign
in with the token from the config file:

```
ssh airlift-vps "grep '^admin_token' /var/lib/airlift/.airlift/config"
```

### Rotating the admin token

`admin_token` is restart-only, so rotate it on the box and restart:

```
ssh airlift-vps
NEW=$(openssl rand -hex 24)
sed -i "s|^admin_token = .*|admin_token = $NEW|" /var/lib/airlift/.airlift/config
systemctl restart airlift
```

Any open `/admin` tab is signed out on the next call; sign in again with the new
token.

## Removal

```
ssh airlift-vps
systemctl disable --now airlift caddy
rm -f /etc/systemd/system/airlift.service /usr/local/bin/airlift
rm -rf /var/lib/airlift
apt-get purge -y caddy          # optional
userdel airlift                 # optional
```
