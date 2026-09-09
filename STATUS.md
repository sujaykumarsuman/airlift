# STATUS

## Phase 1 — Protocol + sender: done

- `sender/airlift.py`: `beam`, `frames`, `decode`; base45, frame codec,
  manifest, gzip/chunk pipeline, reference decoder, SVG renderer, HTML player.
- `sender/testdata/vectors.json` (`--seed 1` over the multi base64 bundle) and
  `testdata/bundles/` (single + multi trees, text + base64 bundles).
- Sender suite runs green on Python 3.9 and 3.14.

## Next — Phase 2: Tower core

- `internal/proto` against `vectors.json`; `internal/bundle` against
  `testdata/bundles/`; session, verify, tlsca, server; `--replay`.

## Open questions

- None.
