# HTTP API

Canonical. The tower serves plain HTTP under a path prefix behind a
TLS-terminating reverse proxy (ADR 0007, ADR 0012); the proxy strips the prefix
and the router stays rooted. `internal/server` implements it; `internal/replay`
and the server tests are its reference clients.

## Routes

```
POST   /api/sessions                      → {sid, token, join_url, expires_at}
GET    /api/sessions/{sid}                token → state snapshot
GET    /api/sessions/{sid}/events         token → SSE state snapshots
POST   /api/sessions/{sid}/frames         token → body {frames:[base45,...]}
                                          → {accepted, dup, bad, have, total, state}
GET    /api/sessions/{sid}/download?as=raw|file|zip   token → bytes
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
| Request body | 256 MiB | `413` |
| Frames per `POST /frames` | 500 | `413` |
| Frame string | 4096 characters | counted as `bad` |
| Concurrent sessions | 32 | `429` on create |
| Malformed JSON body | — | `400` |

## State machine

```
WAITING_MANIFEST → RECEIVING → VERIFYING → READY
                                         ↘ FAILED
```

## Create

`POST /api/sessions` → `201 {sid, token, join_url, expires_at}`. `join_url`
is `<public base>/s/{sid}#t={token}`. In serve mode the tower also prints it,
with a terminal QR code, to stdout.

## State snapshot

Returned by `GET /api/sessions/{sid}` and pushed as each SSE event.

| Field | Type | Notes |
| --- | --- | --- |
| `sid` | string | tower session id |
| `state` | string | see state machine |
| `sender_session` | u32 or null | bound by the first MANIFEST |
| `name` | string | from the manifest; `""` before it |
| `total` | int | `N` chunks; `0` before the manifest |
| `have` | int | distinct chunks received |
| `bitmap` | string | `⌈N/8⌉` bytes, standard base64; bit `i` is chunk `i`, most significant bit first; `""` before the manifest |
| `fps` | number | frames accepted in the last 2 s, divided by 2 |
| `relays` | int | open event streams that declared `role=relay` |
| `verdicts` | object | `{gz_sha, orig_sha, bundle}`; each `null` until its stage ran, then `{ok, expected, actual}` |
| `bundle` | object or null | `{files, total_bytes, paths}` for a verified repobundle; `paths` holds the first 50 |
| `downloads` | string[] | subset of `raw`, `file`, `zip`; empty unless `READY` |
| `dest_path` | string or null | on-disk path once written under `data_dir` (per beam, ADR 0013); null until then |
| `error` | string or null | the failure reason in `FAILED` |
| `started_at` | RFC 3339 or null | when the first frame was accepted |
| `finished_at` | RFC 3339 or null | when verification ended, either way |
| `expires_at` | RFC 3339 | refreshed on every authenticated call |

`expected` and `actual` are hex digests for `gz_sha` and `orig_sha`. For
`bundle` they are prose: `"7 files, each matching its sha256"` against
`"7 files verified"` or `"1 failed: notes/NOTES.txt"`.

## Frames ingest

`POST /api/sessions/{sid}/frames` with `{"frames": ["<base45>", …]}` →
`200 {accepted, dup, bad, have, total, state}`.

- `accepted`: new frames, including DATA frames held before the manifest.
- `dup`: frames already held, and every frame once the session has left the
  receiving states.
- `bad`: undecodable, failing CRC, from another sender session once bound,
  the wrong length for their position, or out-of-range `seq`.

FOUNTAIN packets are accepted like DATA chunks; `have` counts recovered
chunks either way, so it can rise by several per packet.

Multiple scanners may post concurrently. The first MANIFEST binds the sender
session; until then DATA frames from up to 8 sender sessions (65 536 frames)
are held and adopted when their manifest arrives.

## Events

`GET /api/sessions/{sid}/events` → `text/event-stream`: a snapshot on
connect, one per change (coalesced), a `: keepalive` comment every 15 s, and
`event: closed` when the session is deleted or expires. `?role=relay` counts
the connection under `relays`; the dashboard omits it.

```
event: state
data: {"sid":"…","state":"RECEIVING",…}
```

## Downloads

`GET /api/sessions/{sid}/download?as=…`, `READY` only; otherwise `409`, as
is an `as` not listed in `downloads`. Per ADR 0006:

- `raw` — the byte-identical input, named after the manifest (reduced to a
  safe base name). Always available.
- `file` — the bare file, named after its entry, when the bundle has exactly
  one file.
- `zip` — the unpacked tree, named `<stem>.zip`, when the bundle has more
  than one file. Entry paths and modes are preserved; timestamps are fixed.

Responses carry `Content-Disposition: attachment` and `Cache-Control: no-store`.

## On disk

Verified output is written under `data_dir` (per beam; layout in ADR 0013).
`data_dir` is emptied on start behind a guard, and every entry passes the path
sanitiser. (Downloads are served from memory until the per-beam write lands.)

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
