# HTTP API

Canonical. The tower serves this over TLS on the LAN (ADR 0007, ADR 0008).

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
`Authorization: Bearer <token>` header. Tokens are 128-bit random, base64url,
minted with the session. The join URL places the token in the URL fragment
(`/s/{sid}#t=<token>`) so it never reaches server logs; the scan page reads
it from `location.hash` and sends it as the header. Tokens are never logged.

Missing or wrong token → `401`. Unknown `sid` → `404`.

## Limits

| Limit | Value | Response |
| --- | ---: | --- |
| Request body | 256 MiB | `413` |
| Frames per `POST /frames` | 500 | `413` |
| Concurrent sessions | 32 | `429` on create |

## State machine

```
WAITING_MANIFEST → RECEIVING → VERIFYING → READY
                                         ↘ FAILED
```

## State snapshot

Returned by `GET /api/sessions/{sid}` and pushed as each SSE event
(`event: state`, `data: <json>`).

| Field | Type | Notes |
| --- | --- | --- |
| `sid` | string | tower session id |
| `state` | string | see state machine |
| `sender_session` | u32 or null | bound on the first MANIFEST |
| `name` | string | from the manifest |
| `total` | int | `N` chunks; `0` before the manifest |
| `have` | int | distinct chunks received |
| `bitmap` | string | `N` bits, bit-packed, base64 |
| `fps` | number | server-side decoded frames per second (accepted, non-dup) |
| `relays` | int | scanners currently connected via SSE |
| `verdicts` | object | `{gz_sha, orig_sha, bundle}`, each `{ok, expected, actual}` |
| `bundle` | object or null | `{files, total_bytes}` when a repobundle was detected |
| `downloads` | string[] | subset of `raw`, `file`, `zip` currently available |
| `dest_path` | string or null | where `--dest` wrote the result |
| `error` | string or null | set in `FAILED` |
| `expires_at` | string | RFC 3339; TTL is refreshed on activity |

## Frames ingest

`POST /api/sessions/{sid}/frames` with `{"frames": ["<base45>", …]}`.
Each string is decoded, parsed and CRC-checked (`PROTOCOL.md`). The response
counts `accepted` (new), `dup` (already held), `bad` (undecodable, bad CRC,
or wrong sender session), and repeats `have`, `total`, `state`. Multiple
scanners may post to the same session concurrently.

## Downloads

Per ADR 0006, after `READY`:

- `as=raw` — the byte-identical input file. Always available.
- `as=file` — the bare file, when the bundle contains exactly one file.
- `as=zip` — a zip of the unpacked tree, when the bundle contains more than
  one file. Entry paths and modes are preserved; every entry passed the
  sanitiser.

Before `READY`, or for an `as` not in `downloads`, the response is `409`.

## Static

`GET /` serves the dashboard entry, `GET /s/{sid}` the scan entry, and
`/assets/…` the Vite build output, all from the embedded `web/dist`.
`GET /ca.crt` serves the local CA in PEM with `Content-Type:
application/x-x509-ca-cert` so phones offer to install it.
