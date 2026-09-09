# ADR 0007 — Browser ↔ server transport is HTTP only

Status: accepted (Phase 0)

## Context

The phone needs to push frames continuously and both browsers need live
state. WebSockets fit but pull in a dependency, complicate the TLS and proxy
story, and give the relay a connection to keep alive rather than a buffer to
drain.

## Decision

Phone → server is batched `POST /frames`. Server → browsers is Server-Sent
Events. Go's standard library carries both; there is no WebSocket
dependency.

## Consequences

- The relay's failure mode is simple: a failed POST stays in the buffer and
  is retried (ADR 0004).
- SSE reconnects for free in the browser, and a snapshot per event keeps
  clients stateless.
- `vite dev` can proxy `/api` to a running tower with no special handling.
