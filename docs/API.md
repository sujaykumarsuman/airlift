# HTTP API

Canonical. The tower serves plain HTTP under a path prefix behind a
TLS-terminating reverse proxy (ADR 0007, ADR 0012); the proxy strips the prefix
and the router stays rooted. `internal/server` implements it; `internal/replay`
and the server tests are its reference clients.

## Routes

The middle column is the auth tier (see Auth): *public*, *token* (a valid token,
no client yet), *client* (token + a registered client), *s-admin* (a session-admin
client), *a-admin* (the airlift admin, `admin_token`).

```
POST   /api/sessions                  public → body {label?,password?,joiners_admin?,
                                               max_gz_bytes?,idle_ttl?}
                                             → {sid, token, client_id, name, join_url, expires_at}
POST   /api/sessions/{sid}/join       public → body {password, name?} → {token, client_id, name}
POST   /api/sessions/{sid}/clients    token  → body {name?, role?} → {client_id, name, session_admin, roles}
                                             (reopens a session suspended by inactivity — ADR 0018)
GET    /api/sessions/{sid}            client → place snapshot
GET    /api/sessions/{sid}/events     client → SSE place snapshots
POST   /api/sessions/{sid}/frames     client → body {frames:[base45,...]}
                                             → {accepted, dup, bad, completed_beams}
POST   /api/sessions/{sid}/ping       client → 204; activity, resets the inactive clock
POST   /api/sessions/{sid}/extension  client → body {reason?}; 204; request more time (→ PENDING_REVIEW)
GET    /api/sessions/{sid}/download?beam=<bid>&as=raw|file|zip  client → bytes (409 while suspended)
PATCH  /api/sessions/{sid}            s-admin → body {password} (set or, with "", clear)
POST   /api/sessions/{sid}/max-age    s-admin → 200 {expires_at}; +1h before the max_age cap (ADR 0018)
POST   /api/sessions/{sid}/knock      public → body {name?} → {id, status}; ask to be admitted (ADR 0021)
GET    /api/sessions/{sid}/knock      public → {status: pending|admitted|denied|none, token?} (poll)
POST   /api/sessions/{sid}/knock/{kid}  s-admin → body {decision:"admit"|"deny"}; resolve a pending knock
DELETE /api/sessions/{sid}/clients/{cid}  s-admin → evict a client's address
DELETE /api/sessions/{sid}/beams/{bid}    s-admin → remove a beam and its files
DELETE /api/sessions/{sid}[?hard]     s-admin → soft-terminate (freeze, keep files), or ?hard purge now
GET    /api/info                      public → {version, public_url, base_path, admin_enabled, caps}

GET    /api/admin/config              a-admin → {keys:[{name,value,source,live},…]} (secrets masked)
PATCH  /api/admin/config              a-admin → body {changes:{key:value,…}} → the fresh dump
GET    /api/admin/sessions            a-admin → {sessions:[snapshot + {label, addresses},…]}
GET    /api/admin/events              a-admin → SSE {sessions:[…]} (event: sessions)
DELETE /api/admin/sessions/{sid}[?now] a-admin → warn (TERMINATING), or with ?now terminate at once
POST   /api/admin/sessions/{sid}/cancel-termination  a-admin → back to OPEN
POST   /api/admin/sessions/{sid}/review  a-admin → body {decision:"accept"|"reject", note?}
DELETE /api/admin/sessions/{sid}/clients/{cid}  a-admin → evict a client
GET    /api/admin/sessions/{sid}/download?beam=<bid>&as=…  a-admin → bytes (no activity marked)

GET    /                              home: the landing — create a session, or join by id
GET    /{sid}                         session dashboard (ADR 0020; 404 if {sid} is mis-shaped)
GET    /s/{sid}                       scan page (token arrives in #t=, the dashboard's client id in &c=)
GET    /admin                         admin console (sign in with admin_token)
GET    /docs                          docs walkthrough (static; its screenshots under /docs/*.webp)
GET    /legal                         MIT licence, terms of use, privacy notes (static)
```

`GET /api/info` is unauthenticated (the pages call it before any session
exists) and never logged; `caps` carries `{max_gz_bytes, idle_ttl,
max_age, sessions}` (the `*_ttl`/`max_age` in seconds).

## Client address and X-Forwarded-For

A client is identified by its address. It is the direct peer, unless the peer
is in `trusted_proxies` (default loopback), in which case `X-Forwarded-For` is
read right to left, skipping further trusted hops, and the first untrusted
address — the client the trusted proxy appended — is used. The leftmost entry
is client-spoofable and is never trusted on its own.

## Auth

A session is multi-user (ADR 0017). Every `/api/sessions/{sid}…` call carries
the session token in the `Authorization: Bearer <token>` header; tokens are
128-bit random, base64url (22 characters), minted with the session, and never
logged. A session id is a human `xxx-xxx-xxx` (three lowercase triples, ADR 0020),
and each session lives at its own path `<base>/<sid>`. The token, when in the link,
rides in the **fragment** (`…/<sid>#t=<token>`) so it never reaches server logs; a
**password** session's link is the id alone (`…/<sid>`) and the token is obtained
by entering the password.

Beyond the token, most calls also carry a **client id** in the
`X-Airlift-Client` header. A client is one participant — one device
(`POST …/clients`, or minted by create/join). Registering mints a new client
unless the request's `X-Airlift-Client` names one of the session's clients
bound to the caller's address, which is then returned (ADR 0022): a reload keeps
its identity, a second device behind the same NAT is a second participant, and a
resume from elsewhere is refused. The id is rechecked against the caller's
address on every call, so it is not a secret. There are four tiers:

- **public** — no auth: create, join, knock + poll (ADR 0021), `/api/info`, pages.
- **token** — a valid token, no client needed: register a client.
- **client** — token + a registered, non-evicted client whose id matches the
  caller's address: snapshot, events, frames, ping, extension, download.
- **s-admin** (session admin) — a client that is a session admin: delete the
  session, evict a client, set the password, remove a beam, extend the max_age cap.
- **a-admin** (airlift admin) — the operator, holding the tower's `admin_token`
  (ADR 0014). A fifth, orthogonal tier: `/api/admin/*` checks only the token,
  ignores `X-Airlift-Client`, and never touches the four session tiers. The token
  is the `Authorization: Bearer` on every admin call and on the admin SSE (there
  is no cookie); it is compared in constant time, masked in the config dump, and
  never logged. An unconfigured `admin_token` makes the whole subtree a `404` (the
  surface is invisible); a wrong token is charged against `rate_admin` — a `429`
  when the bucket is empty, else a `401` — while a valid token is never throttled.

A client's roles are the union of its open streams' roles (`?role=relay` on the
event stream marks a scanner). The creator is the first session admin; password/
token joiners are admins iff `joiners_admin` was set.

There is no query-string fallback, so browsers use `fetch` throughout: a
streaming `fetch` with a small SSE parser instead of `EventSource`, and
`fetch` → blob → object URL instead of a bare download link.

Unknown `sid` → `404`. Missing or wrong token → `401`. A valid token with no or
an unknown client → `401`; a client id from a different address, a non-admin on
a session-admin route, or an evicted address → `403` (an evicted address gets
`{"error":"evicted"}`). A `409` means the action is not allowed in the session's
current status: frames, ping, patch, beam-removal and `…/max-age` need a *live*
session (`OPEN` or `TERMINATING`), so they `409` once it is `TERMINATED`/
`PENDING_REVIEW`/`REJECTED`; a session suspended by inactivity also `409`s
downloads until reopened (ADR 0018); an admin cancel/review/extension `409`s out
of its expected state.
Session expiry is no longer refreshed by every call — see Lifecycle.

## Limits

| Limit | Value | Response |
| --- | ---: | --- |
| Request body | `max_body` (default 8 MiB) | `413` |
| Frames per `POST /frames` | 500 | `413` |
| Frame string | 4096 characters | counted as `bad` |
| Beams per place | `max_beams` (default 10) | over-cap MANIFEST auto-evicts the oldest terminal beam, else counted as `bad` |
| Held pre-manifest senders | 8 (65 536 frames) | further held frames counted as `bad` |
| Concurrent sessions | `sessions` (default 32) | `429` + `Retry-After` on create |
| Create / join / frames / ping rate | `rate_create` / `rate_join` / `rate_frames` / `rate_ping` (per address; join also per session) | `429` + `Retry-After` |
| Extension-request rate | `rate_extension` (per address and per session) | `429` + `Retry-After` |
| Wrong admin-token rate | `rate_admin` (per address, charged only on a failed compare) | `429` + `Retry-After` |
| Malformed JSON body | — | `400` |
| Create option over its cap | — | `400` naming the cap |

Rate budgets come from the config `rate_*` keys (a zero budget disables that
limit); the frames rate check runs before the body is read.

## The place model

A session is a *place*: a named join field holding a list of beams (ADR 0015).
Each beam is one payload, identified by its sender-session u32 from the frame
header (`bid` = eight hex digits). A beam is born the instant its MANIFEST
arrives and runs its own state machine; beams in one place decode, verify and
complete independently. There is no place-level transfer state — an empty place
simply has no beams yet.

```
(no beam)   ─MANIFEST→   RECEIVING → VERIFYING → READY
                                              ↘ FAILED
```

## Create

`POST /api/sessions` takes an optional body `{label, password, joiners_admin,
max_gz_bytes, idle_ttl}` (durations in seconds); each limit is clamped to its cap,
and a value above a cap is a `400` naming it. It registers the caller as the first
session admin and returns `201 {sid, token, client_id, name, join_url,
expires_at}`. `join_url` is `<public base>/<sid>#t=<token>` for a public session,
or `<public base>/<sid>` (id only) when a join password is set — the password
session's token never travels in the link (ADR 0020). In serve mode the tower also
prints it, with a terminal QR code, to stdout. `idle_ttl` sets the idle grace (see
Lifecycle).

When a password is set the session is also joinable without a token: `POST
/api/sessions/{sid}/join {password, name}` → `{token, client_id, name}` (a `404`
when no password is set). `PATCH /api/sessions/{sid} {password}` sets or (with
`""`) clears it. Passwords are salted SHA-256 in memory, never stored in
plaintext or logged.

## Place snapshot

Returned by `GET /api/sessions/{sid}` and pushed as each SSE event.

| Field | Type | Notes |
| --- | --- | --- |
| `sid` | string | tower session id |
| `status` | string | `OPEN`, `TERMINATING`, `TERMINATED`, `PENDING_REVIEW` or `REJECTED` (see Lifecycle) |
| `relays` | int | open event streams that declared `role=relay` |
| `beams` | object[] | the beams read into the place, in arrival order |
| `clients` | object[] | the registered clients: `{client_id, name, roles, session_admin, connected, last_active}` |
| `terminated` | object or null | `{by, reason, at, cleanup_at}` once non-live (`TERMINATED`/`PENDING_REVIEW`/`REJECTED`); null while OPEN/TERMINATING |
| `terminate_at` | RFC 3339 or null | the warning deadline; set only while `TERMINATING` |
| `extension` | object or null | `{by, reason, at, decision?, note?, decided_at?}` once a client has requested more time; `by` is a client name |
| `expires_at` | RFC 3339 | the earliest applicable deadline (see Lifecycle): the OPEN clock, the `TERMINATING` warning, the `PENDING_REVIEW` review deadline, or the cleanup time |
| `reopenable` | bool | true when the session was suspended by inactivity and opening its link would revive it (ADR 0018); while true, access is revoked (downloads `409`) |
| `has_password` | bool | a join password is set, so the share link is the id alone (no token) and joiners enter the password (ADR 0020) |
| `knocks` | object[] | pending admission requests `{id, name, at}` (no address), oldest first — a session admin admits/denies each (ADR 0021) |

Each entry of `beams` is:

| Field | Type | Notes |
| --- | --- | --- |
| `bid` | string | eight hex digits of `sender_session`; the beam's id in URLs and downloads |
| `sender_session` | u32 | the sender u32 from the frame header |
| `name` | string | from the manifest |
| `state` | string | `RECEIVING`, `VERIFYING`, `READY` or `FAILED` |
| `total` | int | `N` chunks |
| `have` | int | distinct chunks received |
| `bitmap` | string | `⌈N/8⌉` bytes, standard base64; bit `i` is chunk `i`, most significant bit first |
| `fps` | number | frames accepted for this beam in the last 2 s, divided by 2 |
| `verdicts` | object | `{gz_sha, orig_sha, bundle}`; each `null` until its stage ran, then `{ok, expected, actual}` |
| `bundle` | object or null | `{files, total_bytes, paths}` for a verified repobundle; `paths` holds the first 50 |
| `downloads` | string[] | subset of `raw`, `file`, `zip`; empty unless `READY` |
| `saved_path` | string or null | the beam's directory under `data_dir` once written (ADR 0016); null before it verifies, for a FAILED beam, or when the on-disk write failed and it is served from memory |
| `error` | string or null | the failure reason in `FAILED` |
| `started_at` | RFC 3339 or null | when the beam's MANIFEST arrived |
| `finished_at` | RFC 3339 or null | when the beam's verification ended, either way |

`expected` and `actual` are hex digests for `gz_sha` and `orig_sha`. For
`bundle` they are prose: `"7 files, each matching its sha256"` against
`"7 files verified"` or `"1 failed: notes/NOTES.txt"`.

## Frames ingest

`POST /api/sessions/{sid}/frames` with `{"frames": ["<base45>", …]}` →
`200 {accepted, dup, bad, completed_beams}`. `completed_beams` lists the `bid`s
whose beam received its last chunk during this POST (`[]` otherwise), so a relay
keeps feeding the place after any one beam fills.

- `accepted`: new frames, including DATA/FOUNTAIN frames held before their beam's
  manifest.
- `dup`: frames already held or already decoded, a re-inserted schedule MANIFEST
  for a known beam, and every frame for a beam that has left `RECEIVING`.
- `bad`: undecodable, failing CRC, the wrong length for their position,
  out-of-range `seq`, a MANIFEST whose `total` disagrees with its payload, or a
  MANIFEST that would exceed the per-place beam cap.

FOUNTAIN packets are accepted like DATA chunks; a beam's `have` (in the
snapshot) counts recovered chunks either way, so it can rise by several per
packet.

Multiple scanners may post concurrently, and one place may hold several beams
(cap `max_beams`, default 10). A differing sender is a different beam, never an
error. DATA/FOUNTAIN frames that arrive before their own MANIFEST are held —
up to 8 pending senders, 65 536 frames across them — and adopted when that
sender's manifest arrives, without disturbing any other beam.

## Lifecycle

A session has a `status` of `OPEN`, `TERMINATING`, `TERMINATED`,
`PENDING_REVIEW` or `REJECTED` (ADR 0013, ADR 0014). *Live* means `OPEN` or
`TERMINATING` — the transfer runs, and the concurrency cap counts these.
**Presence keeps a connected session alive** (ADR 0019): while any client stream
is open, `expires_at` is only the `max_age` cap (unset ⇒ no expiry). Two clocks
apply otherwise:

- **idle** — `idle_ttl` after the last client stream leaves (runs only while no
  stream is connected; `reopenable` when it fires, ADR 0018).
- **max_age** — `max_age` from the creation base, when set (a reopen rebases it; a
  session admin pushes it out an hour at a time via `POST …/max-age`).

`inactive_ttl` is gone (ADR 0019). *Activity* — a frames POST that accepted or
duplicated ≥ 1 frame, a download, or a ping — moves the last-activity used for the
idle base; a bare snapshot, an open stream and a register/join are presence.

Terminating:

- A session admin's `DELETE` or any clock firing moves `→ TERMINATED` at once.
- An airlift admin's `DELETE …` starts a **warning**: `OPEN → TERMINATING` with
  `terminate_at = now + warning_ttl`, the transfer staying live so a **cancel**
  (`POST …/cancel-termination`) returns it to `OPEN`. The warning elapsing, a
  session-admin `DELETE`, or `DELETE …?now` moves it `→ TERMINATED`.

A non-live session freezes the transfer (frames, ping, patch and beam-removal
return `409`) but keeps its files and the snapshot. A session terminated
**deliberately** (a session or airlift admin) or by the `max_age` cap keeps
serving READY downloads through its terminated window; a session **suspended by
inactivity** (ADR 0018) revokes all access — downloads `409` too — until reopened.

Extension and review (ADR 0014):

- Any registered client may `POST …/extension {reason?}` once on a `TERMINATED`
  session, moving it `→ PENDING_REVIEW` and stopping the cleanup clock in favour
  of a review clock (`review_ttl`). `extension.by` is the requester's name.
- An airlift admin `POST …/review {decision,note?}` either **accepts** (`→ OPEN`,
  every clock restarted, the beams and files intact) or **rejects** (`→ REJECTED`
  with the note). A `review_ttl` elapsing counts as a rejection.

Reopen by link and max-age grants (ADR 0018):

- A session suspended by the idle grace (a `system` terminate for `idle_ttl`;
  `reopenable: true`) is revived — `→ OPEN`, every clock reset, beams and files
  intact — simply by registering a client or password-joining, so opening the link
  reopens it with no admin review. The deliberate terminations and the `max_age`
  cap above are **not** reopenable this way; they keep the extension → review path.
- A session admin may `POST …/max-age` to add an hour to the `max_age` cap
  (repeatable), so an actively-used session can outlive the 24 h cap without the
  operator; a reopen clears the granted hours.

`terminated_ttl` after a session reaches `TERMINATED` or `REJECTED`, it and its
`<data_dir>/<sid>` directory are deleted (`event: closed`). `PENDING_REVIEW` is
never swept-to-delete while it awaits a decision. A session admin's `DELETE …?hard`
does that deletion at once (ADR 0019), rather than waiting for the sweep.

The web pages: the dashboard is the shared session (ADR 0019) — the join
link/QR opens it, any client watches/downloads, and a **Scan a beam** button opens
the scanner on demand (which stops itself once the beam is received). It shows the
expiry countdown and the session-admin **+1 h** button only in the last 30 min
before the `max_age` cap (presence keeps it alive otherwise), a **Reopen** button
for an idle-suspended session, and **End session** / **Delete now** for admins; the
admin console drives the warning, cancel and review.

## Events

`GET /api/sessions/{sid}/events` → `text/event-stream`: a snapshot on connect,
one per change (coalesced), and a `: keepalive` comment every 15 s. Each push is
named on the entering edge of a status change so a page can react — `terminating`,
`terminated`, `rejected`, and `reopened` on a return to `OPEN` (a cancel or an
accepted extension) — and `state` otherwise (entering `PENDING_REVIEW` included).
`event: closed` fires when the session is finally deleted, and `event: evicted`
when the viewer's address has been evicted (the stream then ends). The snapshot
`status` is authoritative; the event name is only a hint, and clients re-render on
any name. `?role=relay` counts the connection under `relays` and adds `relay` to
the client's roles.

```
event: state
data: {"sid":"…","relays":0,"beams":[…],…}
```

## Admin surface

`/api/admin/*` (ADR 0014) is gated by the tower's `admin_token` (see Auth). It is
`404` when `admin_token` is unset, so an unconfigured tower's admin surface is
invisible. `GET /api/admin/sessions` returns every session as its snapshot plus
`label` and `addresses` (a `{client_id: address}` map — the operator sees
addresses, which the session snapshot deliberately omits); `GET /api/admin/events`
streams the same list (`event: sessions`), pushing only when it changes. The
mutation routes drive the lifecycle above: warn/terminate-now
(`DELETE …/{sid}[?now]`), `cancel-termination`, `review`, client `evict`, and a
`download` that serves a READY beam in any non-deleted status without marking
activity.

### Runtime config

`GET /api/admin/config` → `{keys:[{name, value, source, live}, …]}`, secrets
masked (`****`). `source` is the winning layer (`flag` > `env` > `overrides` >
`file` > `default`); `live` is whether the key is PATCH-able at runtime.

`PATCH /api/admin/config {changes:{key:value,…}}` merges live-key changes into the
tower's `overrides` file (atomic, mode 0600) and applies them without a restart,
returning the fresh dump. Restart-only keys (`public_url`, `listen`, `admin_token`,
`data_dir`, `trusted_proxies`) and unknown keys are rejected with `400`, and an
invalid value with `400`. The cap, the rate budgets, `max_body` and the
warning/review windows take effect on the next request; the per-session clocks and
beam ceilings bind **new** sessions (existing sessions keep what they were created
with). A key pinned by a higher-precedence flag or env var keeps that value and
source after a `PATCH` — the dump shows it, so a pinned key is visibly pinned
rather than silently ignored. The change survives a restart via the overrides file
(which lives under the tower's home, never `data_dir`).

## Downloads

`GET /api/sessions/{sid}/download?beam=<bid>&as=…`. `beam` is the eight-hex-digit
bid; a missing or malformed one is `400`, an unknown one `404`. The beam must be
`READY`, otherwise `409`, as is an `as` not listed in that beam's `downloads`.
Per ADR 0006:

- `raw` — the byte-identical input, named after the manifest (reduced to a
  safe base name). Always available.
- `file` — the bare file, named after its entry, when the bundle has exactly
  one file.
- `zip` — the unpacked tree, named `<stem>.zip`, when the bundle has more
  than one file. Entry paths and modes are preserved; timestamps are fixed.

Once a beam is written to disk (ADR 0016) its downloads stream from the backing
file via `http.ServeContent`, so responses carry `Content-Length` and honour
`Range`; a persist failure serves the same bytes from memory. Either way they
carry `Content-Disposition: attachment`, the content type, and
`Cache-Control: no-store`, and the token stays in the header.

## On disk

On READY a beam's verified output is written under `<data_dir>/<sid>/<bid>/`
(ADR 0016):

```
<sid>/session.json   the session receipt: sid, label, status, terminated,
                     created/started/finished, senders, clients, per-beam refs,
                     the lifecycle event log (ADR 0013; no token/password/address)
<sid>/<bid>/         one directory per beam (ADR 0016):
    raw/<name>       the byte-identical input (always)
    tree/…           the unpacked repobundle tree, modes preserved (a bundle)
    <stem>.zip       the zip of the tree (a bundle of more than one file)
    meta.json        sid, bid, sender_session, name, state, sizes, hashes,
                     verdicts, bundle summary, downloads, started_at, finished_at
```

`session.json` is (re)written on each beam READY and on every lifecycle
transition, atomically (temp + rename). The per-beam files stay as ADR 0016.

The three download kinds are served from these files and the in-memory copies
freed; `saved_path` is the beam directory. Writes are staged in a sibling temp
directory and published with a single rename, so a half-written beam is never
visible. A persist failure keeps the beam READY and serves from memory with
`saved_path` null; a FAILED beam writes nothing. `data_dir` is emptied on start
behind a guard, every entry passes the one path sanitiser, and a session's
directory is removed when the session is deleted or swept. (Persistence does not
survive a tower restart — a non-goal.)

## Static

`GET /` serves the tower entry (the landing; the same page is the session
dashboard at `GET /{sid}`), `GET /s/{sid}` the scan entry, `GET /admin` the
admin console, `GET /docs` the docs page (its screenshots under `/docs/…`), and
`/assets/…` the Vite build output, all from the embedded `web/dist`; until the
UI is built they are placeholders. Every HTML page is served with a `<base href>`
carrying the configured path prefix injected into its head — which is why
in-page links are written `page#fragment`, never a bare `#fragment`. `/sw.js`,
`/manifest.webmanifest` and `/icons/…` make the scan page installable and
offline-first on the phone; the service worker never touches `/api/`. A session
lives at `/{sid}` with its token in the fragment (`#t=…`, ADR 0020); the
dashboard keeps what it needs per session in `sessionStorage` across reloads.

## Replay (internal)

`internal/replay` feeds a frames dump into a session as a scanner would — loop
schedule, batched POSTs, configurable loss and reordering — over either a
private loopback tower or a running one. It is not a user command; it is the
camera-free dev loop and the end-to-end tests (bundle → beam → replay → READY).
