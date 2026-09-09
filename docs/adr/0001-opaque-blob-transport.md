# ADR 0001 — Opaque blob transport

Status: accepted (Phase 0)

## Context

The primary payload is a `repobundle` text file, which has its own structure
(per-file boundaries, hashes, modes). It is tempting to make the QR layer
bundle-aware, for example one frame group per file.

## Decision

The QR layer carries an opaque byte blob. Input file → gzip → chunk → frames.
The tower rebuilds the byte-identical input before anything looks at its
contents. Bundle handling is a separate post-transport stage (ADR 0006).

## Consequences

- Any file transfers, not only bundles. The sender has no format knowledge.
- Verification is uniform: two hashes over the whole blob (`PROTOCOL.md`).
- gzip over the whole file compresses better than per-file, and base64
  bundles compress well.
- The bundle stage can be tested in isolation against `tools/repobundle.py`
  output, and the transport against random bytes.
