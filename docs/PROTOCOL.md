# Protocol

The optical layer carries an opaque byte blob (ADR 0001). The beam never
parses the payload; the tower rebuilds the byte-identical input and only then
looks at what it is. `internal/beam` (encode) and `internal/proto` (codec) are
the reference implementation of everything below, and
`testdata/vectors/vectors.json` is the frozen fixture every implementation must
agree on.

## Pipeline

```
input file ──gzip──▶ blob ──chunk──▶ N chunks ──frame──▶ bytes ──base45──▶ text ──QR──▶ canvas
```

- **gzip**: level 9, mtime zeroed, no filename. Deterministic for a given
  zlib, which is all that is needed: the receiver verifies against the
  `gz_sha256` carried in the manifest, never against a recomputed stream.
- **chunk**: fixed payload size (`--chunk`, default 1311 bytes — the largest
  that fits QR version 30 at ECC M); the last chunk
  is shorter. `N = ceil(gz_size / chunk)`, at least 1, at most 65535.
- **frame**: header + payload, below.
- **base45**: RFC 9285. Two bytes become three characters, least significant
  first; a trailing odd byte becomes two. The 45-character alphabet
  `0-9 A-Z space $ % * + - . / :` is exactly QR's alphanumeric set, so the QR
  encoder selects alphanumeric mode on its own (ADR 0002). A frame of `b`
  bytes is `3·⌊b/2⌋ + 2·(b mod 2)` characters. Decoders reject a length that
  is 1 modulo 3, any character outside the alphabet, a triplet above `0xFFFF`
  and a pair above `0xFF`.
- **QR**: alphanumeric mode, ECC level M by default (`--ecc`), never Micro
  QR. Every frame of a beam is rendered at one version, the one the longest
  frame needs, so the symbol geometry on screen never changes; shorter frames
  (the manifest, the last chunk) get a free ECC upgrade within that version.
  `beam` fixes the version and ships the symbol's plan (`rsc.io/qr/coding`,
  ADR 0011); the page carries the frames as text and encodes each symbol
  itself (ADR 0011 amended), drawn at an integer pixel pitch inside a 4-module
  quiet zone. At ECC M the
  largest frame that fits version 40 is 2260 bytes, so `--chunk` tops out at
  2242.

## Frame wire format

Big-endian, 18-byte header followed by the payload.

| Offset | Field     | Type | Meaning |
| ---:   | ---       | ---  | --- |
| 0      | `magic`   | u16  | `0x414C` (`"AL"`) |
| 2      | `ver`     | u8   | `1` |
| 3      | `type`    | u8   | `0` MANIFEST · `1` DATA · `2` FOUNTAIN |
| 4      | `session` | u32  | random per sender run; distinct from the tower session id |
| 8      | `seq`     | u16  | DATA: chunk index · FOUNTAIN: packet seed · MANIFEST: `0` |
| 10     | `total`   | u16  | `N`, number of source chunks |
| 12     | `len`     | u16  | payload length in bytes |
| 14     | `crc32`   | u32  | CRC-32 (IEEE 802.3, as `zlib.crc32`) over the payload |
| 18     | `payload` | bytes | `len` bytes |

A receiver rejects a frame when the magic or version is wrong, the type is
unknown, `len` does not match the bytes present, or the CRC does not match.
It deduplicates accepted frames by `(session, type, seq)`.

### MANIFEST payload

Compact JSON: no whitespace, ASCII only, keys in exactly this order.

```json
{"name":"bundle-base64.txt","gz_size":14212,"gz_sha256":"…","orig_size":19231,"orig_sha256":"…","chunk":600}
```

- `name` — base name of the input file; used only for downloads.
- `gz_size`, `gz_sha256` — of the gzip blob as transported.
- `orig_size`, `orig_sha256` — of the original input.
- `chunk` — payload bytes per DATA frame; every DATA frame except the last
  carries exactly this many.

`total` in the manifest frame header equals `N`; the tower sizes its bitmap
from it.

### DATA payload

Bytes `[seq·chunk, min((seq+1)·chunk, gz_size))` of the gzip blob.

### FOUNTAIN payload

An LT packet of exactly `chunk` bytes: the XOR of the source chunks listed
by `fountain_indices(seq, N)`, where `seq` is the packet's u16 seed. The
last source chunk is zero-padded to `chunk` bytes before coding, and the
receiver trims it back using `gz_size`.

`fountain_indices` is a cross-implementation contract (ADR 0009):

1. Degree distribution: robust soliton over 1..N with `c = 0.1`, `δ = 0.5`.
   `R = c · ln(N/δ) · √N`; spike position `M = ⌊N/R⌋` clamped to 1..N;
   `ρ(1) = 1/N`, `ρ(i) = 1/(i(i−1))`; `τ(i) = R/(iN)` for `i < M`,
   `τ(M) = R · ln(R/δ) / N` only when `R > δ`, else 0. Weights `ρ+τ` are
   normalised by their sum and accumulated left to right into thresholds
   `⌊acc · 2³² + ½⌋`, the last forced to 2³². Every step is a plain IEEE
   double operation in that order, with no fused multiply-add.
2. PRNG: xorshift32 (`x ^= x<<13; x ^= x>>17; x ^= x<<5`), state
   `seq · 2654435761 + 2654435769 (mod 2³²)` (1 if that is 0), four warm-up
   rounds discarded.
3. One draw picks the degree `d`: the number of thresholds ≤ the draw, plus
   one, capped at N. Further draws reduced modulo N pick `d` distinct chunk
   indices, repeats rejected.

The sender emits packets with seeds `0 … K−1`, `K` defaulting to
`N + max(48, ⌈3·√N·ln N⌉)`. Any `≈ 1.2 N` distinct packets decode a large
transfer; small N needs more, which the default's surplus term covers.

## Loop schedule

The player cycles frames at `--fps` (default 10):

- **Sequential** (small payloads): `[M, D0 … D(N-1)]` repeating, with `M`
  re-inserted after every 20 data frames (`--manifest-every`) so a scanner
  that joins mid-loop learns `N` promptly.
- **Fountain** (chosen automatically once N is large enough): `[M, F0 … F(K−1)]`
  repeating, same `M` cadence. Missed packets cost nothing beyond themselves:
  any sufficient set of distinct packets decodes.

One pass is `N + ⌈N / 20⌉` frames, so `(N + ⌈N/20⌉) / fps` seconds; real runs
need more than one pass because frames are missed.

The player is one self-contained HTML file (ADR 0003): a `<canvas>` the inline
encoder (`qrjs.js`, ADR 0011 amended) paints per frame from the frames' text
and the `PLAN`, an inline loop driven by `requestAnimationFrame`, and keys for
pause, step, fps, size, fullscreen and hiding the chrome.

## Reassembly and verification chain

On the tower (ADR 0005), and identically in the offline `internal/beam.Decode`:

1. Frames arrive as base45 strings; decode → parse → CRC check → dedup by
   `(session, type, seq)`.
2. Each sender session is its own beam within the tower session (ADR 0015). A
   MANIFEST for a new sender creates a beam; a MANIFEST for a known one is a
   re-inserted schedule frame (counted as `dup`). DATA/FOUNTAIN frames that
   precede their own manifest are held per sender and adopted when it arrives —
   which matters because a scanner usually joins mid-loop — without disturbing
   any other beam. This is the multi-beam place: one join field, many payloads.
3. A beam is born `RECEIVING` when its MANIFEST sets `N` and the expected
   hashes; there is no place-level `WAITING_MANIFEST`.
4. DATA chunks (as degree-1 packets) and FOUNTAIN packets feed that beam's
   peeling decoder; its `have` and bitmap report recovered chunks. When every
   chunk is recovered the beam moves to `VERIFYING`. Packets from several
   scanners merge in any order.
5. Verify: concatenate chunks in `seq` order → the length must equal
   `gz_size` and the sha256 must equal `gz_sha256` → gunzip → the length must
   equal `orig_size` and the sha256 must equal `orig_sha256`.
6. If the plaintext starts with `#repobundle v1`, run the bundle stage
   (ADR 0006): unpack with per-file sha256 checks, then build the downloads.
7. Every hash matched → `READY`; anything failed → `FAILED` with the expected
   and actual values. Nothing is offered for download before `READY`.

## Frames dump

The frames dump is the internal fixture format the frozen `testdata/vectors/`
files use and that `internal/replay` consumes to drive a tower without a
camera. It is not a user-facing artifact. Shape:

```json
{
 "sender_session": 577090037,
 "manifest": {"name": "…", "gz_size": 14212, "…": "…"},
 "frames": ["<base45>", "<base45>", "…"]
}
```

`frames` holds each frame exactly once, in the order `[M, D0 … D(N-1)]` (or
`[M, F0 … F(K-1)]` in fountain layout); the loop schedule is the consumer's
business. `manifest` is the MANIFEST payload parsed, so a consumer can check
its own parser against it. A fountain dump adds
`"fountain": {"packets": K, "indices": [[…], …]}`, the index set of every
seed, so a decoder can check its `FountainIndices` directly.
`testdata/vectors/vectors.json` and `vectors-fountain.json` are these dumps
for `testdata/bundles/multi/bundle-base64.txt`, frozen from the original Python
sender and never regenerated from Go (ADR 0010).

## Sender session vs tower session

`session` in the frame header is a u32 minted by `airlift` per run, from the
OS CSPRNG or, with `--seed N`, deterministically so that dumps are
reproducible. The tower session (`sid` + token) is minted by the tower when
the dashboard creates one. A tower session is a place that holds one beam per
distinct sender session; the sender u32 keys the beam (`bid` = its eight hex
digits) and never leaves the frame header (ADR 0015).
