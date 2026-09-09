# ADR 0006 — Bundle unpack is a Go stage after verification

Status: accepted (Phase 0)

## Context

Most transfers are `repobundle` files. Operators want the tree, not the
bundle, and they want it verified. Shelling out to `tools/repobundle.py` on
the laptop would work but adds a Python dependency to the tower and gives the
dashboard no per-file verdicts.

## Decision

After the transport hashes match, the tower sniffs for `#repobundle v1`. If
present it unpacks with a Go port of `repobundle.py unpack` — both `text` and
`base64` formats, per-file sha256 check, modes preserved — and offers a zip
when the bundle has more than one file, the bare file when it has exactly
one, and the raw bundle always. If it is not a bundle, only the raw file is
offered. Path safety: absolute paths and any `..` component are rejected; zip
entries and `--dest` writes go through the same sanitiser.

## Consequences

- `tools/repobundle.py` is the reference; the Go port is tested against
  fixtures it packed (`testdata/bundles/`) byte for byte and mode for mode.
- Nothing is downloadable until every hash, including per-file hashes, has
  matched.
- The sanitiser is a single function with its own adversarial tests.
