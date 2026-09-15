# ADR 0023 — Direct send: `airlift beam --to-session` from a machine that is not air-gapped

Status: accepted (2026-09-15)

Amends the roles in `CLAUDE.md` ("nothing joins the session as a sender … a
machine already on the LAN would just upload to tower directly — out of
scope"). Builds on ADR 0017 (client tiers), ADR 0020 (links and the password
gate), ADR 0021 (knock) and ADR 0022 (the resume key). The beam page (ADR 0003)
stays the only path out of an air gap; nothing about it changes.

## Context

airlift's sender is an offline QR page because the machine holding the files
has no network. In practice the operator also moves files from machines that
*are* connected — a laptop into a session a phone is watching, a build box into
a session a colleague downloads from — and there the page, the screen and the
camera are pure overhead: the same bundle, verified the same way, could reach
the tower in a second over HTTP. The tower already ingests frames over HTTP
from scanners, so the transport exists; what is missing is a sender that is a
participant, and a way for the session's admins to see and agree to what it
adds.

## Decision

**One flag, same command.** `airlift beam PATH -s LINK` (long form
`--to-session`) bundles and encodes exactly as for a page, then relays the
frames to the session named by `LINK` instead of writing HTML; the tower
verifies them as it does a scanned beam. There is no new subcommand (ADR 0010
stands). Page-only flags (`--no-open`, `--fps`, `--ecc`, `--version-target`,
`--manifest-every`, `--seed`) are named as ignored — a fixed `--seed` could name
a beam already in the session — and `--out` with `-s` is an error. With
`--mode auto` a direct send is sequential (HTTP loses nothing, so a fountain's
surplus buys nothing) and chunks default to 2712 bytes, the most a frame
carries (`proto.MaxFrameText`).

**Getting in follows the link.** A share link carries the token (ADR 0020). A
link without it probes `POST …/join` with an empty password — which the tower
answers `401` for a password session without spending the join rate budget, and
`404` for a public (or missing) one. For a password session the CLI asks for
the password on the terminal with echo off (three tries; off a terminal it stops
and says so); for a public one it knocks (ADR 0021) as `airlift beam · <name>`
and waits to be let in. A link whose path ends `/s/<sid>` is read as a scanner
link first and, if no tower answers there, as a tower mounted under a prefix
ending in `s`. Errors never repeat the link's fragment.

**A sender is a participant, and its upload is agreed.** The CLI registers
with `role: "sender"` — honoured for a fresh client or a resume that presents
the resume key, never for a keyless resume (which the bound address alone
would let turn someone else's client into a sender). A sender is listed with the
`sender` tag and holds an event stream (`?role=sender`: presence only, not a
relay, not a viewer) so the dashboard shows it online while it waits. It then
asks leave:

```
POST   /api/sessions/{sid}/uploads          client  → body {name, bytes, chunks, sender_session} → {id, status}
GET    /api/sessions/{sid}/uploads/{uid}    client  → {id, status: pending|approved|denied|expired|cancelled|done, by?}
DELETE /api/sessions/{sid}/uploads/{uid}    client  → 204; the sender withdraws its own request
POST   /api/sessions/{sid}/uploads/{uid}    s-admin → body {decision: "approve"|"deny"} → 204
```

- A request is **approved at once** when the sender is itself a session admin
  (a joiners-admin session); otherwise it is **pending** and the snapshot lists
  it as `uploads: [{id, client_id, client, name, bytes, chunks, at}]` — who,
  what and how big, no address. The dashboard shows session admins an **Upload
  requests** section with Approve and Deny, beside Requests to join.
- **What is approved is what arrives.** The frames handler refuses a sender's
  frames (`403 upload not approved`) unless it holds an approved request, and
  then accepts only frames that build that request's beam: its `sender_session`,
  a manifest matching the declared name, gzip size and chunk count, and only a
  beam that approval itself created (not one another participant started under
  the same u32). A request naming a beam already in the session is refused.
- **One request per client, fixed once approved.** An identical repeat returns
  the open request unchanged; a pending request for a different beam is replaced
  by a new request with a new id (so an admin's click on the old one admits
  nothing); an approved request cannot be changed — finish or withdraw it first.
- **An approval admits one beam and ends with it**: `done` when the beam
  reaches READY or FAILED (including failing on arrival, over the size limit)
  or is removed. A session admin can revoke an approval by denying it. A
  withdrawn, revoked or expired approval discards its unfinished beam — no other
  approval can finish it, so it would only hold a place and memory.
- **Limits.** A pending request expires after 10 minutes; an approval that
  carries no accepted frame for 10 minutes expires. At most 10 pending requests
  per session and 3 per address; the request is rate-limited like a join; an
  ended request's record is forgotten 10 minutes later (and when its client asks
  again); an evicted sender's open request is cancelled. Polls answer `409` once
  the session is not live, and a knock poll answers `none`.
- Scanners and viewers are untouched: their frames need no approval.

**This is consent, not an access control.** Any holder of the session's token
can relay frames — that is how a phone's scanner works (ADR 0019) — so a
participant willing to register as a viewer, or to open the scanner, needs no
approval. The gate guarantees something narrower and still worth having: an
honest `airlift beam -s` never adds a beam to someone's session unannounced,
the admin decides with the beam's name and size in view, and what they approve
is exactly what arrives. The token (or password, or admission) remains the
real boundary, as before.

**The CLI waits, then tells the truth.** `--wait` (default 3 minutes) bounds
each wait — to be let in, and for the approval — with a countdown. However the
run ends before the beam is READY — the wait running out, Ctrl-C (or SIGTERM,
SIGQUIT), a network failure, a refusal — an open request is withdrawn, which
discards a half-sent beam; the message says "withdrawn" only when a withdraw
was sent. A request the tower expired while `--wait` still has time is asked
again. After approval the frames go in batches (≤ 500 frames, ≤ 4 MiB), waiting
out `429`, halving on `413` or on a timeout; the reply timeout bounds the wait
for an answer, not the upload, so a slow uplink is not cut off. After the first
batch the CLI checks that the tower took the beam and stops at once, with the
tower's reason, if it failed on arrival. A gap the tower reports afterwards is
resent (the exact missing chunks, from the bitmap). Only calls that are safe to
repeat are retried after a dropped connection; a reply that is not a tower's is
reported, not retried. The run ends with the tower's verdicts and the dashboard
link; exit 0 only for READY, 1 for any other outcome (denied, timed out, failed
verification, unreachable, an older tower), 2 for bad usage, 130 when
interrupted. A tower that predates this refuses the `sender` role (`400 unknown
role`), and the CLI says it needs airlift v0.1.7 or later.

**A finished sender leaves.** A sender with no stream and no open request is
parked at once (hidden, record kept — ADR 0022's parking): when its last
stream closes, and when its request ends while it is gone. Repeated runs do not
pile up offline participants.

**The terminal is only a convenience.** Questions are asked only when both
stdin and stderr are a terminal (the driver is asked; `/dev/null` is not one):
with no PATH, what to beam (a shell-like line — quotes, `\ `, `~/`) and where (a
session link, or Enter for a page); several files without `--name`, the name; a
password session, the password. An interrupt ends a question at once, echo
restored. Long steps draw one live line on stderr — bundling, gzip, the
countdowns, a progress bar with rate and time left, verification — cut to the
terminal's width and redrawn in place, and written once per milestone off a
terminal; the permanent summary goes to stdout and never contains the token or
the password. `internal/term` provides the terminal check, width and echo
control with the standard library alone (termios and window-size ioctls on
Unix, the console mode and screen buffer on Windows).

## Consequences

- airlift gains a second, explicitly *non*-air-gapped way in. The roles now
  name it: the **direct sender**, a CLI participant whose upload a session admin
  agrees to. The offline beam remains the sender inside an air gap and never
  talks to anything.
- The approval is a courtesy with teeth only for the path it covers: it keeps
  honest command-line uploads visible and exact, but it is not a permission
  separating readers from writers — a token holder could always relay frames.
  Documentation says so rather than implying more.
- The wire format, verification chain and on-disk layout are unchanged — a
  directly sent beam is indistinguishable from a scanned one once READY.
- The tower gains four routes, a `sender` role (also recorded in the
  session.json receipt), the `uploads` snapshot field and two lifecycle events
  written for an admin's decision (`upload_approved`, `upload_denied`).
