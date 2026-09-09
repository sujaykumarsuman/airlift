# STATUS

## Phase 5 — One `airlift` binary: built and verified

- `cmd/airlift` with `pack`, `unpack`, `beam`, `frames`, `decode`, `tower`,
  `replay`. The Python sender and `tools/repobundle.py` are retired (ADR 0010);
  `internal/bundle.Pack` reproduces the four committed bundles byte for byte,
  `internal/beam` holds the shared encoder, QR rendering (`rsc.io/qr/coding`,
  ADR 0011) and the embedded player, and `internal/replay` uses that encoder.
- `beam` takes `--in FILE` or `--root DIR [PATHS...]` (packs first); `--dump`
  writes the frames dump; `--version-target` agrees with the README table.
  `tower` and `replay` keep their Phase 1–4 behaviour (TLS, `--dest`).
- Fixtures moved to `testdata/vectors/`; the gate, CI and release workflow drop
  Python and build `airlift`; `docs/BUNDLE.md` added; README and CLAUDE.md
  rewritten for one binary; `docs/PROTOCOL.md`/`API.md` references updated.
- Verified: the four bundles reproduced byte for byte; `unpack` and a Go
  `pack → frames → replay → READY` round-trip the multi tree; the beam is
  structurally sound with no external references; `frames --fountain` over the
  multi bundle yields the frozen index sets and decodes back. Exit criterion:
  `beam --root testdata/bundles/multi/tree --fountain --dump beam.json` then
  `replay beam.json --drop 0.3 --shuffle` reaches READY in one pass.

## Pending — the hosted tower and the hardware runs

- Phase 6–8 (prompt 002): the hosted, multi-user, HTTP-behind-a-proxy tower
  with sessions, clients, lifecycle and admin; the VPS. `tower`/`replay` still
  carry the local-CA TLS and `--dest` of Phase 1–4 until Phase 6 removes them.
- Hardware, still outstanding from Phase 3/4: Mac + Android, install `/ca.crt`
  once, scan the join QR, scan `beam.html` off the monitor, compare zip and
  `--dest` with the source; then a 1 MB bundle in fountain mode at ≥ 8 fps, and
  two phones on one session.

## Next

- Phase 6: config file, overrides, env and flags; remove TLS/LAN/`--dest`; open
  multi-user sessions, clients per address, the lifecycle and on-disk data;
  `public_url` with `<base href>`; the web changes. ADRs 0012, 0013.

## Open questions

- None new. Whether Go's gzip ever crosses a chunk boundary on some toolchain
  and changes N for the multi fixture is guarded by the fountain vector test,
  which fails loudly if the packet count diverges.
