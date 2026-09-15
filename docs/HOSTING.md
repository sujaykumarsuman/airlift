# Hosting airlift on a VPS

> **Note (2026-09-15):** the live tower now runs on **k3s**, not Caddy + systemd.
> See [`docs/build-plan/k3s-migration.md`](build-plan/k3s-migration.md) and
> `deploy/k8s/` for the current deployment. The Caddy + systemd instructions
> below are the previous setup, retired at the k3s cutover and kept for
> reference/rollback.

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
| config | `/var/lib/airlift/.airlift/config` | mode `0600`, owned by `airlift`; `public_url` carries the `/airlift` prefix |
| admin token | `/etc/default/airlift` | mode `0600`, root-owned; `AIRLIFT_ADMIN_TOKEN=…`, loaded by the systemd unit as an env var (env wins over the config file) |
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
config with `public_url=https://<domain><prefix>`, an `0600` `/etc/default/airlift`
holding a freshly generated `AIRLIFT_ADMIN_TOKEN` (migrating any token from an
older config, so a re-run never rotates a live token), seeds the hub page if
absent, and installs Caddy. `deploy` builds
the Linux binary and rolls it out with a rename + `systemctl restart` (no downtime
for the binary swap). Caddy issues the certificate as soon as the A record
resolves; verify with `curl https://<domain><prefix>/api/info`.

`VPS`, `DOMAIN`, `PREFIX` and `PUBLIC_URL` (= `https://$(DOMAIN)$(PREFIX)`) default
to the live host in the `Makefile`.

## Updating

Push a new build with `make deploy`. The web UI is embedded in the binary, so
there is nothing else to copy. The binary is stamped with `VERSION` (`git
describe --tags` by default), which the tower reports at `/api/info` and in the
landing footer — so **deploy from a tag**: cut the release first
(`git tag -a vX.Y.Z -m "…" && git push origin vX.Y.Z`; the release workflow
builds and publishes `airlift-<os>-<arch>`), then `make deploy`. An untagged
HEAD still deploys, but `make deploy` says so and the tower reports something
like `v0.1.1-3-gabc1234`. Sessions are memory-only, so a restart drops any
in-flight transfer (expected — see the non-goals in `CLAUDE.md`).

## Configuration

Every setting lives in `/var/lib/airlift/.airlift/config` (flat `key = value`;
`docs/API.md` and `internal/config/registry.go` list them). Restart-only keys —
`public_url`, `listen`, `admin_token`, `data_dir`, `trusted_proxies` — need an
`airlift.service` restart. The `admin_token` is supplied as the
`AIRLIFT_ADMIN_TOKEN` environment variable from `/etc/default/airlift`, not the
config file (env beats the config file in the precedence above). The rest are
live-editable from `/admin` → Settings,
which writes `/var/lib/airlift/.airlift/overrides` (atomic, `0600`) and applies
them without a restart (ADR 0014). Precedence is flag > env `AIRLIFT_<KEY>` >
overrides > config file > default.

## The admin surface

`/admin` is gated by `admin_token` (never logged, masked in the config dump). It is
supplied to the tower as the `AIRLIFT_ADMIN_TOKEN` environment variable from
`/etc/default/airlift` (mode `0600`, root-owned). Sign in with:

```
ssh airlift-vps "grep '^AIRLIFT_ADMIN_TOKEN' /etc/default/airlift"
```

### Rotating the admin token

The admin token is restart-only, so rotate it on the box and restart:

```
ssh airlift-vps
NEW=$(openssl rand -hex 24)
sed -i "s|^AIRLIFT_ADMIN_TOKEN=.*|AIRLIFT_ADMIN_TOKEN=$NEW|" /etc/default/airlift
systemctl restart airlift
```

Any open `/admin` tab is signed out on the next call; sign in again with the new
token.

## Removal

```
ssh airlift-vps
systemctl disable --now airlift caddy
rm -f /etc/systemd/system/airlift.service /usr/local/bin/airlift /etc/default/airlift
rm -rf /var/lib/airlift
apt-get purge -y caddy          # optional
userdel airlift                 # optional
```
