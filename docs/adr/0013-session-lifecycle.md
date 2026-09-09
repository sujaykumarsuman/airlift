# ADR 0013 — Session lifecycle: three clocks, soft termination, session.json

Status: accepted (Phase 6.6, prompt 002)

This number was reserved by prompt 002 ahead of ADR 0015–0017 (which took the
next free numbers as 6.2–6.5 shipped); it now covers the lifecycle half of
prompt-002 points 9–11. It supersedes the single inactive-only TTL of ADR 0005
and ADR 0017. The airlift-admin terminator, the extension/review flow and the
admin surface are Phase 7 (ADR 0014); this ADR reserves the seams for them.

## Context

Through Phase 6.5 a session had one expiry knob — `expiresAt`, refreshed to
`now + inactive_ttl` by `auth.Touch` on **every** authenticated call, with
`Store.Sweep` deleting a session the moment it passed. Prompt 002 asks for a
richer lifecycle: three independent clocks, a session that is *terminated* (its
files kept for a window) rather than deleted outright, an activity model that
distinguishes real work from mere presence, and a session-level receipt on disk.

## Decision

**Three clocks, earliest wins.** While a session is `OPEN`, its `expires_at` is
the earliest applicable of:

- **idle** — `idle_ttl` after the last client stream leaves. It runs only while
  no stream is connected, from the later of the last departure and the last
  activity (the *folded idle base*), so a stream-less but actively-fed relay is
  not reaped mid-transfer.
- **inactive** — `inactive_ttl` after the last activity, while streams are
  connected.
- **max_age** — `max_age` from creation, always, when set.

A zero limit disables that clock; with none set the session never expires. The
per-session limits are clamped at creation (ADR 0017); the per-session `max_age`
override is airlift-admin only, so it waits for Phase 7 — a 6.6 session inherits
the global `max_age`.

**Activity vs presence.** *Activity* is a frames POST that accepted or duplicated
at least one frame, a download, or a ping (`POST /api/sessions/{sid}/ping`,
`rate_ping`, client tier). A bare GET, an open event stream, a register/join and
an all-bad frames POST are **presence**, not activity: they do not move the
inactive clock. `auth.Touch`'s blanket refresh is gone; only the activity points
call `MarkActivity`. A reconnecting stream is presence, so it does not reset the
inactive clock — the resulting "reconnect cliff" only fires when total inactivity
already exceeded `inactive_ttl`, which is correct.

**Soft termination.** A session has a `status`: `OPEN` or `TERMINATED` (Phase 7
adds the reserved `TERMINATING`/`PENDING_REVIEW`/`REJECTED`). A session admin's
`DELETE /api/sessions/{sid}` and any clock firing move `OPEN → TERMINATED` at
once, recording `terminated {by, reason, at, cleanup_at}` (`by` is `"session
admin"` or `"system"`; `"airlift admin"` joins in Phase 7). Terminating **freezes
the transfer** (frames/ping/patch/beam-removal return `409`; a second terminate
is `409`) but **keeps the files** and keeps serving READY-beam downloads and the
snapshot. It does **not** set the `closed` flag — only the final cleanup delete
does — so a beam finishing in the terminated window still persists and ADR 0016's
finalize orphan-reclaim reasoning holds.

**Two-phase sweep.** `Store.Sweep` now (1) terminates an `OPEN` session past its
deadline (`by:"system"`, reason the clock that fired) and (2) deletes a
`TERMINATED` session past its `cleanup_at = terminated_at + terminated_ttl`,
reclaiming `<data_dir>/<sid>` through the ADR-0016 evict hook. Every transition is
an SSE `event: terminated` (once per stream, carrying the snapshot) and a
`session.json` entry.

**session.json.** A session-level receipt is written under `<data_dir>/<sid>/`
on a beam reaching READY and on every lifecycle transition: `sid`, `label`,
`status`, `terminated`, `created/started/finished`, the sender u32s, the clients
seen, per-beam references to their `meta.json` (ADR 0016), and the lifecycle
event log. It is written with the same atomic temp+rename discipline as the
per-beam files, serialised by a mutex, and carries **no** token, password, salt,
hash or client address. A failed write is a logged no-op — the in-memory
snapshot remains the source of truth (ADR 0005/0015).

## Consequences

- `expires_at` now moves with activity and presence rather than every call; the
  web reads `status`/`terminated` from the snapshot (the terminated page, the
  countdown and the visibility/input-gated ping emission are 6.7).
- The deferred half (Phase 7, ADR 0014) slots in without rework: the `Status`
  type reserves the three extra states; `terminated.by` is a free string;
  `deadlineLocked` is reused by a reopen; the two-phase sweep has clear insertion
  points for `PENDING_REVIEW` (skip the OPEN deadline) and `REJECTED`; the
  `session.json` event log is an open append list. `warning_ttl`, `review_ttl`,
  `rate_extension` and `rate_admin` stay typed-but-unconsumed config, not dead
  code.
- A headless `--session` session (no creator client, ADR 0017) is still not
  API-terminable; a clock or the process ending reclaims it.
