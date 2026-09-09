# HTTP API

Canonical. The tower serves plain HTTP under a path prefix behind a
TLS-terminating reverse proxy (ADR 0007, ADR 0012); the proxy strips the prefix
and the router stays rooted. `internal/server` implements it; `internal/replay`
and the server tests are its reference clients.

## Routes

```
POST   /api/sessions                      → {sid, token, join_url, expires_at}
GET    /api/sessions/{sid}                token → place snapshot
GET    /api/sessions/{sid}/events         token → SSE place snapshots
POST   /api/sessions/{sid}/frames         token → body {frames:[base45,...]}
                                          → {accepted, dup, bad, completed_beams}
GET    /api/sessions/{sid}/download?beam=<bid>&as=raw|file|zip   token → bytes
DELETE /api/sessions/{sid}                token
GET    /api/info                          → {version, public_url, base_path, admin_enabled, caps}
GET    /                                  tower dashboard
GET    /s/{sid}                           scan page (token arrives in #t=)
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

Every `/api/sessions/{sid}…` call carries the session token in the
`Authorization: Bearer <token>` header. Tokens are 128-bit random, base64url
(22 characters), minted with the session; ids are 64-bit random hex. The
join URL places the token in the fragment (`/s/{sid}#t=<token>`) so it never
reaches server logs; the scan page reads `location.hash` and sends the
header. Tokens are never logged.

There is no query-string fallback, so browsers use `fetch` throughout: a
streaming `fetch` with a small SSE parser instead of `EventSource`, and
`fetch` → blob → object URL instead of a bare download link. The web UI
(Phase 3) does exactly that.

Unknown `sid` → `404`. Missing or wrong token → `401`. Every authenticated
call refreshes the session's TTL.

## Limits

| Limit | Value | Response |
| --- | ---: | --- |
| Request body | `max_body` (default 8 MiB) | `413` |
| Frames per `POST /frames` | 500 | `413` |
| Frame string | 4096 characters | counted as `bad` |
| Beams per place | `max_beams` (default 10) | over-cap MANIFEST counted as `bad` |
| Held pre-manifest senders | 8 (65 536 frames) | further held frames counted as `bad` |
| Concurrent sessions | 32 | `429` on create |
| Malformed JSON body | — | `400` |

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

`POST /api/sessions` → `201 {sid, token, join_url, expires_at}`. `join_url`
is `<public base>/s/{sid}#t={token}`. In serve mode the tower also prints it,
with a terminal QR code, to stdout.

## Place snapshot

Returned by `GET /api/sessions/{sid}` and pushed as each SSE event.

| Field | Type | Notes |
| --- | --- | --- |
| `sid` | string | tower session id |
| `relays` | int | open event streams that declared `role=relay` |
| `beams` | object[] | the beams read into the place, in arrival order |
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
connect, one per change (coalesced), a `: keepalive` comment every 15 s, and
`event: closed` when the session is deleted or expires. `?role=relay` counts
the connection under `relays`; the dashboard omits it.

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
