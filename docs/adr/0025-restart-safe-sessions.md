# ADR 0025 — Restart-safe sessions: state on disk, rehydrated on start

Status: accepted (2026-10-18)

Supersedes ADR 0005 where it makes sessions memory-only ("nothing survives a
tower restart"), and the clauses of ADR 0016 that empty `data_dir` on start and
skip `fsync` because persistence is a non-goal. Builds on ADR 0013/0018 (the
lifecycle clocks), ADR 0016 (the per-beam directory and its `meta.json`), ADR
0022 (resume keys) and ADR 0024 (streamed uploads). Its other half is the drain
a stopping tower runs (docs/API.md "Stopping: the drain").

## Context

The tower runs as one pod with `Recreate` on a read-write-once volume, so every
restart — a deploy, a config or token change, a node reboot, a storage fault —
stops the old process before the new one starts. Until now each restart dropped
every session: sessions lived only in memory (ADR 0005) and the new process
emptied `data_dir` before serving (ADR 0016). A 5 GiB `airlift beam -s` upload cut
at 90 % had to start again in a new session; a READY beam nobody had downloaded
yet was gone; every link stopped working. Deploys were therefore gated by hand on
an idle check.

A rolling update does not help: a second pod on the one node would mount the same
volume, its start-up wipe would delete the first pod's live uploads, and the
sessions would split between two in-memory stores. One tower, one `data_dir`,
stays the rule; what changes is that the tower writes down what it holds and reads
it back.

## Decision

**A state file per session.** `<data_dir>/<sid>/state.json`, mode 0600, written
atomically (a temp file in the same directory, `fsync`, rename). It holds what a
session needs to carry on, and no secret in the clear:

- the token's SHA-256 (never the token), the join password's salt and salted
  SHA-256 (as before, now on disk), and for a password session the token sealed
  with the password (below);
- every client: id, name, session-admin and sender flags, the SHA-256 of its resume
  key (never the key), the address it last spoke from, and its clocks; the
  addresses evicted for the session's life; the pending admission requests;
- the lifecycle: status, termination, warning and review deadlines, the extension
  request, the event log, and every clock base and limit (idle, max_age and its
  grants, terminated_ttl, the beam caps);
- the upload records (ADR 0023/0024) and the beam index: each beam's bid, name,
  state, progress, verdicts, bundle summary, error and timestamps.

It is rewritten after every change to the session, coalesced to at most one write
per session per 250 ms — a scan changes a session several times a second — and
written for every session at the end of a drain. A drained stop therefore loses
nothing; a crash loses at most the last 250 ms of changes. `session.json` (ADR
0013) stays the human-readable receipt; `state.json` is the tower's own.

**Secrets are re-learned, not stored.** A call that presents the token is checked
against its hash in constant time; the first one after a restart also gives the
tower the token back in memory, so the replies that hand it out (create, a
password join, an admitted knock) still can. A resume key is checked and
re-learned the same way. A password session cannot wait for that — its joiners
have only the password — so its token is also kept sealed (AES-256-GCM) under a
key derived from the salt and the password, and a correct password opens it. The
seal adds nothing to what the password already grants.

**Rehydrated on start, before the tower listens.** `data_dir` is no longer emptied
(the guard that refuses a non-empty directory airlift did not create stays). Each
`<sid>/` whose `state.json` reads back is restored into the store; one without a
readable state, or a TERMINATED/REJECTED session past its cleanup time, is deleted;
inside a restored session, anything the state does not account for — a temp
directory, an orphaned beam directory, a stale temp file — is deleted. The sweep
then advances any clock that ran out while the tower was down, as it would have.
Presence starts at zero, so a session's idle grace runs from the restart; an
approved upload's freshness restarts too, since the downtime was the tower's.

**What comes back, and how.**

- A **READY** beam comes back from its `<bid>/meta.json`, which is written before
  the rename that publishes the beam; its downloads are derived from that receipt.
  A beam directory the state missed (a crash inside the coalescing window) is
  adopted the same way. A READY beam that was being served from memory (a persist
  failure, ADR 0016) comes back FAILED: its bytes went with the process.
- A **FAILED** beam comes back as recorded.
- A **beam in frames** still RECEIVING or VERIFYING is dropped: its chunks lived in
  the decoder's memory. The scanner keeps relaying the loop and the next MANIFEST
  starts the beam again; a QR beam is minutes at most.
- A **streamed upload** resumes. After each part the receiver `fsync`s the bytes
  and writes `.<bid>.upload/progress.json`: `received` and the marshalled SHA-256
  state (`crypto/sha256`'s digest is an `encoding.BinaryMarshaler`). The beam comes
  back RECEIVING at that offset, and the first part after the restart re-creates
  the receiver lazily — the file truncated to `received`, the hash resumed from its
  state. The sender's existing resume path (the poll's `received`, a part's `409
  {received}`) needs nothing new. An upload whose bytes were all in is verified at
  start.
- Not kept: event streams (the pages reconnect), download links (15-minute tickets
  held in memory — a download cut by a restart is asked for again), rate-limit
  buckets, and pre-manifest frames held for a beam whose MANIFEST had not arrived.

**The command line rides out a restart.** `airlift beam -s` waits out a `503` with
`Retry-After` and a tower it cannot reach, within `--wait`, on every call it may
repeat, and `--wait` now defaults to 10 minutes (was 3), the same as an upload
approval's own expiry.

## Consequences

- A restart is a pause, not a reset: links, passwords, participants, clocks,
  pending requests, approvals and READY files carry over; dashboards reconnect;
  a streamed upload resumes from where the tower's copy ends.
- The volume now holds secrets-adjacent data for a session's life: the token and
  resume-key hashes (of 128-bit randoms: useless to an attacker), the password's
  salted hash and the sealed token, and participants' addresses (needed for
  eviction and for a keyless resume). All of it is 0600 on the tower's own volume
  and deleted with the session. Someone who takes the volume can brute-force a
  weak session password offline and with it open the sealed token, as they could
  with the hash alone. The legal page says what is kept.
- `state.json` carries `"v": 1`. A tower that cannot read a state file deletes
  that session and logs it, so a format change must bump `v` and migrate or accept
  the loss. An older binary started on a newer `data_dir` empties it, as it always
  did.
- Still one writer: `Recreate` stays, and two towers on one `data_dir` remain
  unsupported.
- The idle check before a deploy (`hack/airlift-idle.sh` in the infra repo) stops
  being the only thing between a deploy and lost work: what a restart still
  interrupts is a QR beam mid-scan and a download in flight. Whether deploys go
  back to being automatic is the operator's call.
- `CLAUDE.md` drops the non-goal "persistence across tower restarts".
