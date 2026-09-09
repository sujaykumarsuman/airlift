# STATUS

## Phase 6 — hosted multi-beam tower (in progress)

- **6.1 config** (done): `internal/config` — `~/.airlift` layer, precedence
  flag > env > overrides > file > default, 21 keys (incl. `max_beams=10`,
  `max_age=24h`), 0600 admin_token, read-or-create template, atomic overrides,
  masked admin dump. Adversarially reviewed.
- **6.2 HTTP-only + base path + `/api/info`** (done, ADR 0012): `internal/tlsca`
  deleted; `tower` rewritten to plain HTTP driven by the config with a hardened
  data_dir preflight; `public_url` → `<base href>` injection; right-to-left
  X-Forwarded-For trust; `--dest`/`--bind`/`--cert`/`/ca.crt` gone; the web
  build is base-path-relative (Vite `base:'./'`, `document.baseURI`, SW/manifest
  runtime prefix). ADR 0008 superseded, 0007 amended; API.md/CLAUDE.md/README
  updated. Verified live: `/api/info`, `<base href>`, session create over plain
  HTTP, config + data-dir sentinel; a prefix-strip httptest proves rooted
  routing behind `/airlift`.
- **6.3 multi-beam session core** (done, ADR 0015): a session is now a *place*
  holding a list of beams keyed by the sender u32 (`bid` = its hex). `Session`
  routes each frame to its beam; a new sender's MANIFEST births a beam, a known
  one's is `dup`, pre-manifest frames are held per sender and adopted draining
  only that sender's bucket (never the whole hold). Each beam runs
  RECEIVING → VERIFYING → READY | FAILED on its own decoder; `max_beams` and a
  per-beam gzip ceiling bound the place. Snapshot is a place envelope
  (`{sid, relays, beams[], expires_at}`); frames reply is `completed_beams`;
  downloads are `?beam=<bid>&as=…`. Web: types split into `Beam`/`Snapshot`, the
  relay no longer latches `done`, the scan page tracks an active beam, the
  dashboard lists beam cards. `WAITING_MANIFEST` removed. API.md/PROTOCOL.md/
  CLAUDE.md updated; two-beam end-to-end test added. Go + web gates green.
- **6.4 per-beam on-disk + downloads-from-disk** (done, ADR 0016): on READY a
  beam is written to `<data_dir>/<sid>/<bid>/{raw/<name>, tree/, <stem>.zip,
  meta.json}`, staged in a sibling temp dir and renamed into place;
  `internal/server/persist.go` does the write, finalize composes it after verify.
  Downloads stream from disk via `http.ServeContent` (`Range`/`Content-Length`),
  the in-memory copies freed; `session.Download` gained a `Blob` seam
  (`MemBlob`/`fileBlob`). A persist failure keeps the beam READY served from
  memory (`saved_path` null); a FAILED beam writes nothing. Session dirs are
  reclaimed on delete/sweep via a new store `SetEvictHook`. Design + adversarial
  review by workflow; Go gates green under `-race`. `meta.json` not `beam.json`
  (ADR 0010). The session-level `session.json` (clients, lifecycle) is 6.6.
- **Next**: clients/password/limits (6.5), lifecycle (6.6), web lifecycle UI (6.7).

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

## Pending — the rest of Phase 6, then admin and the VPS

- Phase 6 remaining: 6.5–6.7 (below). Phase 7 admin surface; Phase 8 the VPS.
- Hardware, still outstanding from Phase 3/4 (now over the hosted, HTTP tower):
  Mac + Android, scan the join QR, scan `beam.html` off the monitor, compare the
  zip download with the source; then a 1 MB bundle in fountain mode at ≥ 8 fps,
  and two phones on one session.

## Next

- 6.5 open creation + clients + password + rate limits (incl. per-session
  `max_age` override, airlift-admin only; beam removal + auto-evict oldest
  terminal at cap), 6.6 lifecycle (status + three clocks +
  terminate/extend/review/reopen; the session-level `session.json` with clients
  and lifecycle events; directory cleanup tied to `TERMINATED`/`REJECTED`), 6.7
  web lifecycle UI. ADR 0013.

## Open questions

- Fountain auto-threshold is `FountainThreshold = 24` chunks; tune once real
  phone runs show where sequential stops being snappy enough.
- Whether Go's gzip ever crosses a chunk boundary on some toolchain and changes
  N for the multi fixture is guarded by the fountain vector test, which fails
  loudly if the packet count diverges.
