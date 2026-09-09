# STATUS

## Phase 5 — One `airlift` binary, two commands: built and verified

- `cmd/airlift` exposes only `beam` and `tower` (ADR 0010). The Python sender
  and `tools/repobundle.py` are retired; `internal/bundle.Pack` reproduces the
  four committed bundles byte for byte, `internal/beam` holds the shared
  encoder + `Build`, QR rendering (`rsc.io/qr/coding`, ADR 0011) and the
  embedded player, and `internal/replay` (camera-free dev loop) uses it.
  Bundling, the frame codec, decode and replay are internal, not commands.
- `beam PATH…` bundles a folder (git-aware) or several files, sends a single
  file as-is, always carries a name (folder/file name, `--name`, a `name:` line
  in `--files-from`, or a prompt), auto-selects sequential vs fountain by size
  (no flag), writes a self-contained page and opens it in the browser
  (`--no-open` to suppress). `--version-target` agrees with the README table.
- `tower` keeps its Phase 1–4 behaviour (local-CA TLS, `--dest`, LAN bind);
  `~/.airlift` config and preflight are Phase 6.
- Fixtures under `testdata/vectors/`; the gate, CI and release drop Python and
  build `airlift`; `docs/BUNDLE.md` added, ADRs 0010/0011, README/CLAUDE/
  PROTOCOL/API updated; the old `docs/BUILD-PLAN.md` retired.
- Verified: the four bundles reproduced byte for byte; a Go bundle → beam
  (auto fountain) → replay through loss → READY restores the multi tree on
  disk; the beam is structurally sound with no external references; `beam`
  fountain over the multi bundle yields the frozen index sets and decodes back;
  `beam .` and multi-file naming exercised through the CLI.

## Pending — the hosted tower and the hardware runs

- Phase 6–8 (prompt 002): the hosted, multi-user, HTTP-behind-a-proxy tower
  with sessions, clients, lifecycle and admin; the VPS. `tower`/`replay` still
  carry the local-CA TLS and `--dest` of Phase 1–4 until Phase 6 removes them.
- Hardware, still outstanding from Phase 3/4: Mac + Android, install `/ca.crt`
  once, scan the join QR, scan `beam.html` off the monitor, compare zip and
  `--dest` with the source; then a 1 MB bundle in fountain mode at ≥ 8 fps, and
  two phones on one session.

## Next

- Phase 6: `~/.airlift` config (read/create) + preflight for `tower`, overrides,
  env and flags; remove TLS/LAN/`--dest`; open multi-user sessions, clients per
  address, the lifecycle and on-disk data; `public_url` with `<base href>`; the
  web changes. ADRs 0012, 0013.
- **Multiple beams per session** (operator request): a live session should
  accept and list several named beams. The tower today binds one sender session
  per tower session (the first MANIFEST), so this needs the Phase 6 session
  model — a session holding N payloads keyed by beam name, each with its own
  reassembly and download. Design it there.

## Open questions

- Fountain auto-threshold is `FountainThreshold = 24` chunks; tune once real
  phone runs show where sequential stops being snappy enough.
- Whether Go's gzip ever crosses a chunk boundary on some toolchain and changes
  N for the multi fixture is guarded by the fountain vector test, which fails
  loudly if the packet count diverges.
