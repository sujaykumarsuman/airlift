# HTTP API

Canonical. The tower serves this over TLS on the LAN (ADR 0007, ADR 0008).
`internal/server` implements it; `internal/replay` and the server tests are
its reference clients.

## Routes

```
POST   /api/sessions                      → {sid, token, join_url, expires_at}
GET    /api/sessions/{sid}                token → state snapshot
GET    /api/sessions/{sid}/events         token → SSE state snapshots
POST   /api/sessions/{sid}/frames         token → body {frames:[base45,...]}
                                          → {accepted, dup, bad, have, total, state}
GET    /api/sessions/{sid}/download?as=raw|file|zip   token → bytes
DELETE /api/sessions/{sid}                token
GET    /ca.crt                            local CA certificate (PEM)
GET    /                                  tower dashboard
GET    /s/{sid}                           scan page (token arrives in #t=)
```

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
| `dest_path` | string or null | what `--dest` received: the unpacked tree for a bundle, else the raw file |
| `error` | string or null | the failure in `FAILED`; in `READY`, a `--dest` write failure (downloads still work) |
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
  the wrong length for their position, out-of-range `seq`, or FOUNTAIN
  (Phase 4).

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

## `--dest`

The raw file lands at `<dest>/<name>`; a bundle's tree at `<dest>/<stem>/`
(`<name>.tree/` when the name has no extension). Every entry passes the path
sanitiser. Existing files are overwritten.

## Static

`GET /` serves the dashboard entry, `GET /s/{sid}` the scan entry, and
`/assets/…` the Vite build output, all from the embedded `web/dist`; until
the UI is built they are placeholders. The dashboard also accepts
`/#s={sid}&t={token}` so a second device can watch an existing session; it
keeps its own session in `sessionStorage` across reloads. `GET /ca.crt` serves the local CA in
PEM with `Content-Type: application/x-x509-ca-cert` so phones offer to
install it; with `--cert/--key` it is `404`.

## Replay mode

`airlift-tower --replay FILE` serves the same API over a private loopback
listener without TLS, feeds `FILE` into a fresh session as a scanner would,
prints the verdicts, and exits 0 on `READY`.
