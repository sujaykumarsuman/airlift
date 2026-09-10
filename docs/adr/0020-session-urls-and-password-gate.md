# ADR 0020 — Session URLs, human ids, and the password gate

Status: accepted (Phase 11 stage 1, prompt 002)

Revises how a session is named, linked and joined. Builds on ADR 0017 (the token
and the salted-password join), ADR 0019 (the shared dashboard) and ADR 0012 (the
path-prefix base href).

## Context

Sessions were named by a 16-hex id and joined through a fragment deep link that
always carried the 128-bit token (`…/#s=<sid>&t=<token>`). Two problems: the id
was not something you could read out or type, and — because the token rode in
*every* link — a join password did nothing (whoever opened the link already had
the token). A session should have a human handle, live at its own URL, and let a
password actually gate entry.

## Decision

**Human ids.** A session id is now three dash-separated lowercase triples, e.g.
`qkf-mzt-bwp` (`Store.freshIDLocked`, retried for uniqueness). The **token**, not
the id, gates access, so the id only needs to be readable and collision-free
(~42 bits, and access is rate-limited regardless). `session.ValidID` gives the
router a cheap format check.

**Every session at its own path.** The join link is `<base>/<sid>` (`JoinURL`),
and the dashboard page is served for `GET /{sid}` (and the scanner for
`/s/{sid}`), reading the sid from the URL — `sessionPage` 404s a mis-shaped id so
a stray path is not served the dashboard. The token, when present, stays in the
**fragment** (`…/<sid>#t=<token>`), never the path, so it never reaches logs.

**The token gates access; the password is its human alternative.**

- **Public session** (no password): the link carries the token
  (`…/<sid>#t=<token>`) for one-tap join. The dashboard strips the token from the
  address bar into per-session storage, so a reload rejoins. Opening the id
  *without* the token grants nothing — it needs the link (see stage 2).
- **Password session**: the link is the id alone (`…/<sid>`). Opening it shows a
  **password prompt**; a correct password (`POST …/join`) returns the token,
  which is loaded and stored on that device. The token never travels in the link.

The dashboard tells the two apart when it opens a token-less id by probing
`POST …/join` with an empty password: `401` ⇒ password-protected (show the form),
`404` ⇒ public/needs-its-link (or missing) — the privacy split from ADR 0017. The
snapshot gains `has_password` so a client can render the right share link and
guidance.

**Home page.** The create form is now just **Join password** + **Joiners are
session admins**. Alongside it, a **Join a session** box takes a session id and
navigates to `<base>/<sid>`.

**Layout.** The dashboard shows a **Session** panel — id, a labelled
**Participants** list (always shown), and the session controls (End / Delete,
+1 h) — kept distinct from the **beam** cards (Download / Remove).

## Stage 2 (deferred)

Opening a **public** session by id without its token currently says "needs its
link". The next stage turns that into an **admission (knock)** flow: a token-less
open registers a pending request, the session admin admits or denies it, and on
admit the token is issued — so a guessed id still yields no access without the
admin's say-so.

## Consequences

- A session has a readable handle, a shareable URL, and a password that actually
  gates entry. Guessing an id gets you nowhere without the token or the password.
- The token remains the real credential (strong, out of logs); the password is
  the low-tech, out-of-band gate on top of a token-less link.
- `JoinURL`'s shape now depends on `HasPassword`, and the old
  `#s=<sid>&t=<token>` deep link and 16-hex id are gone (ADR 0017/0019 amended).
- Memory-only is unchanged; ids are per-run and not stable across a restart.
