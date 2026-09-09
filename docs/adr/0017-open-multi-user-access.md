# ADR 0017 — Open multi-user access: clients, passwords, rate limits, eviction

Status: accepted (Phase 6.5, prompt 002)

Builds on ADR 0015 (a session is a place of beams). This is the access/identity
layer prompt 002 sketched as its "ADR 0013"; the number drifted because 6.2/6.3/
6.4 took ADR 0012/0015/0016, so it lands as 0017. The session lifecycle (the
status machine, the three clocks, `terminate`/`review`, the session-level
`session.json`) and the admin surface are separate, later work (still called ADR
0013 in prompt 002).

## Context

Through Phase 6.4 a tower session was guarded only by its token: anyone with the
token could do anything, and there was no notion of *who* was connected. Prompt
002 makes the tower a shared service where an operator creates a session with
options, people join it (by token link or password), the dashboard shows who is
present, and an admin can remove a misbehaving participant or a stale beam.

## Decision

- **A client per address.** A session holds a registry of clients, one per
  caller address (the address `clientAddr` already derives from the trusted
  X-Forwarded-For, ADR 0012). `POST /api/sessions/{sid}/clients` registers or
  returns the client bound to the caller's address, with a generated two-word
  name (or a proposed one, de-duplicated by a numeric suffix). The client id
  travels as `X-Airlift-Client` on every authenticated call and the SSE stream,
  and is rechecked against the caller's address each time — so it is not a
  secret. A client's roles are the union of its open streams' roles; the
  snapshot lists `clients[] {name, roles, session_admin, connected, last_active}`.
- **Four auth tiers** replace the single token check: *public* (`/api/info`,
  create), *bootstrap* (a valid token, no prior client: register, and join),
  *client* (token + a registered, non-evicted client whose id matches the
  caller's address: snapshot, events, frames, download), and *session-admin*
  (a client tier plus the admin flag: delete the session, evict a client, set
  the password, remove a beam).
- **Open creation with options.** `POST /api/sessions` takes an optional
  `{label, password, joiners_admin, max_gz_bytes, idle_ttl, inactive_ttl}` body,
  every limit clamped to the server cap (over a cap is a 400 that names it),
  registers the creator as the first session admin, and returns
  `{sid, token, client_id, name, join_url, expires_at}`. `idle_ttl`/
  `inactive_ttl`/`max_gz_bytes` are stored on the session for the 6.6 clocks;
  6.5 does not run them. (A headless `--session` session registers no creator,
  so it has no session admin and is not API-deletable — the operator kills the
  process; the TTL sweep reclaims it.)
- **Password join.** When a password is set, `POST /api/sessions/{sid}/join
  {password, name}` returns a token and a client without any prior token; it is
  a 404 when no password is set, so a token-less caller cannot tell a
  password-less session from a missing one. Passwords are a salted SHA-256 in
  memory, compared in constant time, never stored or logged; `PATCH
  /api/sessions/{sid} {password}` sets or clears it (session admin). Joiners are
  session admins iff `joiners_admin` was set.
- **Eviction bars an address.** `DELETE /api/sessions/{sid}/clients/{cid}`
  (session admin) marks the client's address evicted for the session's life:
  every client at that address is dropped, its streams get `event: evicted` and
  close, and its POSTs and re-registration are `403 {error:"evicted"}`.
- **Rate limits.** A token-bucket limiter (per address, and per session for
  join) guards create, join and frames, answering `429` with `Retry-After`; the
  session-concurrency cap does too. The frames check precedes the body read.
  Budgets come from the config `rate_*` keys; a zero budget disables a kind.
- **Operator beam control.** `DELETE /api/sessions/{sid}/beams/{bid}` (session
  admin) removes a beam and reclaims its on-disk directory. At the per-place
  beam cap a new MANIFEST auto-evicts the **oldest terminal** (READY/FAILED)
  beam to make room, rejecting only when nothing is terminal — so a verify in
  flight is never cancelled.

## Consequences

- Every client of the tower moves together: `internal/replay` registers a
  `replay` client and sends the header; both web pages register before they
  subscribe, show their own name, and the dashboard lists the clients with evict
  and remove-beam controls for an admin.
- The evicted-address set, the stream-close plumbing (`event: evicted`, and the
  SSE client stopping on a 403), and the two store cleanup hooks (session and
  beam directory) are the seams the 6.6 lifecycle (terminate/review) and the
  Phase 7 admin evict reuse. The snapshot builder is kept localised so the
  lifecycle's `status`/`terminate_at` fields slot in later without churn.
- The per-session `max_age` override the operator asked for is airlift-admin
  only, so it waits for the admin tier in Phase 7; 6.5 only stores and clamps
  `idle_ttl`/`inactive_ttl`/`max_gz_bytes`.
