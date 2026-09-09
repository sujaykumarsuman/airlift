# STATUS

## Phase 0 — Scaffold: done

- Layout, `CLAUDE.md`, `docs/{BUILD-PLAN,PROTOCOL,API}.md`, ADRs 0001–0008.
- Pre-commit gate wired for all three components; each has a passing no-op test.
- `make web` / `make tower` / `make tower-all` work end to end on the stubs.

## Next — Phase 1: Protocol + sender

- `sender/airlift.py`: `beam`, `frames`, `decode`.
- `sender/testdata/vectors.json` from `--seed 1`; `testdata/bundles/` fixtures.

## Open questions

- None.
