# ADR 0009 — Fountain mode is an LT code with fixed, cross-implementation parameters

Status: accepted (Phase 4)

## Context

Sequential mode needs every one of N frames; a missed frame costs a whole
extra pass of the loop, and stragglers dominate long transfers. A rateless
code makes any N(1+ε) distinct packets sufficient, so loss only delays
completion by the frames lost. The sender is Python and the receiver is Go,
so the packet construction must be reproducible bit for bit in both.

## Decision

`beam --fountain` emits LT (Luby transform) packets. Packet `seq` XORs the
source chunks `fountain_indices(seq, N)`, drawn by:

- **Degree** from a robust soliton distribution with `c = 0.1`,
  `δ = 0.5`, computed in IEEE doubles with plain left-to-right operations
  (no fused multiply-add: Go wraps products in `float64()`), then quantised
  to cumulative thresholds scaled to 2^32. The spike term is dropped when
  `R ≤ δ` (N ≤ 4) so every weight stays non-negative.
- **Randomness** from xorshift32 seeded with
  `seq·2654435761 + 2654435769 (mod 2^32)`, four warm-up rounds, one draw
  for the degree, then draws reduced modulo N (rejecting repeats) for the
  indices. Sixteen-bit seeds cap a beam at 65 536 distinct packets.
- **Packet count** defaulting to `N + max(48, ⌈3·√N·ln N⌉)`: measured with
  this distribution, decoding needs up to ~2.7 N packets at N = 24 but only
  ~1.2 N at N = 1200, and the surplus term covers the worst observed case
  with margin while staying near 1.2 N for large transfers. Every packet
  is a full chunk; the last source chunk is zero-padded before coding.
- **Decoding** by belief propagation (peeling) in both languages; DATA
  frames enter the same decoder as degree-1 packets, so one decoder serves
  both modes and any number of relays in any order.

`sender/testdata/vectors-fountain.json` carries the packets and, for
cross-checking, the index set of every seed; the Go tests require equality.

## Consequences

- Fountain beams are larger than sequential ones (about K/N times), and the
  loop repeats a fixed set of K packets rather than being truly endless.
- A 1-ulp difference between the two languages' `log` could in principle
  move one threshold by one unit in 2^32; a draw would have to land on that
  exact unit to matter. The vector test pins the case that ships.
- Two scanners on one session supply distinct packets at twice the rate; the
  server tests assert the resulting speed-up.
