# ADR 0018 — Reopen by link, suspended access, and session-admin max-age grants

Status: accepted (Phase 9, prompt 002)

Revises the recovery half of the session lifecycle (ADR 0013 clocks, ADR 0014
review flow). Builds on ADR 0013 (the three clocks, the two-phase sweep, the
`terminated.by`/`reason` receipt), ADR 0014 (the five states and the reopen that
rebases every clock) and ADR 0017 (the client/session-admin auth tiers).

## Context

ADR 0013/0014 gave every terminated session exactly one road back: a client
requests more time (`TERMINATED → PENDING_REVIEW`) and the **airlift admin** (the
tower operator) accepts or rejects it. That is right for a session an operator
*deliberately* ended, but heavy-handed for the common case — a session that was
only dropped because nobody touched it for a while. For a personal, multi-user
tool the person holding the session link should be able to bring such a session
back with no operator in the loop, exactly as they would by re-opening any link.

Two more wrinkles surfaced in use:

- The idle clock (10 m, no streams connected) and the inactive clock (30 m, a
  stream connected but no activity) gave two different "gone" times for what a
  user thinks of as one thing — "nobody has used it for a while".
- The 24 h `max_age` hard cap has no low-friction, session-owner escape hatch; a
  genuinely long-lived, actively-used session could only be kept alive by the
  operator through the admin surface.

## Decision

**One inactivity rule, a longer grace window.** `idle_ttl` defaults to **30 m**
(was 10 m) so "no activity for 30 minutes" is a single rule whether or not a tab
is still open; `terminated_ttl` defaults to **1 h** (was 30 m), the window in
which a suspended session can be reopened before the sweep deletes it.
`inactive_ttl` (30 m) and `max_age` (24 h) are unchanged.

**Suspended is a reopenable TERMINATED.** No new `Status`. A session is
*suspended* when it is `TERMINATED` with `terminated.by == "system"` and a reason
of `idle_ttl` or `inactive_ttl` — i.e. reaped by inactivity, not by a deliberate
terminate or the cap. The predicate is `reopenableLocked`; the snapshot carries a
boolean **`reopenable`** so both web pages branch on it without re-deriving the
reason.

**While suspended, access is revoked.** `Live()` already refuses ingest, ping,
patch and beam-removal on a `TERMINATED` session; ADR 0018 also **blocks
downloads** (`GET …/download` → 409) while `reopenable`. Nothing works until the
session is reopened — the receipt is kept on disk, but it is not served.

**Reopen by link — no review.** Registering a client (`POST …/clients`) or
password-joining (`POST …/join`) on a reopenable session first calls
`Session.Reopen`, which reuses ADR 0014's reopen (status `→ OPEN`, every clock
restarted from now, `max_age` grants cleared, the beams and their files intact)
and logs a `reopened` event naming the client. So simply opening the link revives
the session: a fresh visitor reopens it transparently, and an already-open tab
shows a **"Session paused — Reopen"** control that re-registers. `Reopen` is a
no-op (returns false) on anything not reopenable, so the two register paths call
it unconditionally.

**Deliberate terminations keep the review flow.** A `session admin` or
`airlift admin` terminate, and the `max_age` cap, are **not** link-reopenable:
they keep ADR 0014's `request more time → airlift-admin review` path and their
files stay downloadable through the terminated window. This is the whole point of
the split — an operator's decision is not undone by a passer-by with the link.

**A session-admin max-age grant.** `POST …/max-age` (session-admin tier) adds one
hour to the cap via a per-session `maxAgeBonus` folded into the `max_age`
deadline; the dashboard shows a **"+1 h"** button to session admins while the
session is live. So an actively-used session can outlive the 24 h cap without the
operator, while the cap itself stays in force by default. The grant resets on a
reopen.

## Consequences

- A session dropped for inactivity is recoverable by anyone holding its link for
  `terminated_ttl` (1 h), with no operator involved; the operator review flow is
  reserved for terminations an operator (or the cap) actually chose.
- The distinction rides on the termination receipt already written by ADR 0013
  (`by`/`reason`) plus one snapshot boolean — no sixth state, no new persistence.
- Beams still have no lifetime of their own: they live and die with the session,
  and the inactivity clock is reset by real activity (a frames POST with
  progress, a download, or an activity ping) exactly as before.
- Memory-only remains a non-goal exception (CLAUDE.md): a tower restart still
  drops every session, suspended or live. Reopen-by-link recovers a session only
  within a single tower process's lifetime.
- The `max-age` grant is a fixed one hour, repeatable; it is deliberately not a
  free-form override (that stays an operator concern), which keeps the
  session-admin surface a single button.
