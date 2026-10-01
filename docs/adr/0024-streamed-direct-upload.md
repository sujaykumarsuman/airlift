# ADR 0024 — Streamed direct upload: `airlift beam -s` sends the file as it is, up to `max_upload_bytes`

Status: accepted (2026-10-01)

Amends ADR 0023 (direct send relays frames) and the convention that browsers
download through blobs. Builds on ADR 0016 (the per-beam directory) and ADR
0017 (limits). The beam page and everything a scanner does are unchanged.

## Context

ADR 0023 let a connected machine send a beam straight to a session by relaying
the same frames a page shows, repobundle and all. Frames are the right shape
for a camera and the wrong one for a link: a frame header counts at most
65 535 chunks of at most 2 712 bytes over HTTP (about 177 MB, and
`max_gz_bytes` held it to 64 MiB), base45 and JSON add half again on the wire,
and both ends held the whole payload in memory — the CLI to bundle and encode
it, the tower to decode, gunzip, unpack and zip it. The hosted tower runs in
128 MiB; a beam of a few tens of megabytes could take it down, and with it
every session. The operator wants to move files of up to 5 GB from a connected
machine — and a file sent that way needs none of the air gap's machinery: no
repobundle, no frames, just the file.

## Decision

**Large files are the command line's alone.** A beam carried in frames — a QR
scan, or an older CLI — stays bounded by `max_gz_bytes` (64 MiB). A direct send
is bounded by a new live key, **`max_upload_bytes` (default 5 GiB)**, advertised
in `/api/info` `caps` (0 when the tower has no `data_dir`) and checked when the
upload is requested (`413`).

**A direct send streams the file as it is, from disk to disk.** The CLI hashes
the file where it is and asks leave as before, now declaring the file's size
and sha256. A folder or several files are not bundled: the CLI zips them
(paths, modes and times kept; the same git-aware file list a page bundles) into
one temporary file, `<name>.zip`, and sends that file as it is.

```
POST /api/sessions/{sid}/uploads                       client → {name, bytes, sha256, sender_session}
POST /api/sessions/{sid}/uploads/{uid}/data?offset=N   client → the file's bytes from N
GET  /api/sessions/{sid}/uploads/{uid}                 client → {…, received}   (where to resume)
```

A request carrying `sha256` is *streamed*; one without it is ADR 0023's, which
the tower still takes for an older CLI, under `max_gz_bytes`. The approval flow —
pending, approve/deny, expiry, withdrawal, one open request per sender, the
dashboard's Upload requests — is ADR 0023's unchanged; a streamed approval
admits no frames.

**The tower appends and hashes as the bytes land, and keeps the file as sent.**
Each part must start where the tower's copy ends (`409 {received}` otherwise);
it carries at most `max_body` bytes of the file, sent as it is or
`Content-Encoding: gzip` (the CLI compresses a part when that saves a tenth; the
encoding is transport only). The first part creates the beam, RECEIVING, under
`<data_dir>/<sid>/.<bid>.upload/raw/<name>`; the sha256 is computed
incrementally, so when the last byte lands verification is a comparison, not a
pass over gigabytes. A part cut short keeps what arrived and the sender resumes
from `received`; a dropped connection costs at most one part. A part's body
must keep moving (each read within 60 s, the part within 15 minutes) and no
cleanup ever waits on a part in flight, so a stalled sender holds nothing but
its own upload. Only progress keeps an approval fresh. There is **no
bundle stage**: whatever the file holds — a repobundle included — it is the
result, downloadable as `raw`. On a match the staging directory is renamed to
`<data_dir>/<sid>/<bid>/` (ADR 0016's layout, `raw/` and `meta.json`, which
carries `stream: true` and no gzip blob); on a mismatch, a failed write or a
removal, its files are deleted at once. The beam's progress is counted in bytes
(`stream`, `size`, `received` in its snapshot; `total`/`have` count 1 MiB units
for the minimap).

**The disk is checked, not trusted.** The tower refuses what it cannot hold —
`507` at the request and at the first part — counting free space under
`data_dir`, less what uploads in flight may still write, less a 64 MiB margin. A
full disk mid-upload fails the beam cleanly with `507`.

**Downloads are short-lived links.** A result of gigabytes cannot go through a
`fetch` → blob → object URL: the browser holds it all before saving. The
dashboard now asks `POST /api/sessions/{sid}/download-link {beam, as}` (client
tier, the same checks as a download, counted as activity) for a path
`api/dl/<ticket>` and hands it to the browser's download manager: progress,
`Range` resume, straight to disk. The ticket is 128 random bits, names one
download of one beam for the participant who asked (an evicted participant's
links stop working), lasts 15 minutes and is held only in memory; the session
token still never appears in a URL. The admin console keeps its token-header
download.

## Consequences

- `airlift beam -s` carries files of gigabytes with the tower and the CLI each
  using a few tens of megabytes; a 5 GiB file goes at the link's speed. The
  frames' limits and memory cliff no longer apply to the command line, and the
  QR path keeps its 64 MiB ceiling.
- A directly sent folder arrives as a zip, not an unpacked tree: the dashboard
  offers that zip (the same thing it offers for a scanned folder) and the tower
  stores nothing it did not receive. The per-file sha256 of a repobundle gives
  way to the zip's per-entry CRC under the whole file's sha256.
- The CLI needs a tower of this release or later (one without
  `max_upload_bytes` is named as too old); an older CLI still sends frames to a
  new tower, bounded by `max_gz_bytes`.
- The hosted tower's volume grows to 50 Gi (infra repo), room for several 5 GiB
  uploads beside the QR beams.
- The scanner, the frame protocol, the repobundle, the verification chain for
  frames and the on-disk layout are unchanged. A streamed beam is a beam: it
  lists, verifies, downloads, expires and is removed like any other.
