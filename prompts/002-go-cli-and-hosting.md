# airlift — prompt 002: one Go binary, hosted multi-user tower, admin

Continues `prompts/001-init.md`, whose four phases are on `main`. Read
`CLAUDE.md`, `STATUS.md`, `docs/PROTOCOL.md`, `docs/API.md` and
`docs/adr/` first; they describe what exists. This prompt changes the
shape of the project and adds four phases. Where it contradicts a locked
decision, it supersedes it: record each supersession as a new ADR (0010
onwards) and update `CLAUDE.md`; never edit `prompts/001-init.md` or the
old ADRs beyond a "Superseded by" status line.

The repository will be made public at the end of this work: no secrets, no
credentials, no personal infrastructure baked into code or docs. A domain
or address appears only as an example value or a `deploy/` variable with a
placeholder default.

## What changes

1. **One Go binary, `airlift`, and a standard CLI.** The Python sender and
   `tools/repobundle.py` are retired. `airlift` packs a folder, a file or a
   list of files into a repobundle, produces the beam page, runs the tower,
   and talks to a hosted tower as a client. It reads `~/.airlift/config`;
   flags override it. The air-gapped machine gets one static binary. The
   web UI stays TypeScript; it runs in browsers. `make` builds, tests,
   packages and deploys artifacts; it never runs the application.
2. **The tower is a hosted, public, multi-user service.** Anyone can create
   a session and anyone can join one with its QR code or link, or with its
   password when the creator set one. There are no user accounts: the
   identities are per-session tokens, per-client names bound to addresses,
   and one airlift admin token. The tower speaks plain HTTP behind a
   reverse proxy that terminates TLS with real certificates; for
   development it runs on `localhost`, which browsers already treat as a
   secure context.
3. **Sessions have a lifecycle beyond the transfer.** They open, they can
   be terminated with warning, a terminated session lingers for half an
   hour so its clients can ask for an extension, the airlift admin reviews
   such requests, and only then is the data removed. Session files live
   under `~/.airlift/data/<session-id>/` on the tower's host for exactly
   that lifetime. Nothing survives a tower restart. `--dest` goes away.
4. **An admin dashboard** at `<public_url>/admin`, behind the admin token,
   manages every session (terminate with warning, evict, download, review
   extension requests) and the adjustable server settings.

## Roles (restate in CLAUDE.md; use these names everywhere)

Beam, tower, scanner, dashboard viewer as before, plus:

- **Client** — one participant in a session: one per address, with a
  temporary unique name and a client id, holding any number of scanner or
  viewer streams.
- **Session admin** — a client that may terminate the session, evict other
  clients and set or clear the password. The creator always is one; joiners
  are session admins only when the creator chose `joiners_admin` at
  creation.
- **Airlift admin** — the operator, identified by the admin token, who
  manages all sessions and the server through `/admin`.

The beam is written by `airlift beam`; it is still one self-contained HTML
file that never talks to anything. The tower runs on the operator's VPS,
or on `localhost` while developing.

## Facts that constrain the design

- `getUserMedia` needs a secure context: HTTPS from the proxy in
  production, `localhost` in development. Nothing else is supported, and
  the docs say so.
- Creation is open, so every public route is rate-limited per address,
  every session is bounded in size and lifetime by server caps that
  creators can only tighten, and the session count is capped.
- Clients are identified by address. Behind the proxy the address is the
  first `X-Forwarded-For` entry, trusted only when the request arrives
  from `trusted_proxies` (default loopback); otherwise the peer address.
  Two phones behind one NAT are one client; the docs say so and the
  transfer still works, since frames are merged regardless of who relays.
- The tower lives under a path prefix, `/airlift`, on
  `projects.sujaykumar.dev`. The proxy strips the prefix; the tower must
  still emit correct absolute links from its configured public URL.
- The Python-generated `vectors.json` and `vectors-fountain.json` are the
  frozen cross-language contract for the frame format and the fountain
  index sets. Keep them, move them to `testdata/vectors/`, and never
  regenerate them from Go.

## Locked decisions for this prompt (record as ADRs)

1. **Single binary `cmd/airlift`** with subcommands `pack`, `unpack`,
   `beam`, `frames`, `decode`, `tower`, `replay`, `sessions`, `fetch`.
   `cmd/tower` is removed; `make all` cross-compiles `airlift` for
   darwin/linux/windows, amd64 and arm64; the release workflow attaches
   those binaries and nothing else.
2. **Configuration: `~/.airlift/config`**, a flat `key = value` file with
   `#` comments, parsed by hand. `AIRLIFT_HOME` relocates the directory;
   `--config FILE` names another file; every key has a flag of the same
   name; precedence is flag, then environment (`AIRLIFT_<KEY>`), then
   `~/.airlift/overrides` (written by the admin dashboard), then the file,
   then default. Keys: `public_url`, `listen` (default `127.0.0.1:8443`),
   `admin_token`, `data_dir` (default `~/.airlift/data`),
   `trusted_proxies` (`127.0.0.1,::1`), `sessions` (32), `max_gz_bytes`
   (64 MiB, the ceiling creators can lower), `idle_ttl` (10m),
   `inactive_ttl` (30m), `max_age` (0, off), `warning_ttl` (1m),
   `terminated_ttl` (30m), `review_ttl` (24h), `max_body` (8 MiB), and the
   per-address rates `rate_create` (5/min), `rate_join` (10/min),
   `rate_frames` (30/s), `rate_ping` (2/min), `rate_extension` (3/h),
   `rate_admin` (10/min). A file holding `admin_token` must be mode 0600
   or the tower refuses to start. The client side reads `public_url` (for
   `replay`) and `admin_token` (for `sessions` and `fetch`) from the same
   file.
3. **`pack` is a byte-for-byte port of `repobundle.py pack`.** Same header,
   same entry lines, same text and base64 rules (binaries skipped in text
   format, boundary markers refused, base64 wrapped at 120), same git-aware
   file list (`git ls-files -z` plus untracked-not-ignored, sorted; plain
   walk without `.git` as the fallback), same mode field, output file and
   symlinks excluded, `--files-from`, explicit paths, `--root`. The four
   committed bundles under `testdata/bundles/` must be reproduced exactly;
   that test is what allows `tools/repobundle.py` to be deleted. Write the
   format down in `docs/BUNDLE.md` before deleting the script.
4. **`beam` takes either a file or a tree.** `--in FILE` sends the file as
   is; `--root DIR [PATHS...] [--files-from LIST] [--format base64|text]`
   packs first (base64 by default) and beams the bundle, writing the bundle
   next to the page unless `--no-bundle`; `--dump FILE` also writes the
   frames dump that `replay` and `decode` read. All the 001 flags remain:
   `--chunk`, `--version-target`, `--ecc`, `--fps`, `--manifest-every`,
   `--seed`, `--fountain`, `--fountain-packets`. Fountain packet count and
   `FountainIndices` come from `internal/proto`; the encoder (gzip →
   chunks → frames, both modes) moves to one package that `beam`, `frames`
   and `replay` share.
5. **QR encoding in Go with one forced version per beam**, alphanumeric
   mode, ECC L/M/Q/H, mask chosen by penalty score. Either `rsc.io/qr/coding`
   (forced plan, `Alpha` encoding; implement the mask penalty if it does
   not) or `github.com/skip2/go-qrcode` (`NewWithForcedVersion`); choose,
   justify in the ADR, and keep the dependency list in CLAUDE.md current.
   The SVG path renderer and the HTML player move verbatim into Go, the
   player as an embedded template; the emitted page stays self-contained.
6. **The tower is HTTP only.** Remove `internal/tlsca`, `/ca.crt`, the CA
   hint, the "first run on a phone" section, LAN detection and `--bind`.
   Add `GET /api/info` (unauthenticated): `{version, public_url,
   base_path, admin_enabled, caps: {max_gz_bytes, idle_ttl, inactive_ttl,
   max_age, sessions}}`, which the pages use for guidance and form limits.
7. **Sessions are created openly, with options, and joined by token or
   password.** `POST /api/sessions` takes an optional body `{label,
   password, joiners_admin, max_gz_bytes, idle_ttl, inactive_ttl}`; every
   limit is clamped to the server cap, and a request above a cap is a 400
   that names the cap. It registers the creator as the first client, a
   session admin, and returns `{sid, token, client_id, name, join_url,
   expires_at}`. The join link and QR carry the token in the fragment as
   before. When a password is set the session is also joinable at
   `/s/{sid}` and `/#s={sid}` without a token: `POST
   /api/sessions/{sid}/join` with `{password, name}` returns `{token,
   client_id, name}`; without a password that route is 404. Joiners by
   either path are session admins when `joiners_admin` is set, plain
   clients otherwise. Passwords are stored as salted SHA-256 in memory and
   compared in constant time; join attempts are rate-limited per address
   and per session. Session-admin actions (`DELETE /api/sessions/{sid}`,
   `DELETE /api/sessions/{sid}/clients/{cid}`, `PATCH /api/sessions/{sid}`
   for the password) require the session token plus a session-admin
   client id; everything else requires the session token plus a
   registered client id.
8. **One client per address per session.** `POST /api/sessions/{sid}/
   clients` with `{name?, role}` registers or returns the client already
   bound to the caller's address: `{client_id, name, session_admin,
   roles}`. Names are generated (two short lowercase words) or proposed,
   made unique within the session by a numeric suffix. The client id is
   sent as `X-Airlift-Client` on the SSE subscription and every POST and
   must match the caller's address; a client may hold several streams and
   its `roles` are those of its open streams. Eviction marks the client's
   address for the session: its streams receive `event: evicted` and
   close, its POSTs and a re-registration from that address are 403 with
   `{error: "evicted"}`. The snapshot lists clients `{name, roles,
   session_admin, connected, last_active}`; the dashboard shows them and
   the scan page shows its own name. `airlift replay` registers as a
   scanner named `replay`.
9. **Activity, clocks and files.** Activity is a frames POST that
   accepted or duplicated at least one frame, a download, or a ping
   (`POST /api/sessions/{sid}/ping`, sent by pages once a minute only while
   visible and within five minutes of pointer, key or touch input). An open
   stream alone is presence, not activity. While a session is OPEN three
   clocks run, from the session's own limits clamped to the caps:
   `idle_ttl` after the last client leaves, `inactive_ttl` after the last
   activity while clients are connected, `max_age` overall when set;
   reaching any of them terminates the session (decision 10). On READY the
   tower writes `<data_dir>/<sid>/<name>` (the raw file), `<stem>/` (the
   unpacked tree, when a bundle), `<stem>.zip` (when the bundle has more
   than one file) and `session.json` (sid, label, name, sizes, hashes,
   verdicts, bundle summary, sender session, `started_at`, `finished_at`,
   clients, lifecycle events), then serves downloads from those files and
   frees the in-memory copies. A FAILED session writes nothing. On start
   the tower empties `data_dir`. The snapshot's `dest_path` becomes
   `saved_path`; `expires_at` is the earliest applicable deadline and
   moves with activity.
10. **Lifecycle.** Alongside the transfer `state`, every session has a
    `status`: `OPEN` → `TERMINATING` → `TERMINATED` → `PENDING_REVIEW` →
    `OPEN` or `REJECTED` → gone.
    - The airlift admin terminating a session starts a `warning_ttl`
      countdown: status `TERMINATING`, `terminate_at` in the snapshot, and
      `event: terminating {by: "airlift admin", at}` on every stream; the
      pages show a banner with the countdown. The admin can cancel within
      the minute, or pass `now` to skip it. A session admin's own
      termination, and any clock in decision 9, terminate at once with the
      reason recorded.
    - `TERMINATED`: the transfer state is frozen, relays are refused, the
      files stay, and for `terminated_ttl` every stream and every join or
      register attempt sees the terminated page: who ended it and why,
      time left, and one extension request per session with a reason
      (`POST /api/sessions/{sid}/extension {reason}`, any client, even one
      arriving now). No request in time → the session and its directory
      are deleted.
    - `PENDING_REVIEW`: the clock stops; the request and its author show
      on the admin dashboard, which decides (`POST /api/admin/sessions/
      {sid}/review {decision: accept|reject, note}`). Accept → `OPEN`
      again with the original configuration, transfer state and files, the
      clocks restarted; every waiting page returns to the session by
      itself. Reject → `REJECTED` with the note shown, deleted
      `terminated_ttl` after the decision. A request older than
      `review_ttl` with no decision counts as rejected.
    The snapshot carries `status`, `terminate_at`, `terminated: {by,
    reason, at, cleanup_at}` and `extension: {by, reason, at, decision,
    note, decided_at}`, and every transition is an SSE event and a
    `session.json` entry.
11. **Limits answer clearly:** 413 for a body over `max_body`, 429 with
    `Retry-After` for a rate limit or the session cap, a FAILED session
    with a stated reason for a manifest over its `max_gz_bytes`, 409 for
    an action the current status does not allow.
12. **`public_url`** carries scheme, host, port and path prefix
    (`https://projects.sujaykumar.dev/airlift`; default
    `http://localhost:8443`). Its path drives `<base href>` injected into
    the served HTML, every generated link, service worker registration and
    scope, and relative URLs inside the web manifest. The router stays
    rooted; the proxy strips the prefix. Vite builds with `base: './'` and
    every client URL resolves against `document.baseURI`.
13. **Admin.** `admin_token` in the config enables `/admin` (a third Vite
    entry) and `/api/admin/*`, all requiring `Authorization: Bearer
    <admin_token>`; without it they are 404 and `/api/info` says
    `admin_enabled: false`. The admin page has a login form that keeps the
    token in `sessionStorage` after a successful `GET /api/admin/config`.
    Routes: `GET /api/admin/sessions` and `GET /api/admin/events` (SSE of
    the same list: sid, label, status, state, sizes, clients with name,
    roles, address, connected and last activity, created, `expires_at`,
    pending extension), `DELETE /api/admin/sessions/{sid}[?now]` and
    `POST .../cancel-termination`, `DELETE /api/admin/sessions/{sid}/
    clients/{cid}`, `POST /api/admin/sessions/{sid}/review`, `GET
    /api/admin/sessions/{sid}/download?as=raw|file|zip`, `GET
    /api/admin/config` (effective settings and their sources) and `PATCH
    /api/admin/config` for the keys that can change live (`sessions`,
    `max_gz_bytes`, the `*_ttl` keys, `max_age`, `max_body`, every
    `rate_*`), applied at once and persisted to `~/.airlift/overrides`;
    `listen`, `public_url`, `data_dir`, `trusted_proxies` and
    `admin_token` need a restart and the page says so. The admin page
    shows pending reviews first. Admin logins are rate-limited; the token
    is never logged. `airlift sessions` and `airlift fetch SID [--as
    raw|file|zip] [--out DIR]` are CLI clients of these routes, `fetch`
    writing into the laptop's own `<data_dir>/<sid>/`, where nothing
    expires.
14. **`replay` targets.** `airlift replay FILE --into JOIN_URL` joins an
    existing session; `airlift replay FILE` with `public_url` configured
    creates a session on that tower (creation is open), prints the join
    link and feeds it; with nothing configured it runs the private loopback
    tower as today, which CI relies on.
15. **Deployment is a systemd unit plus Caddy**, checked in under
    `deploy/`: the `airlift` user with a real home so `~/.airlift/` is
    literal; a unit running `airlift tower`; a Caddyfile serving
    `{$AIRLIFT_DOMAIN}` with `handle_path /airlift/*` reverse-proxied to
    `127.0.0.1:8443`; Makefile targets `vps-bootstrap` (packages, user,
    directories, a config with a generated `admin_token` written on the
    server and never in the repo), `deploy` (build for the VPS's
    architecture, copy the binary, restart, health check via `/api/info`)
    and `vps-logs`. Host and user come from variables with placeholder
    defaults. No Docker: the binary is static.

## Conventions (additions and removals)

- Remove `sender/`, `uv`, `ruff`, `pytest` and their pre-commit hooks and CI
  job; the gate is `gofmt` + `go vet` + `go test ./...` and the web trio.
  Keep `.gitattributes` `-text` rules for `testdata/`.
- `internal/bundle` gains `Pack`; `internal/beam` (or similar) holds the
  encoder, QR rendering and player; `internal/config` the file, overrides,
  env and flag merge; `internal/session` the clients, clocks, lifecycle
  and on-disk layout; `internal/server` the public, session and admin
  routes, limits, address resolution and base path; `internal/replay`
  loses its private encoder for the shared one. `web/` gains the `admin`
  entry, a join page and a terminated page; keep the entries sharing one
  small style and API module.
- Every removed feature leaves no dead flag, doc line or test behind.
- British English in docs. Conventional Commits. One branch per phase,
  squash to `main`, stop after each phase for review. `STATUS.md` at the
  end of every phase. Ultra-concise output.

## Phases

### Phase 5 — The `airlift` binary

`cmd/airlift` with `pack`, `unpack`, `beam`, `frames`, `decode`, `tower`,
`replay` (`sessions` and `fetch` arrive with Phase 7); `pack` and `unpack`
on `internal/bundle`; the shared encoder; `beam` with QR rendering and the
embedded player; `tower` and `replay` moved over unchanged in behaviour
(still `--dest` for now). `docs/BUNDLE.md`. Fixtures moved
(`testdata/vectors/`), Python removed, gate and CI updated, README and
CLAUDE.md rewritten for one binary. ADRs 0010 (single binary, Python
retired) and 0011 (QR library).

Tests: the four bundles reproduced byte for byte; `unpack` of each matches
its tree; a Go end-to-end `pack → frames → replay → READY` whose restored
tree equals the source; the emitted beam checked structurally (frame count,
order, no external references); `--version-target` agrees with the README
table; `frames --fountain` for the multi bundle yields the same
`fountain.indices` as the frozen vectors, and its dump decodes to the
original.

Exit: `airlift beam --root testdata/bundles/multi/tree --fountain --dump
beam.json --out beam.html`, then `airlift replay beam.json --drop 0.3
--shuffle`, reaches READY; gate green. Commit.

### Phase 6 — Sessions, clients, lifecycle, hosted shape

Decisions 2, 6 to 12 and 14. Config file, overrides, env and flags; remove
TLS, LAN detection and `--dest`; open creation with options, caps and
`joiners_admin`; password join; clients bound to addresses, names, roles
and eviction; activity, the three clocks and on-disk session data; the
lifecycle with its warning, terminated window, extension request, review
outcomes and reopening (the admin side of review is exercised through the
API in this phase; the page comes in Phase 7); limits and rate limits;
`public_url` with `<base href>` injection and a test that serves under a
prefix behind an `httptest` handler that strips it; web changes (creation
form with limits from `/api/info` and the `joiners_admin` choice, join
page with name and password, client names on both pages, session-admin
controls, the terminating banner, the terminated page with the extension
form and live status, automatic return on acceptance, pings gated on
visibility and recent input, the clients list and closing countdown,
`saved_path`, relative URLs, manifest and service worker relative); docs
(API.md rewritten for the new routes and the two state machines, README
for the hosted shape and localhost development). ADR 0012 (HTTP only
behind a proxy; supersedes 0008 and decision 13) and ADR 0013 (open
multi-user sessions, clients per address, session admins, the lifecycle,
on-disk session data; supersedes the hosting and multi-user non-goals and
the TTL of ADR 0005).

Tests, beyond units: creation clamps and rejects over-cap options; join
with the right and wrong password, and 404 without one; a second
registration from the same address returns the same client; names are
unique; a client id from another address is 403; an evicted address
cannot re-register and its stream got `evicted`; session-admin actions
refused to a plain client and allowed to a joiner when `joiners_admin`
was set; a session with an open stream outlives `idle_ttl`; one without
terminates at `idle_ttl`; an open but silent stream terminates at
`inactive_ttl` while frames or pings keep resetting it; `max_age` ends an
active one; termination freezes the transfer, refuses relays and keeps the
files; no extension → deleted at `terminated_ttl`; an extension → pending,
clock stopped; accept → `OPEN` with the same configuration and files,
streams told; reject → deleted `terminated_ttl` later; `review_ttl`
expiry counts as rejection; leftovers cleared on start; downloads from
disk with memory released; 413, 429 with `Retry-After`, 409 on
status-forbidden actions, the gz-cap failure reason; tokens, passwords
and the admin token never in the log.

Exit: `airlift tower` on `localhost` with a config file (`idle_ttl = 1m`,
`inactive_ttl = 2m`, `terminated_ttl = 3m`), a session created from the
dashboard with a password, `joiners_admin` off and a lowered size cap,
joined from a second browser tab by password as a named viewer, filled to
READY by `airlift replay --into`, the files under `~/.airlift/data/<sid>/`;
the viewer evicted from the creator's dashboard and unable to rejoin; the
creator ending the session, both tabs showing the terminated page, the
viewer tab requesting an extension, the request accepted through the admin
API, both tabs back in the session with its files; then terminated again
and left alone until the directory is gone; a second session whose tabs are
closed at once and is gone after `idle_ttl` plus `terminated_ttl`; then the
flow behind a prefix-stripping proxy under `/airlift/`. Gate green. Commit.

### Phase 7 — Admin

Decision 13 and the CLI clients. Admin routes and SSE, the `admin` entry
with login, pending reviews first, sessions table with live updates,
terminate with warning and cancel, terminate now, evict, download, the
review form, and the settings page with live keys, restart-only keys and
their sources; overrides file; `airlift sessions` and `airlift fetch`.
Tests: every admin route is 404 without `admin_token` configured and 401
with a wrong token; login rate limit; the warning reaches every stream and
cancel restores `OPEN`; terminate now skips the warning; a review decision
reaches the waiting pages; a PATCH persists to overrides and survives a
restart; the token never appears in logs. ADR 0014 (admin surface, review
and runtime overrides).

Exit: on `localhost`, log in at `/admin`, watch two sessions live,
terminate one with the warning visible on its dashboard and cancel it,
terminate it for real, see its extension request arrive, accept it and
watch the session reopen, evict a client from the other, lower `sessions`
to 1 and see the next creation refused, restart the tower and see the
override hold. Gate green. Commit.

### Phase 8 — The VPS

Operator prerequisites, to confirm before starting: the 1Password MCP is
connected in this session; the Hostinger DNS panel has (or will get, when
asked) an `A` record `projects` → the VPS address; the apex stays on
GitHub Pages.

1. Access: find the VPS SSH key in 1Password (search by host, address,
   "hostinger", "pem"); prefer the 1Password SSH agent; if the key must be
   written, `~/.ssh/airlift-vps.pem` with mode 0600 and a `Host airlift-vps`
   entry in `~/.ssh/config`. Never print, log or commit key material.
2. Survey the VPS before changing it: OS, open ports, web servers, how
   `careerdock` runs (systemd, Docker, pm2, nginx sites), where its data
   lives (databases, volumes, uploads, env files, crontabs). Report.
3. Back up `careerdock` completely: application files, configuration,
   environment files, web server sites, units, crontabs, database dumps
   and Docker volumes, into one archive with a manifest and sha256 sums.
   Download it to the laptop under `~/Backups/careerdock-<date>/`, verify
   the checksum there, and report the size and contents. Only then wipe
   `careerdock` from the VPS: containers, images, volumes, files, units,
   web server configuration, users and crontabs, so ports 80 and 443 are
   free. The operator has authorised the wipe; the verified local backup
   is the condition.
4. `make vps-bootstrap`, then `make deploy`. Caddy obtains the certificate
   once DNS resolves; verify with `curl` and `/api/info`, and that the
   address seen by the tower is the client's, not the proxy's. Put the
   laptop's config in place (`public_url`, `admin_token`).
5. Smoke test from the laptop: create a session on the hosted dashboard
   with a password, join it by password from another browser, fill it with
   `airlift replay --into` to READY, download the zip, log in at `/admin`,
   terminate it with the warning showing on the dashboard, request and
   accept an extension, then end it; then `airlift replay` config-driven,
   `airlift sessions`, `airlift fetch`, and confirm the directory
   disappears after the idle and terminated periods.
6. Docs: `docs/HOSTING.md` (what runs where, config keys, overrides, admin
   token rotation, updating, removal), README pointers. STATUS.md lists the
   hardware runs still pending: a phone scanning a beam via the hosted
   tower, and the monitor runs from phases 3 and 4.

Exit: the hosted dashboard reaches READY through replay over the internet;
the admin page manages it; `careerdock` is gone from the VPS and safe on
the laptop; nothing sensitive in the repo. Commit.

## Before going public (ask the operator, do not decide)

- Licence file (MIT is the obvious default for personal tooling).
- Whether `deploy/` should carry the real domain as the example value.
- Whether the release workflow should build on tag only, or also on `main`.

## Start

Read the documents named at the top. Execute Phase 5. Stop for review.
