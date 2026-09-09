# HTTP API

Canonical. The tower serves plain HTTP under a path prefix behind a
TLS-terminating reverse proxy (ADR 0007, ADR 0012); the proxy strips the prefix
and the router stays rooted. `internal/server` implements it; `internal/replay`
and the server tests are its reference clients.

## Routes

The middle column is the auth tier (see Auth): *public*, *token* (a valid token,
no client yet), *client* (token + a registered client), *admin* (a session-admin
client).

```
POST   /api/sessions                  public → body {label?,password?,joiners_admin?,
                                               max_gz_bytes?,idle_ttl?,inactive_ttl?}
                                             → {sid, token, client_id, name, join_url, expires_at}
POST   /api/sessions/{sid}/join       public → body {password, name?} → {token, client_id, name}
POST   /api/sessions/{sid}/clients    token  → body {name?, role?} → {client_id, name, session_admin, roles}
GET    /api/sessions/{sid}            client → place snapshot
GET    /api/sessions/{sid}/events     client → SSE place snapshots
POST   /api/sessions/{sid}/frames     client → body {frames:[base45,...]}
                                             → {accepted, dup, bad, completed_beams}
GET    /api/sessions/{sid}/download?beam=<bid>&as=raw|file|zip  client → bytes
PATCH  /api/sessions/{sid}            admin  → body {password} (set or, with "", clear)
DELETE /api/sessions/{sid}/clients/{cid}  admin  → evict a client's address
DELETE /api/sessions/{sid}/beams/{bid}    admin  → remove a beam and its files
DELETE /api/sessions/{sid}            admin  → delete the session
GET    /api/info                      public → {version, public_url, base_path, admin_enabled, caps}
GET    /                              tower dashboard
GET    /s/{sid}                       scan page (token arrives in #t=)
```

`GET /api/info` is unauthenticated (the pages call it before any session
exists) and never logged; `caps` carries `{max_gz_bytes, idle_ttl,
inactive_ttl, max_age, sessions}` (the `*_ttl`/`max_age` in seconds).

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
logged. The join URL places the token in the fragment (`/s/{sid}#t=<token>`) so
it never reaches server logs; the scan page reads `location.hash`.

Beyond the token, most calls also carry a **client id** in the
`X-Airlift-Client` header. A client is one participant, registered once per
address (`POST …/clients`, or minted by create/join). The id is rechecked
against the caller's address on every call, so it is not a secret. There are
four tiers:

- **public** — no auth: create, join, `/api/info`, static pages.
- **token** — a valid token, no client needed: register a client.
- **client** — token + a registered, non-evicted client whose id matches the
  caller's address: snapshot, events, frames, download.
- **admin** — a client that is a session admin: delete the session, evict a
  client, set the password, remove a beam.

A client's roles are the union of its open streams' roles (`?role=relay` on the
event stream marks a scanner). The creator is the first session admin; password/
token joiners are admins iff `joiners_admin` was set.

There is no query-string fallback, so browsers use `fetch` throughout: a
streaming `fetch` with a small SSE parser instead of `EventSource`, and
`fetch` → blob → object URL instead of a bare download link.

Unknown `sid` → `404`. Missing or wrong token → `401`. A valid token with no or
an unknown client → `401`; a client id from a different address, a non-admin on
an admin route, or an evicted address → `403` (an evicted address gets
`{"error":"evicted"}`). Every authenticated call refreshes the session's TTL.

## Limits

| Limit | Value | Response |
| --- | ---: | --- |
| Request body | `max_body` (default 8 MiB) | `413` |
| Frames per `POST /frames` | 500 | `413` |
| Frame string | 4096 characters | counted as `bad` |
| Beams per place | `max_beams` (default 10) | over-cap MANIFEST auto-evicts the oldest terminal beam, else counted as `bad` |
| Held pre-manifest senders | 8 (65 536 frames) | further held frames counted as `bad` |
| Concurrent sessions | `sessions` (default 32) | `429` + `Retry-After` on create |
| Create / join / frames rate | `rate_create` / `rate_join` / `rate_frames` (per address; join also per session) | `429` + `Retry-After` |
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
max_gz_bytes, idle_ttl, inactive_ttl}` (durations in seconds); each limit is
clamped to its cap, and a value above a cap is a `400` naming it. It registers
the caller as the first session admin and returns `201 {sid, token, client_id,
name, join_url, expires_at}`. `join_url` is `<public base>/s/{sid}#t={token}`;
in serve mode the tower also prints it, with a terminal QR code, to stdout.
`idle_ttl`/`inactive_ttl` are stored for the lifecycle clocks (a later phase);
6.5 does not enforce them.

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
| `relays` | int | open event streams that declared `role=relay` |
| `beams` | object[] | the beams read into the place, in arrival order |
| `clients` | object[] | the registered clients: `{client_id, name, roles, session_admin, connected, last_active}` |
| `expires_at` | RFC 3339 | refreshed on every authenticated call |

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

## Events

`GET /api/sessions/{sid}/events` → `text/event-stream`: a snapshot on
connect, one per change (coalesced), a `: keepalive` comment every 15 s,
`event: closed` when the session is deleted or expires, and `event: evicted`
when the viewer's address has been evicted (the stream then ends). `?role=relay`
counts the connection under `relays` and adds `relay` to the client's roles.

```
event: state
data: {"sid":"…","relays":0,"beams":[…],…}
```

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
raw/<name>        the byte-identical input (always)
tree/…            the unpacked repobundle tree, modes preserved (a bundle)
<stem>.zip        the zip of the tree (a bundle of more than one file)
meta.json         sid, bid, sender_session, name, state, sizes, hashes,
                  verdicts, bundle summary, downloads, started_at, finished_at
```

The three download kinds are served from these files and the in-memory copies
freed; `saved_path` is the beam directory. Writes are staged in a sibling temp
directory and published with a single rename, so a half-written beam is never
visible. A persist failure keeps the beam READY and serves from memory with
`saved_path` null; a FAILED beam writes nothing. `data_dir` is emptied on start
behind a guard, every entry passes the one path sanitiser, and a session's
directory is removed when the session is deleted or swept. (Persistence does not
survive a tower restart — a non-goal.)

## Static

`GET /` serves the dashboard entry, `GET /s/{sid}` the scan entry, and
`/assets/…` the Vite build output, all from the embedded `web/dist`; until
the UI is built they are placeholders. The two HTML pages are served with a
`<base href>` carrying the configured path prefix injected into their head.
`/sw.js`, `/manifest.webmanifest` and `/icons/…` make the scan page installable
and offline-first on the phone; the service worker never touches `/api/`. The
dashboard also accepts `/#s={sid}&t={token}` so a second device can watch an
existing session; it keeps its own session in `sessionStorage` across reloads.

## Replay (internal)

`internal/replay` feeds a frames dump into a session as a scanner would — loop
schedule, batched POSTs, configurable loss and reordering — over either a
private loopback tower or a running one. It is not a user command; it is the
camera-free dev loop and the end-to-end tests (bundle → beam → replay → READY).
