# ADR 0003 — The sender emits one self-contained HTML player

Status: accepted (Phase 0)

## Context

The sender runs inside the air gap, where nothing can be installed and no
network exists. It needs to display frames at a steady rate with pause and
step controls. A GUI toolkit, a video file, or a terminal renderer would each
add a dependency or lose control over timing and size.

## Decision

`airlift.py beam` writes a single HTML file: inline SVG frames plus an inline
JS loop. Zero runtime dependencies beyond a browser. Full-screen centred QR,
maximum square, black on white; keys for pause, step and fps.

Still the decision after the Go port (ADR 0010: `airlift beam`, the player JS
inline in the Go-emitted page) and after ADR 0011's amendment (2026-09-15: the
frames are inline text and the page encodes the symbols itself) — one file,
offline, nothing to install.

## Consequences

- The beam is not a session participant and never talks to the tower. This
  is what preserves the air gap (see roles in `CLAUDE.md`).
- The player JS lives in the Python-emitted HTML, not in `web/`. The two
  must not share a build.
- Python 3.9+ with `segno` (pure Python, vendor-able) is the entire sender
  toolchain.
