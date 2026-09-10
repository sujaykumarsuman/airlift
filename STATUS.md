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
- **6.5 open multi-user access** (done, ADR 0017): a client per address
  (`X-Airlift-Client`) with four auth tiers (public/token/client/admin); open
  creation with clamped options; salted-SHA-256 password join + `PATCH` to
  set/clear it; token-bucket rate limits (create/join/frames, 429 +
  `Retry-After`); address eviction (`event: evicted`, 403); operator beam
  removal (`DELETE …/beams/{bid}`) + auto-evict of the oldest terminal beam at
  the cap. `internal/replay` and both web pages register a client; the dashboard
  lists clients with evict/remove controls; the scan page has a password-join
  form. API.md/CLAUDE.md + ADR 0017. Landed in three commits; Go gates green
  under `-race`, web tsc/eslint/vitest green.
- **6.6 session lifecycle** (done, ADR 0013): `status` OPEN/TERMINATED; three
  clocks (idle/inactive/max_age, earliest-wins) replacing the single inactive
  TTL; the activity-vs-presence model + `POST …/ping` (rate_ping); a session-admin
  `DELETE` and any clock soft-terminate (freeze frames/ping/patch/beam-removal
  with 409, keep files), then the two-phase `Sweep` deletes after
  `terminated_ttl`; `event: terminated`; a session-level `session.json` receipt
  (atomic, no secrets). Web `types.ts` gained `status`/`terminated`; the
  terminated-page UX + ping emission are 6.7. API.md/CLAUDE.md + ADR 0013. Go
  gates green under `-race`, web green. Extension/review/admin-terminate deferred
  to Phase 7.
- **6.7 web lifecycle UI** (done): a DOM-free, unit-tested activity pinger
  (`shared/ping.ts`) — both pages `POST …/ping` at most once a minute, only while
  visible and within 5 min of real input, keeping the inactive clock alive;
  `shared/lifecycle.ts` friendly who/why + countdown helpers (guarding Go
  zero-time). The scan page shows a full-screen ended overlay with a live
  countdown to `cleanup_at`; the dashboard shows an "expires in …" countdown
  while OPEN and a terminated panel with its own countdown (text-node patched so
  download buttons aren't rebuilt mid-click). `saved_path` was already surfaced.
  Web tsc/eslint/vitest green (13 new tests). Terminated-page controls needing
  admin (warning/cancel, extension form) stay Phase 7.
- **Phase 6 complete.**

## Phase 7 — admin surface, review flow, runtime overrides (done, ADR 0014)

Seven slices, each committed and reviewed:

- **7.1 lifecycle state machine** (done): the five states in the session package —
  OPEN → TERMINATING (an airlift-admin warning grace window, `Live()=OPEN||
  TERMINATING`) → TERMINATED → PENDING_REVIEW (one client extension request) → OPEN
  on accept / REJECTED on reject; `warning_ttl`/`review_ttl` as method arguments; a
  reopen rebases every clock; the SSE names each transition edge; the two-phase
  sweep reused, no new closed-flag or dir-delete path. Adversarially reviewed (no
  correctness findings; stale comments fixed).
- **7.2 extension route + client UI** (done): `POST …/extension` (client tier,
  `rate_extension`); the scan/tower pages drive their lifecycle chrome off the
  status — a TERMINATING warning banner + countdown (still live), a frozen overlay
  with an extension form → awaiting-review → reopened, or a rejected note.
- **7.3 admin auth + read routes + SSE** (done): the fifth tier gated by
  `admin_token` (404/constant-time/`rate_admin`); `GET /api/admin/config`,
  `…/sessions` (with client addresses), and `…/events` (a 1 s coalesced diff).
- **7.4 admin mutations** (done): warn/terminate-now, cancel, review, evict, and a
  no-activity download.
- **7.5 admin console** (done): a third Vite entry at `/admin` — login,
  pending-reviews-first, a live table with controls and a review form.
- **7.6 runtime overrides** (done): a live `PATCH /api/admin/config` that persists
  to the 0600 overrides file and applies without a restart (a `srv.live` atomic
  swap + `SetRates`/`SetMax`/`SetLimits`/`SetLifecycle`), plus the settings page.
- **7.7 ADR 0014 + docs** (done): the `sessions`/`fetch` resolution (fold into
  `/admin`, ADR 0010 unamended), ADR 0014, and the API.md/CLAUDE.md/README/STATUS
  updates.

Go gates green under `-race`; web tsc/eslint/vitest green.

- **Phase 7 complete.**

## Phase 8 — the VPS (done)

The tower is deployed and live at **https://projects.sujaykumar.dev** (Hostinger
KVM, Ubuntu 24.04). Caddy terminates TLS (Let's Encrypt, auto-renew) and
reverse-proxies to the tower on `127.0.0.1:8443`; the tower runs as the
unprivileged `airlift` systemd user with an `0600` config holding the admin token.
The prior `careerdock` stack was surveyed, backed up (verified, 332 MB, on the
laptop under `~/Backups/careerdock-20260910/`), then wiped to free ports 80/443.
Verified over the internet: a valid cert, the real client address through
`X-Forwarded-For` (not the proxy's), a beam driven to READY through 30% loss +
reorder, on-disk persistence + the unpacked tree, and admin terminate over HTTPS.
Deploy tooling in `deploy/` + `make vps-bootstrap` / `make deploy`; `docs/HOSTING.md`
documents it. `sessions`/`fetch` are the `/admin` table + Download (ADR 0014), so
no CLI subcommand was added.

- **Next**: the hardware runs below.

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

## Pending — hardware validation

- Hardware, still outstanding from Phase 3/4 (now over the hosted, TLS tower at
  `projects.sujaykumar.dev`): Mac + Android, scan the join QR, scan `beam.html`
  off the monitor, compare the zip download with the source; then a 1 MB bundle
  in fountain mode at ≥ 8 fps, and two phones on one session.
- Deferred from Phase 7: the airlift-admin-only per-session `max_age` override (a
  one-field extension of the admin terminate route; not in the exit demo).

## Open questions

- Fountain auto-threshold is `FountainThreshold = 24` chunks; tune once real
  phone runs show where sequential stops being snappy enough.
- Whether Go's gzip ever crosses a chunk boundary on some toolchain and changes
  N for the multi fixture is guarded by the fountain vector test, which fails
  loudly if the packet count diverges.
