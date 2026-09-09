# ADR 0005 — The server is the source of truth for sessions

Status: accepted (Phase 0)

## Context

Progress, verdicts and downloads must agree between the phone and the
dashboard, with any number of phones relaying.

## Decision

Sessions are in-memory on the tower with a TTL. The tower parses frames,
validates CRC, dedups by `(sender_session, seq)`, tracks the chunk bitmap,
and runs the verification chain on completion. Dashboard and phone both read
state over SSE (ADR 0007).

## Consequences

- One implementation of the protocol, in `internal/proto` and
  `internal/session`, tested against the sender's vectors.
- Nothing survives a tower restart; that is a non-goal (`CLAUDE.md`).
- `--replay` can drive a session from a frames dump with loss and reordering,
  giving a hardware-free end-to-end test.
