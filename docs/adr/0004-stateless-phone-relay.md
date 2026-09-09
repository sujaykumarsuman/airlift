# ADR 0004 — The phone is a stateless relay

Status: accepted (Phase 0)

## Context

The phone could reassemble the file itself and upload the result. That puts
protocol logic in two languages, makes multi-phone sessions impossible, and
leaves file bytes on a device that only needs a camera.

## Decision

The scan page decodes QR → string, dedups within the session by string hash,
batches, and POSTs. It never parses frames and never holds file bytes. All
protocol logic lives in Go, once. If a POST fails the phone buffers and
retries with backoff; nothing is dropped.

## Consequences

- Several phones can feed one session; the tower merges them (ADR 0005).
- The scan page stays small enough to be readable at arm's length and to be
  installed offline-first (Phase 4).
- The phone's tests cover dedup, batching and URL/token parsing only; the
  decoder is exercised on hardware.
