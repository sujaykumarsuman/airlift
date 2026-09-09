# ADR 0016 — Per-beam on-disk layout and download serving

Status: accepted (Phase 6.4, prompt 002)

Refines prompt-002 point 9's file-writing for the multi-beam place model (ADR
0015). The keep-versus-delete lifecycle (terminate, review, `terminated_ttl`)
and the session-level `session.json` with clients and lifecycle events remain
with the session/lifecycle model (ADR 0013, phases 6.5/6.6).

## Context

Through Phase 6.3 the tower kept a beam's verified output in memory and served
downloads from those bytes. Prompt-002 point 9 requires the tower to write the
verified output under `data_dir` on READY, serve downloads from those files, and
free the in-memory copies, so a place accumulating several large beams does not
hold every result in RAM and an operator can browse the results on disk.

The multi-beam model changes the unit: a session (a *place*) holds a list of
beams keyed by the sender u32, so files are written **per beam** under the
session, not once per session.

## Decision

On a beam reaching READY the tower writes its verified output to
`<data_dir>/<sid>/<bid>/`:

```
<data_dir>/<sid>/<bid>/
    raw/<name>        the byte-identical input, safe base name (always)
    tree/…            the unpacked repobundle tree, modes preserved (a bundle)
    <stem>.zip        the zip of the tree (a bundle of more than one file)
    meta.json         the per-beam receipt
```

- **`raw/` and `tree/` are separate subdirectories.** Prompt-002 point 9's flat
  `<data_dir>/<sid>/<name>` collides when the manifest name is extensionless: the
  `multi` fixture is named `multi`, so the raw file `multi` and the tree
  directory `multi/` would fight for one path. Isolating the raw file under
  `raw/` and the tree under `tree/` removes the collision for every name.
- **Downloads map to files:** `raw` → `raw/<name>`; `file` (a one-file bundle) →
  the single file inside `tree/`; `zip` (a many-file bundle) → `<stem>.zip`.
  They are served with `http.ServeContent` from the backing file — `Range` and
  `Content-Length` handled, a zero modtime so no `Last-Modified`/304 pairs with
  `Cache-Control: no-store` — keeping `Content-Disposition: attachment`, the
  content type, and the header-only token. The in-memory copies are released on
  the success path.
- **`meta.json`, not `beam.json`.** ADR 0010 forbids a `beam.json` artifact on
  the user's disk (it named the retired frames-dump). `meta.json` records `sid`,
  `bid`, `sender_session`, `name`, `state`, `gz_size`, `orig_size`, `gz_sha256`,
  `orig_sha256`, `verdicts`, the bundle summary, `downloads`, `started_at` and
  `finished_at`, mirroring the beam's snapshot so the on-disk and in-memory
  records agree exactly (finalize stamps one finish instant used by both).
- **Atomic publish.** Everything is staged in a sibling temp directory
  (`os.MkdirTemp` under `<data_dir>/<sid>`) and published with a single
  `os.Rename`. A reader — or an operator browsing `data_dir` — never sees a
  half-written `<bid>/`; a mid-write failure removes the temp directory and
  leaves nothing behind. No `fsync`: persistence across restarts is a non-goal
  and `data_dir` is emptied on start.
- **A persist failure keeps the beam READY, served from memory,** with
  `saved_path` null. A verified transfer — expensive to reproduce, since it
  means re-scanning the QR loop out of the air gap — is never lost to a transient
  disk error. A FAILED beam writes nothing.
- **`saved_path`** in the snapshot is the beam directory once written; null
  before it verifies, for a FAILED beam, or after a persist failure.
- **Path containment** rests on the one sanitiser: `bundle.WriteTree` re-applies
  `SafePath` to every entry and refuses any unverified one, so the tree cannot
  escape `<bid>/tree` even if a bad entry reached the writer.
- **Cleanup.** `data_dir` is emptied on start behind the existing guard. A
  session's directory is also removed when the session is deleted or swept,
  through a single store evict hook (`SetEvictHook`, mirroring `SetCompleteHook`)
  so both the `DELETE` route and the off-server sweep reclaim disk through one
  path.

## Consequences

- A completing beam does one bounded burst of disk I/O off the session lock, in
  its own goroutine; two beams of one place write disjoint `<bid>/` directories,
  sharing only an idempotent `MkdirAll(<sid>)`, so concurrent persistence is
  safe.
- The `session.Download` model gains a `Blob` seam (`MemBlob` before the write, a
  file blob after), so the download handler has one serve path whether a download
  is memory- or disk-backed — which is also what lets the persist-failure
  fallback serve from memory unchanged.
- A `finalize` completing as its session is deleted or swept re-checks after
  publishing and reclaims its own directory: `close()` sets the session's closed
  flag before the evict hook removes the tree, and the mutex orders that write
  ahead of any post-persist `Closed()` read in an orphan-producing interleaving,
  so a persist that lands after the removal always observes the closed session
  and cleans up. The fuller in-flight refusal — a freeze that also stops a beam
  mid-verification — arrives with the lifecycle in 6.6.
- The richer keep-versus-delete policy, the session-level `session.json`, and
  cleanup tied to `TERMINATED`/`REJECTED` build on this per-beam layout in
  phases 6.5/6.6 (ADR 0013).
