# ADR 0011 — QR rendering uses rsc.io/qr/coding with a forced version and a penalty-chosen mask

Status: accepted (Phase 5, prompt 002)

## Context

The Python sender rendered frames with `segno`. In Go the beam must encode
every frame of a beam at one fixed QR version so the symbol geometry never
changes on screen (only the version's data capacity bounds `--chunk`), in
alphanumeric mode (the base45 alphabet is exactly QR's alphanumeric set, ADR
0002), at a selectable ECC level, choosing the mask by the standard penalty
score. Two libraries were candidates: `rsc.io/qr`, already a dependency for the
terminal join QR, and `github.com/skip2/go-qrcode`.

## Decision

Render with **`rsc.io/qr/coding`**. It already ships in the module, so no new
dependency is added, and its low-level API gives exactly the control needed:
`coding.NewPlan(version, level, mask)` forces a plan, `plan.Encode(coding.Alpha(text))`
encodes alphanumeric data, and `Version.DataBytes(level)` exposes the capacity.

- **Version** is the smallest whose alphanumeric capacity holds the longest
  frame of the beam, computed from the same bit model `Encode` enforces
  (`4 + count-indicator + ⌈11·len/2⌉ ≤ DataBytes·8`). `ChunkForVersion`
  inverts this for `--version-target` and reproduces the README tuning table
  exactly (e.g. version 20 at ECC M → 628 bytes), because the capacity tables
  are the ISO/IEC 18004 tables both libraries implement.
- **Mask** is chosen per frame: `coding.NewPlan` takes an explicit mask and
  does no evaluation of its own, so `internal/beam` implements the four
  ISO/IEC 18004 §8.8.2 penalty rules and picks the lowest-scoring of the eight
  masks. The eight plans for the chosen version are built once and reused
  across every frame.
- **SVG** is rendered by the same run-length path algorithm the Python sender
  used, over the module matrix `Code.Black` exposes, with a four-module quiet
  zone in the viewBox.

## Consequences

- The dependency list stays "the standard library plus `rsc.io/qr`"; no ADR is
  needed to add a QR library because none is added.
- The emitted symbols are valid and scannable but not pixel-identical to
  segno's: the mask, and any padding of shorter frames, may differ. The beam is
  checked structurally (one path per frame, correct loop order, no external
  references), not against segno's bytes, so this is expected.
- The penalty evaluation runs eight encodes per frame at build time; for a beam
  of tens to a few hundred frames this is milliseconds and never on a hot path.
