# ADR 0014 — Admin surface, extension review, and runtime overrides

Status: accepted (Phase 7, prompt 002)

Completes the half of the session lifecycle ADR 0013 reserved (the
`TERMINATING` / `PENDING_REVIEW` / `REJECTED` states and the airlift-admin
surface) and upholds ADR 0010's two-command binary. Builds on ADR 0013
(lifecycle), ADR 0016 (per-beam disk layout and orphan-reclaim), ADR 0017 (auth
tiers, eviction, rate limits) and ADR 0012 (HTTP behind a proxy).

## Context

ADR 0013 shipped `OPEN`/`TERMINATED` with three clocks and a two-phase sweep,
and deliberately reserved the seams for the rest: the three extra `Status`
values, `terminated.by` as a free string, `deadlineLocked` for reuse by a reopen,
clear sweep insertion points, and `warning_ttl` / `review_ttl` / `rate_extension`
/ `rate_admin` as typed-but-unconsumed config. The config layer already carried
`WriteOverrides` (an atomic, mode-0600, restart-only-rejecting, precedence-
preserving write returning a fresh `Load`), the registry `Live` flag and `KSecret`
masking. Phase 7 activates all of it and adds the operator's console.

## Decision

**The five-state lifecycle.** `Status` is now `OPEN`, `TERMINATING`,
`TERMINATED`, `PENDING_REVIEW` or `REJECTED`. A predicate `Live() = OPEN ||
TERMINATING` gates every transfer path (`Ingest`, `MarkActivity`, the frames,
ping, patch and beam-removal handlers) and the concurrency count, so a warned
session keeps working and a cancel is seamless. Transitions (session-mutex only,
no store callback):

- **warn** — an airlift admin's `DELETE …` starts `StartTermination`: `OPEN →
  TERMINATING`, `terminate_at = now + warning_ttl`, the transfer staying live; a
  `warning_ttl` of zero collapses to an immediate terminate.
- **cancel** — `CancelTermination`: `TERMINATING → OPEN`, clearing `terminate_at`
  and resuming the ordinary clocks from now.
- **terminate now** — `?now`, or the warning elapsing in the sweep, or a session
  admin's `DELETE`: `→ TERMINATED` (files kept, cleanup clock started).
- **extend** — any registered client's `POST …/extension`: `TERMINATED →
  PENDING_REVIEW`, stopping the cleanup clock in favour of a review clock
  (`review_deadline = now + review_ttl`), recording the requester's **name**
  (never an address). Exactly one request per session.
- **review** — an airlift admin's decision: **accept** reopens (`→ OPEN`, the
  termination cleared, every clock restarted from now, the beams and their files
  intact) or **reject** (`→ REJECTED` with the reviewer's note and a fresh
  cleanup clock). A `review_ttl` elapsing in the sweep counts as a rejection.

`warning_ttl` and `review_ttl` are read at the transition (method arguments), so
the session package keeps its zero config dependency and the values apply live.
`max_age` now counts from a rebasable base so a reopen restarts it, while
`created_at` stays truthful. No transition sets the `closed` flag — only the
final cleanup delete does — so ADR 0016's finalize/orphan-reclaim reasoning is
unchanged: an in-flight beam still reaches `READY` and persists under any
non-`OPEN` state, and a session directory is neither orphaned nor double-deleted.
The two-phase sweep is reused verbatim; only its switch arms grow
(`TERMINATING → TERMINATED`, `PENDING_REVIEW → REJECTED`, and cleanup of both
terminal states). `PENDING_REVIEW` is never swept-to-delete (its cleanup clock is
stopped). A **reopen bypasses the concurrency cap** rather than calling back into
the store, preserving the store→session lock order; cap discipline on reopen, if
ever wanted, belongs in the admin handler, not the session method.

The snapshot gains `terminate_at` (set only while `TERMINATING`) and `extension`.
The per-stream SSE names the entering edge — `terminating`, `terminated`,
`rejected`, and `reopened` on a return to `OPEN` — alongside the existing `state`
/ `closed` / `evicted`; entering `PENDING_REVIEW` stays `state` because it needs
no distinct client action. The snapshot `status` is authoritative; the event name
is only a hint, and the client re-renders on any name.

**A fifth, orthogonal auth tier.** `/api/admin/*` is a separate subtree gated by
`admin_token`; it ignores `X-Airlift-Client` and never touches the four session
tiers, and no session route ever checks `admin_token`. One wrapper: a `404`
identical to an unknown route when `admin_token` is unset (the surface is
invisible and unprobeable), a constant-time bearer compare that proceeds on a
match, else a `rate_admin` charge — a `429` when the bucket is empty, else a
`401`. A valid token is never rate-limited. There is no cookie or minted session:
the `admin_token` is the bearer on every call and on the SSE (through the same
streaming `fetch`), verified once by the web via `GET /api/admin/config`. The
token travels only in the header, is compared in constant time, is masked in the
dump, is rejected by `WriteOverrides` as restart-only, is never written to
`session.json` or `meta.json`, and is never logged.

**Admin routes.** `GET /api/admin/config` (the effective config with sources,
secrets masked) and `PATCH` (below); `GET /api/admin/sessions` (the global list —
each row the snapshot plus the label and the per-client addresses, operator-only)
and `GET /api/admin/events` (the same list over SSE, a 1 s coalescing diff with a
15 s keepalive — a single operator on localhost makes a store-wide broadcaster
unnecessary); `DELETE /api/admin/sessions/{sid}[?now]`; `POST …/cancel-
termination`; `POST …/review {decision,note}`; `DELETE …/clients/{cid}`; and
`GET …/download?beam=&as=` (reusing the beam serve without marking activity — an
operator peek must not keep a session alive). The extension request stays a
client-tier session route, `rate_extension`.

**Runtime overrides.** The registry `Live` flag is the authority: `public_url`,
`listen`, `admin_token`, `data_dir` and `trusted_proxies` are restart-only; every
other key is live-editable. `PATCH /api/admin/config {changes}` validates and
writes them through `WriteOverrides` (atomic, mode 0600, restart-only/unknown
rejected as `400`), then `applyLive` hot-swaps a guarded `liveCfg` pointer
(`max_body`, the caps, the warning/review windows), the limiter's rate budgets,
and the store's cap, per-beam limits and clocks — no restart. The cap, rates,
`max_body` and the warning/review windows bite the next request; the per-session
clocks and beam ceilings bind **new** sessions (the store copies them at
creation), so existing sessions keep what they were created with. Precedence
(flag > env > overrides > file > default) is preserved and surfaced, so a key
pinned by a flag or env var visibly keeps its source after a `PATCH`. The change
survives a restart because the overrides file (under Home, not `data_dir`) is
re-read at the overrides layer.

**The web console.** A third Vite entry (`admin.html`) served at `GET /admin`
with `<base href>` injection. Sign in with the admin token (kept in
`sessionStorage`; a `401` clears it, a `404` says the surface is disabled); a live
sessions table with pending reviews first, per-status controls (terminate with a
warning, cancel, terminate now, evict, download), an accept/reject review form,
and the settings page. Per-row countdowns are text-node patched so the coalesced
SSE does not freeze them and no input is rebuilt mid-edit.

**The CLI stays two commands.** Prompt 002 listed `airlift sessions` and `airlift
fetch`; they **fold into `/admin`** — the live table is the session list, the
per-beam Download button is the fetch — and ship as **no new subcommand**. This
upholds ADR 0010 (which already retired five originally-listed subcommands to
internal processes) unamended, and keeps the air-gapped binary's secret surface
free of the `admin_token`. If headless/scriptable access is ever wanted, a single
`airlift admin sessions|fetch` verb under a scoped ADR-0010 amendment is the path
— never bare top-level commands.

## Consequences

- The four session tiers and every existing route are untouched; the operator
  manages every session and the live-editable settings from one page, and the
  previously un-terminable headless `--session` session is now admin-terminable.
- `session.json`'s event log gains `terminating` / `termination_cancelled` /
  `extension_requested` / `reopened` / `rejected`, but the receipt still carries
  no token, password, salt, hash or address (`extension.by` is a client name).
- No new on-disk write path, so the one path sanitiser and the 0600 discipline
  are unchanged; nothing persists across a restart beyond the overrides file.
- Alternatives rejected: an admin cookie/session (the spec puts the token in the
  header); freezing the transfer during `TERMINATING` (defeats the grace window);
  a store-level broadcaster for the admin SSE (a 1 s diffed ticker suffices for a
  single operator); and shipping bare `airlift sessions`/`fetch` subcommands
  (contradicts ADR 0010 and the operator's minimalism).
