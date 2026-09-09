# Protocol

The optical layer carries an opaque byte blob (ADR 0001). The sender never
parses the payload; the tower rebuilds the byte-identical input and only then
looks at what it is.

## Pipeline

```
input file ──gzip──▶ blob ──chunk──▶ N chunks ──frame──▶ bytes ──base45──▶ text ──QR──▶ SVG
```

- **gzip**: standard gzip, deterministic (mtime zeroed) so the same input
  yields the same `gz_sha256` on every run.
- **chunk**: fixed size (`--chunk`, default 600 bytes of payload); the last
  chunk is shorter. `N = ceil(gz_size / chunk)`; `N ≤ 65535`.
- **frame**: header + payload, below.
- **base45**: RFC 9285. Its 45-character alphabet is exactly QR's
  alphanumeric set (`0-9 A-Z space $ % * + - . / :`), so the QR encoder
  selects alphanumeric mode automatically (ADR 0002).
- **QR**: `segno`, ECC level M by default (`--ecc`), rendered as inline SVG.

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
| 14     | `crc32`   | u32  | CRC-32 (IEEE, as `zlib.crc32`) over the payload |
| 18     | `payload` | bytes | `len` bytes |

A receiver rejects a frame when the magic or version is wrong, `len` does
not match the bytes present, or the CRC does not match. It dedups accepted
frames by `(session, type, seq)`.

### MANIFEST payload

Compact JSON (no whitespace, keys in this order):

```json
{"name":"repo-bundle.txt","gz_size":48213,"gz_sha256":"…","orig_size":131072,"orig_sha256":"…","chunk":600}
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

### FOUNTAIN payload (Phase 4)

An LT-coded packet of `chunk` bytes. `seq` is the PRNG seed from which the
receiver regenerates the degree (robust soliton) and the set of source block
indices XORed into the payload. The last source chunk is zero-padded to
`chunk` bytes before coding. See `BUILD-PLAN.md` Phase 4.

## Loop schedule

The player cycles frames at `--fps` (default 8):

- **Sequential** (default): `[M, D0 … D(N-1)]` repeating, with `M`
  re-inserted after every 20 data frames (`--manifest-every`) so a scanner
  that joins mid-loop learns `N` promptly.
- **Fountain** (Phase 4): `[M, F(s0), F(s1), …]` endless, same `M` cadence.

Estimated transfer time at `fps` is `(N + N/20 + 1) / fps` seconds for one
clean pass; real runs need more than one pass because frames are missed.

## Reassembly and verification chain

On the tower (ADR 0005):

1. Frames arrive as base45 strings; decode → parse → CRC check → dedup.
2. A MANIFEST sets `N` and the expected hashes; the state moves
   `WAITING_MANIFEST → RECEIVING`.
3. DATA frames fill the bitmap. When all `N` bits are set the state moves to
   `VERIFYING`.
4. Verify: concatenate chunks → `sha256` must equal `gz_sha256` → gunzip →
   `sha256` must equal `orig_sha256`.
5. If the plaintext starts with `#repobundle v1`, run the bundle stage
   (ADR 0006): unpack with per-file `sha256` checks, then build the
   downloads.
6. Every hash matched → `READY`; anything failed → `FAILED` with the expected
   and actual values. Nothing is offered for download before `READY`.

## Sender session vs tower session

`session` in the frame header is a random u32 minted by `airlift.py` per run
and is what the tower dedups on. The tower session (`sid` + token) is minted
by the tower when the dashboard creates one. A tower session accepts frames
from exactly one sender session: the first MANIFEST it sees binds it, and
frames from a different sender session are counted as `bad`.
