# ADR 0019 — Shared session, scan on demand, presence-keeps-alive

Status: accepted (Phase 10, prompt 002)

Reshapes the join/role model and the expiry model. Revises ADR 0013 (the three
clocks, "presence is not activity") and the roles in CLAUDE.md; builds on ADR
0018 (reopen by link) and ADR 0017 (auth tiers).

## Context

The tower page and the scan page were reached by different links: the join QR
pointed at the scan page, so a phone that scanned it went straight into a camera
relay, and the dashboard was a separate "watch" link. In use this read wrong — a
session is one shared place, and every client should land on the same dashboard
(watch, download, and *optionally* scan), not be pinned to a scanner by the link
they opened.

The expiry model also surfaced awkwardly: with a stream connected the inactive
clock counted down a visible "expires in 29 min" even though someone was plainly
present, and the session-admin +1 h control extended a cap that was not the
binding clock, so it appeared to do nothing. Two smaller gaps: a scanner kept the
camera running after the beam was fully received (manual close), and a session
creator had no button to delete a session.

## Decision

**One shared session; scanning is on demand.** `JoinURL` is now the dashboard
deep link (`…/#s=<sid>&t=<token>`), so the QR, the printed link and the copy
button all open the shared dashboard — any client watches, downloads and can
invite others. The scanner is opened on demand by a **Scan a beam** button that
`window.open`s the scan page (`…/s/<sid>#t=<token>`) for the session, available to
every client. The role rule from CLAUDE.md still holds — a camera-bearing client
*can* scan — but the landing page is always the dashboard, and scanning is a
deliberate action, not the link's destination.

**Presence keeps a connected session alive.** ADR 0013's "presence is not
activity" is dropped for the connected case: while any client stream is open the
session is bounded only by the `max_age` hard cap. When the last stream leaves,
the idle grace (`idle_ttl`, the one remaining "everyone left" timer) runs, then
it suspends → reopen by link (ADR 0018). `inactive_ttl` is **removed** — the
config key, the create option, the `/api/info` cap and the clock are gone; idle
is the sole after-they-leave grace. A suspension is reopenable only for
`idle_ttl` now.

**The countdown and +1 h appear only near the cap.** The dashboard shows no
expiry countdown while a session is comfortably alive; within the last **30 min**
before the `max_age` cap it shows "ends in …" and, for a session admin, the
**+1 h** button, which pushes the cap out an hour (clearing the countdown again).
So the control acts on the clock it is next to.

**The scanner self-stops and can close.** When the beam a scanner is feeding
reaches `READY` (all packets received, detected on the RECEIVING→READY edge while
its own camera is running), it stops the camera and shows **Close** (which
`window.close()`s the tab — script-closable because the Scan button opened it) and
**Scan another** (restart the camera for the next beam).

**A session admin can delete.** `DELETE /api/sessions/{sid}` still soft-terminates
(freeze, keep files for `terminated_ttl`); with **`?hard`** it purges the session
and its files at once (`Store.Delete` → streams get `closed`, `<data_dir>/<sid>`
reclaimed). The dashboard gives admins **End session** (soft) and **Delete now**
(hard, confirmed).

## Consequences

- A session is genuinely one shared place: the link is an invitation to it, and
  every participant has the same view plus an opt-in scanner.
- The lifecycle is simpler to reason about — connected ⇒ alive (to the cap);
  empty ⇒ idle grace ⇒ suspend ⇒ reopen by link; a deliberate terminate or the
  cap ⇒ review flow. One fewer clock, and no misleading countdown.
- The activity ping (ADR 0013) is now largely vestigial while connected (presence
  is what keeps a session alive); it is left in place, harmless, and still marks
  the last-activity used for the idle base after everyone leaves.
- Memory-only is unchanged (a restart drops every session); hard delete is the
  immediate counterpart to the sweep.
