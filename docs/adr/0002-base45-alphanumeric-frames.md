# ADR 0002 — Frames are base45 in QR alphanumeric mode, ECC M

Status: accepted (Phase 0)

## Context

QR byte mode carries 8 bits per symbol character; alphanumeric mode carries
5.5. Encoding binary as text costs capacity, but binary frames are fragile:
many decoders, including the browser's native `BarcodeDetector`, only return
strings and mangle non-UTF-8 bytes.

## Decision

Frame bytes → base45 (RFC 9285) → QR alphanumeric mode, ECC level M. Base45's
alphabet is exactly QR's alphanumeric character set, so `segno` selects the
mode automatically; the overhead is roughly 3 % versus raw byte mode.

## Consequences

- Frames are text-safe. Every decoder works, including `BarcodeDetector`,
  and the relay can treat a frame as an opaque string (ADR 0004).
- The tower's base45 decoder is the only place frame text becomes bytes.
- ECC M balances density against the glare and motion blur of a phone
  filming a monitor; `--ecc` allows tuning per setup.
