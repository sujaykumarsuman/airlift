# ADR 0024 — Streamed direct upload: `airlift beam -s` sends the file itself, up to `max_upload_bytes`

Status: accepted (2026-10-01)

Amends ADR 0023 (direct send relays frames) and the convention that browsers
download through blobs. Builds on ADR 0016 (the per-beam directory) and ADR
0017 (limits). The beam page and everything a scanner does are unchanged.

## Context

ADR 0023 let a connected machine send a beam straight to a session by relaying
the same frames a page shows. Frames are the right shape for a camera and the
wrong one for a link: a frame header counts at most 65 535 chunks of at most
2 712 bytes over HTTP (about 177 MB, and `max_gz_bytes` held it to 64 MiB),
base45 and JSON add half again on the wire, and both ends held the whole
payload in memory — the CLI to encode it, the tower to decode, gunzip, unpack
and zip it. The hosted tower runs in 128 MiB; a beam of a few tens of
megabytes could take it down, and with it every session. The operator wants to
move files of up to 5 GB from a connected machine, which no setting of the old
path could carry.

## Decision

**A direct send streams the payload's own bytes, from disk to disk.** The CLI
hashes the file where it is (a folder or several files are bundled into a
temporary file first, hashed as they are written) and asks leave as before,
now declaring the payload's size and sha256:

```
POST /api/sessions/{sid}/uploads            client → {name, bytes, sha256, bundle?, sender_session}
POST /api/sessions/{sid}/uploads/{uid}/data?offset=N   client → the payload's bytes from N
GET  /api/sessions/{sid}/uploads/{uid}      client → {…, received}   (where to resume)
```

A request carrying `sha256` is *streamed*; one without it is ADR 0023's, which
the tower still takes (an older CLI keeps working). The approval flow — pending,
approve/deny, expiry, withdrawal, one open request per sender, the dashboard's
Upload requests — is ADR 0023's unchanged; a streamed approval admits no frames.

**The tower appends and hashes as the bytes land.** Each part must start where
the tower's copy ends (`409 {received}` otherwise); it carries at most
`max_body` bytes of the payload, sent as it is or `Content-Encoding: gzip`
(the CLI compresses a part when that saves a tenth). The first part creates the
beam, RECEIVING, under `<data_dir>/<sid>/.<bid>.upload/raw/<name>`; the sha256
is computed incrementally, so when the last byte lands verification is a
comparison, not a pass over gigabytes. A part cut short keeps what arrived and
the sender resumes from `received`; a dropped connection costs one part, not
the upload. The beam's progress is counted in bytes (`stream`, `size`,
`received` in its snapshot; `total`/`have` count 1 MiB units for the minimap).

**Verification and the bundle stage never hold the payload.** A plain file is
READY once its sha256 matches. A repobundle is unpacked from the file by
`bundle.Unpack` — a streaming reader that decides exactly what `Parse` decides,
entry by entry, writing only verified entries into `tree/` — and zipped from the
tree by `bundle.ZipTree`. Then the staging directory is renamed to
`<data_dir>/<sid>/<bid>/` (ADR 0016's layout; `meta.json` gains `stream: true`
and has no gzip blob). There is no in-memory fallback: a write that fails fails
the beam, and a failed or removed beam's files are deleted at once. On the CLI,
`bundle.Pack` now reads each file twice — once to hash and check it, once to
write it — so bundling a folder no longer holds its files in memory either; its
output is byte-for-byte what it was.

**Two limits, by path.** `max_gz_bytes` (64 MiB) stays the ceiling for a beam
carried in frames — a QR scan, or an older CLI. A streamed upload is bounded by
a new live key, **`max_upload_bytes` (default 5 GiB)**, advertised in
`/api/info` `caps` (0 when the tower has no `data_dir`), checked at the request
(`413`). The tower also refuses what its disk cannot hold — `507` at the request
and at the first part — counting free space under `data_dir`, less what uploads
in flight may still write, less a 64 MiB margin; a bundle needs room for itself,
its tree and its zip (three times its size). A full disk mid-upload fails the
beam cleanly with `507`.

**Downloads are short-lived links.** A result of gigabytes cannot go through a
`fetch` → blob → object URL: the browser holds it all before saving. The
dashboard now asks `POST /api/sessions/{sid}/download-link {beam, as}` (client
tier, the same checks as a download, counted as activity) for a path
`api/dl/<ticket>` and hands it to the browser's download manager: progress,
`Range` resume, straight to disk. The ticket is 128 random bits, names one
download of one beam, lasts 15 minutes and is held only in memory; the session
token still never appears in a URL. The admin console keeps its token-header
download.

## Consequences

- `airlift beam -s` carries files of gigabytes with the tower and the CLI each
  using a few tens of megabytes; a 5 GiB file goes at the link's speed. The old
  frames limit and its memory cliff no longer apply to the CLI.
- The CLI needs a tower of this release or later (one without
  `max_upload_bytes` is named as too old); an older CLI still sends frames to a
  new tower, bounded by `max_gz_bytes`.
- The hosted tower's volume grows to 10 Gi (infra repo), so a 5 GiB upload fits
  beside the QR beams; a folder near the limit needs three times its size and is
  refused up front when it would not fit.
- The scanner, the frame protocol, the verification chain for frames and the
  on-disk layout are unchanged. A streamed beam is a beam: it lists, verifies,
  downloads, expires and is removed like any other.
