# ADR 0021 — Admission: knock to enter a public session by id

Status: accepted (Phase 11 stage 2, prompt 002)

Completes ADR 0020: it turns "opening a public session by its bare id needs the
link" into an admission flow, so a session admin can let someone in without
handing out the token or a password. Builds on ADR 0017 (client-per-address,
session-admin tier) and ADR 0020 (session URLs, the token/password gate).

## Context

After ADR 0020, a public session's link carries the token and a password
session's link needs the password — but someone who has only a session's **id**
(told it, saw it on a screen) and neither the token nor a password had no way in.
Two poor options remained: hand them the full link (giving away the token), or
give the session a password. We want a third: they ask, and the session admin
admits them — a waiting-room.

## Decision

**Knock.** A client that opens a public session's id without a token can
`POST …/knock {name?}` (public, no token, rate-limited per address). It records a
**pending** admission request keyed by the caller's address, with the name they
typed, and returns its id. A **password** session (and a missing or non-live one)
returns `404` — indistinguishable, exactly like `/join` — so a bare id still
reveals nothing. A per-session cap (`maxKnocks`) bounds pending requests.

**Poll.** The knocker polls `GET …/knock` (public, keyed by address): `pending`,
`admitted` (with the session **token**), `denied`, or `none`. On `admitted` the
device stores the token and joins like any other client.

**Admit / deny.** Pending knocks appear in the snapshot as `knocks:
[{id, name, at}]` — **no address**, so no PII, and only a session admin can act on
the id. A session admin `POST …/knock/{id} {decision:"admit"|"deny"}` resolves it:
admit makes the poll hand back the token; deny tells the knocker they were
declined (they may knock again). The dashboard shows a **Requests to join**
section with Admit/Deny, and the knocker sees a **Waiting to be let in** screen
that flips to the session on admit.

So the token stays the real credential: it is issued to a knocker only when the
admin says so, and a guessed id yields nothing on its own.

## Consequences

- A public session can be shared as just its id, with the admin as the gate — no
  token leak, no password. It complements the token link (one-tap for people you
  trust with the link) and the password (a shared secret).
- Admission is address-keyed like the rest of the access layer (ADR 0017), and
  the snapshot carries names only — the address never leaves the server.
- Knocks are memory-only session state, swept with the session; the poll is a
  cheap public read, the knock and resolve are rate-/admin-gated.
- The token, once admitted, grants the same access as any holder — admission is
  the admin vouching for that address.
