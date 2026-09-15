# ADR 0011 — QR rendering uses rsc.io/qr/coding with a forced version and a penalty-chosen mask

Status: accepted (Phase 5, prompt 002); amended 2026-09-15 — the page encodes
the symbols itself (see the amendment below)

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
  across every frame. (Since the amendment below the page makes this choice,
  with the same rules; Go keeps the reference encoder for the tests.)
- **SVG** was rendered by the same run-length path algorithm the Python sender
  used, over the module matrix `Code.Black` exposes, with a four-module quiet
  zone in the viewBox. (Superseded by the amendment below: the frames travel
  as text and the page draws a canvas.)

## Consequences

- The dependency list stays "the standard library plus `rsc.io/qr`"; no ADR is
  needed to add a QR library because none is added.
- The emitted symbols are valid and scannable but not pixel-identical to
  segno's: the mask, and any padding of shorter frames, may differ. The beam is
  checked structurally (every frame's text and the plan present, correct loop
  order, no external references — "one path per frame" until the amendment),
  not against segno's bytes, so this is expected.
- The penalty evaluation ran eight encodes per frame at build time; since the
  amendment it runs in the page, once per frame (~5 ms for version 30), and
  `beam` builds a page of thousands of frames in well under a second.

## Amendment (2026-09-15) — the frames travel as text; the page encodes them

A beam stored every frame as a pre-rendered SVG path: about 27 KB per
version-30 frame against 2 KB of frame text, so a 7 MB payload became a 195 MB
page. The version and mask choice stay as decided above, but the rendering
moves into the player:

- **Go emits the plan, not the pictures.** `PlanQR` still fixes one version
  for the beam (the longest frame's) and `PlayerHTML` inlines `PLAN`
  (`internal/beam.PlayerPlan`): version, level, size, the block structure
  (`DataBytes`/`CheckBytes`/`Blocks` from `coding.NewPlan`) and two bitmaps
  over the modules — the function patterns and their colour. `FRAMES` is the
  base45 text of each frame.
- **The page encodes each symbol** with `internal/beam/qrjs.js`, embedded into
  the player (`//go:embed`; plain ES5, no dependency, still offline): the
  alphanumeric bit stream and padding as `Bits` writes them, Reed–Solomon check
  bytes over the same GF(256) and generator as `rsc.io/qr/gf256`, the block
  interleave and zig-zag placement `lplan` performs, the eight masks scored by
  the penalty rules of `penalty.go`, the format bits of `fplan`. It draws on a
  `<canvas>` at an integer pixel pitch (every module the same size — better
  for a camera than the scaled SVG) and caches each packed symbol after its
  first encode, warming the loop ahead of playback.
- **Bit-exact by test.** `encodeSymbol` remains the Go reference; `go test
  ./internal/beam -run TestQRFixtureCurrent -update` freezes its symbols into
  `testdata/qr/matrices.json` (twelve versions — every level at 1, 10, 30 and
  40, M elsewhere — four frames each: 96 symbols with their masks) and
  `web/src/beam/qrjs.test.ts` requires the page's encoder to
  reproduce every one module for module, then reads them back through
  `zxing-wasm`. `TestQRFixtureCurrent` fails when the Go side drifts from the
  fixture; `TestPlayerPlanShape` checks the plan against the coding tables for
  all 40 versions.

Consequences: a beam is now ≈ 1.5 × the bytes of its frames — base45 costs
1.5 × — so ≈ 1.5 × the gzip for a sequential beam and ≈ 2 × for a fountain one,
which carries about a third more packets than chunks (the 7 MB incompressible
payload's fountain page is 13.8 MB; the 24-frame `docs/adr` beam of the time
68 KB instead of 655 KB); `beam` no longer
encodes symbols, so a 7 500-frame build takes well under a second; the SVG path
renderer is gone. The player encodes a version-30 frame in ~5 ms, so 60 fps
remains within budget and nothing changed for the scanner (`make scan-e2e`
reaches READY as before).
