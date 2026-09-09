# Protocol

The optical layer carries an opaque byte blob (ADR 0001). The sender never
parses the payload; the tower rebuilds the byte-identical input and only then
looks at what it is. `sender/airlift.py` is the reference implementation of
everything below, and `sender/testdata/vectors.json` is the fixture every
implementation must agree on.

## Pipeline

```
input file ──gzip──▶ blob ──chunk──▶ N chunks ──frame──▶ bytes ──base45──▶ text ──QR──▶ SVG
```

- **gzip**: level 9, mtime zeroed, no filename. Deterministic for a given
  zlib, which is all that is needed: the receiver verifies against the
  `gz_sha256` carried in the manifest, never against a recomputed stream.
- **chunk**: fixed payload size (`--chunk`, default 600 bytes); the last chunk
  is shorter. `N = ceil(gz_size / chunk)`, at least 1, at most 65535.
- **frame**: header + payload, below.
- **base45**: RFC 9285. Two bytes become three characters, least significant
  first; a trailing odd byte becomes two. The 45-character alphabet
  `0-9 A-Z space $ % * + - . / :` is exactly QR's alphanumeric set, so the QR
  encoder selects alphanumeric mode on its own (ADR 0002). A frame of `b`
  bytes is `3·⌊b/2⌋ + 2·(b mod 2)` characters. Decoders reject a length that
  is 1 modulo 3, any character outside the alphabet, a triplet above `0xFFFF`
  and a pair above `0xFF`.
- **QR**: `segno`, alphanumeric mode, ECC level M by default (`--ecc`), never
  Micro QR. Every frame of a beam is rendered at one version, the one the
  longest frame needs, so the symbol geometry on screen never changes;
  shorter frames (the manifest, the last chunk) get a free ECC upgrade within
  that version. A 4-module quiet zone is part of the SVG viewBox. At ECC M the
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

One pass is `N + ⌈N / 20⌉` frames, so `(N + ⌈N/20⌉) / fps` seconds; real runs
need more than one pass because frames are missed.

The player is one self-contained HTML file (ADR 0003): a single `<svg>` whose
path is swapped per frame, an inline loop driven by `requestAnimationFrame`,
and keys for pause, step, fps and fullscreen.

## Reassembly and verification chain

On the tower (ADR 0005), and identically in `airlift.py decode`:

1. Frames arrive as base45 strings; decode → parse → CRC check → dedup by
   `(session, type, seq)`.
2. Frames are held per sender session. The first MANIFEST seen binds the
   tower session to its sender session; frames held for other sender
   sessions are discarded and later ones rejected (counted as `bad` in the
   API). Holding DATA frames that precede the manifest matters because a
   scanner usually joins mid-loop.
3. The MANIFEST sets `N` and the expected hashes; the state moves
   `WAITING_MANIFEST → RECEIVING`.
4. DATA frames with `seq < N` fill the bitmap. When all `N` bits are set the
   state moves to `VERIFYING`.
5. Verify: concatenate chunks in `seq` order → the length must equal
   `gz_size` and the sha256 must equal `gz_sha256` → gunzip → the length must
   equal `orig_size` and the sha256 must equal `orig_sha256`.
6. If the plaintext starts with `#repobundle v1`, run the bundle stage
   (ADR 0006): unpack with per-file sha256 checks, then build the downloads.
7. Every hash matched → `READY`; anything failed → `FAILED` with the expected
   and actual values. Nothing is offered for download before `READY`.

## Frames dump

`airlift.py frames` writes the JSON that cross-implementation tests and
`airlift-tower --replay` consume:

```json
{
 "sender_session": 577090037,
 "manifest": {"name": "…", "gz_size": 14212, "…": "…"},
 "frames": ["<base45>", "<base45>", "…"]
}
```

`frames` holds each frame exactly once, in the order `[M, D0 … D(N-1)]`; the
loop schedule is the consumer's business. `manifest` is the MANIFEST payload
parsed, so a consumer can check its own parser against it.
`sender/testdata/vectors.json` is this dump for
`testdata/bundles/multi/bundle-base64.txt` with `--seed 1`.

## Sender session vs tower session

`session` in the frame header is a u32 minted by `airlift.py` per run, from
the OS CSPRNG or, with `--seed N`, from `random.Random(N)` so that dumps are
reproducible. The tower session (`sid` + token) is minted by the tower when
the dashboard creates one. A tower session accepts frames from exactly one
sender session, bound by the first MANIFEST it sees.
