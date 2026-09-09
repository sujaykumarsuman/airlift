# airlift — init prompt (v1)

Optical file transfer out of an air-gapped machine. A sender renders a file as
an animated QR loop on a monitor. A phone browser scans the loop and relays
decoded frames into a session. A server on the laptop owns the session: it
reassembles, verifies, unpacks, and serves the result to a dashboard on the
same laptop. Primary payload is a `repobundle` text file
(`tools/repobundle.py`).

Repo: `github.com/sujaykumarsuman/airlift`. Personal tooling. Device
agnostic: Mac + Android today, any laptop + any phone tomorrow. No ecosystem
features (AirDrop, Quick Share, Continuity) anywhere in the main path.

## Roles (canonical — restate verbatim in CLAUDE.md; do not let these blur)

- **Beam** — the HTML file produced by `airlift.py beam`, opened in a browser
  on the **air-gapped machine**. It displays the animated QR loop. It is fully
  offline: no network, no hosting, no dependency on tower or the `web/` build.
  Its player JS stays inline in the Python-emitted HTML. The beam is *not* a
  session participant and never talks to tower — that is what preserves the
  air gap.
- **Tower** — the Go binary on the operator's laptop (the Mac). It hosts the
  session and the `web/` UI on the LAN. Exactly one per session.
- **Scanner** — any camera-bearing browser on the LAN that joins the session
  and relays decoded frames (the `scan` page). Phones, tablets, or the tower
  laptop itself with a webcam. Multiple scanners may feed one session.
- **Dashboard viewer** — a browser on the LAN that joins to watch progress and
  download (the `tower` page). A device with no camera joins as a viewer only.

A device's role is decided by capability and page, never by a "sender mode":
camera → can be a scanner; no camera → viewer only. Nothing joins the session
as a sender. The sender is the offline beam, by construction. If a machine is
already on the LAN, it is not a sender — it would just upload to tower
directly (out of scope; see non-goals).

Three components:

- `sender/` — Python 3.9+, single file `airlift.py`, only dependency `segno`
  (pure Python, vendor-able). Runs inside the air gap. Emits a self-contained
  HTML player.
- `cmd/tower/` — Go, single static binary `airlift-tower`. Go module at repo
  root. Runs on the laptop, binds the LAN interface so the phone can reach
  it. Owns sessions, protocol decode, reassembly, verification, bundle
  unpack, downloads, TLS. Serves the embedded web UI.
- `web/` — vanilla TypeScript + Vite, two entry points: `scan` (phone: camera
  → decode → relay) and `tower` (dashboard on the laptop: create session,
  live progress, verdicts, download). No framework. Embedded into the Go
  binary.

## Non-goals

- Hosted / VPS deployment. The architecture does not preclude it (it would
  need a real certificate, an admin token, and rate limits) but nothing is
  built for it. Do not add flags, modes, or docs for it.
- Persistence across tower restarts. Sessions are memory-only.
- Multi-user. One operator, one laptop, one or more phones.

## Output style

Ultra-concise. No file listings, no step recaps, no summaries of what was done.
Report outcome + blockers only. Stop after each phase for review.

## Locked decisions (do not revisit; record each as an ADR in Phase 0)

1. **Opaque blob transport.** The QR layer never parses repobundle. Input file
   → gzip → chunk → frames. The server rebuilds the byte-identical input.
   Bundle handling is a separate post-transport stage (decision 9).
2. **Frame wire format** (big-endian, 18-byte header):
   ```
   0  magic    u16  0x414C
   2  ver      u8   1
   3  type     u8   0=MANIFEST 1=DATA 2=FOUNTAIN
   4  session  u32  random per sender run (distinct from tower session id)
   8  seq      u16  DATA: chunk index · FOUNTAIN: packet seed · MANIFEST: 0
   10 total    u16  N source chunks
   12 len      u16  payload length
   14 crc32    u32  over payload
   18 payload  bytes
   ```
   MANIFEST payload is compact JSON:
   `{"name","gz_size","gz_sha256","orig_size","orig_sha256","chunk"}`.
3. **Frame bytes → base45 → QR alphanumeric mode, ECC M.** Base45's charset is
   exactly QR's alphanumeric charset, so segno auto-selects alnum mode; cost is
   ~3% vs raw byte mode. Frames are text-safe, so every decoder works —
   including the native `BarcodeDetector`, which only returns strings.
4. **Sender output is one self-contained HTML file.** Inline SVG frames + JS
   loop. Zero runtime deps beyond a browser.
5. **Loop schedule.** Sequential: `[M, D0..D(N-1)]` repeating, M re-inserted
   every 20 data frames. Fountain (Phase 4): `[M, F(s0), F(s1), ...]` endless,
   same M cadence.
6. **The phone is a stateless relay.** It decodes QR → string, dedups within
   the session by string hash, batches, and POSTs. It never parses frames,
   never holds file bytes. All protocol logic lives in Go, once. If a POST
   fails the phone buffers and retries; nothing is dropped.
7. **The server is the source of truth.** Sessions are in-memory with a TTL.
   The server parses frames, validates crc, dedups by `(sender_session,
   seq)`, tracks the chunk bitmap, and runs the verification chain on
   completion. Dashboard and phone both read state over SSE. Multiple phones
   may relay into one session.
8. **Verification chain.** concat → sha256 vs `gz_sha256` → gunzip → sha256
   vs `orig_sha256` → (if bundle) per-file sha256 during unpack. Nothing is
   offered for download until every hash matches. Verdicts are shown on the
   dashboard with the actual hashes.
9. **Bundle stage in Go.** After verification the server sniffs for
   `#repobundle v1`. If present: unpack (Go port of `repobundle.py unpack`,
   both `text` and `base64` formats, per-file sha check, modes preserved) and
   offer *zip* when the bundle has more than one file, the *bare file* when
   it has exactly one, and the *raw bundle* always. If not a bundle: raw
   file only. Path safety: reject absolute paths and any `..` component;
   zip entries and `--dest` writes both go through the same sanitiser.
10. **Transport between browser and server: HTTP only.** Phone → server is
    batched `POST`. Server → browsers is SSE. No WebSocket dependency; Go
    stdlib carries both.
11. **Auth is the session token.** Created with the session, 128-bit random,
    base64url. Carried in the URL *fragment* for the phone join link so it
    never reaches logs; sent as a header on every API call.
12. **TLS via a built-in local CA.** `getUserMedia` needs a secure context,
    and an ephemeral self-signed cert means an interstitial on every run.
    Instead: tower generates a CA once and persists it under
    `os.UserConfigDir()/airlift/` (`~/Library/Application Support/airlift`
    on macOS). Each run it issues a short-lived leaf, signed by that CA, with
    SANs for every current LAN IPv4 plus `localhost` and `127.0.0.1`. The
    CA is served at `GET /ca.crt`. One-time bootstrap per phone: proceed
    through the interstitial once, open `/ca.crt`, install as a CA
    certificate; from then on no warnings, and DHCP address changes are
    irrelevant because the leaf is reissued under the same CA. `--cert/--key`
    overrides the whole mechanism (for mkcert users). Stdlib `crypto/x509`
    only.
13. **Bind to the detected LAN interface, not `0.0.0.0`.** `--bind IP`
    overrides. `--dest DIR` always writes verified output to disk in addition
    to serving downloads; it is the normal way results land on the laptop.

## Conventions

- Layout:
  ```
  CLAUDE.md  STATUS.md  README.md  Makefile
  go.mod  embed.go                     (root package embeds web/dist)
  cmd/tower/main.go
  internal/proto/      base45, frame codec, crc, manifest
  internal/session/    store, ingest, bitmap, completion, TTL
  internal/verify/     sha chain, gunzip
  internal/bundle/     repobundle unpack (Go port), zip builder
  internal/tlsca/      local CA, leaf issuance, persistence
  internal/server/     handlers, SSE, auth, embed serving
  docs/BUILD-PLAN.md  docs/PROTOCOL.md  docs/API.md  docs/adr/NNNN-*.md
  prompts/                             (this file lives here)
  sender/airlift.py  sender/test_airlift.py  sender/testdata/
  web/                                 (vite; entries: scan, tower)
  tools/repobundle.py                  (provided; read it, do not modify)
  testdata/bundles/                    (trees + bundles packed by the Python
                                        script, consumed by Go tests)
  ```
- `go:embed` cannot reach parent dirs, so `embed.go` sits at repo root and
  embeds `web/dist`. `make web` builds dist; `make tower` builds the binary;
  `make tower-all` cross-compiles darwin/linux/windows.
- Trunk-based. One short-lived branch per phase (`phase/1-sender`), squash to
  `main`. Conventional Commits.
- Pre-commit gate: `ruff` + `pytest` for sender; `gofmt` + `go vet` +
  `go test ./...` for tower; `tsc --noEmit` + `eslint` + `vitest` for web.
  Keep it fast.
- `STATUS.md` updated at the end of every phase: done / next / open questions.
- British English in docs.

## HTTP API (`docs/API.md` is the canonical copy)

```
POST   /api/sessions                      → {sid, token, join_url, expires_at}
GET    /api/sessions/{sid}                token → state snapshot
GET    /api/sessions/{sid}/events         token → SSE state snapshots
POST   /api/sessions/{sid}/frames         token → body {frames:[base45,...]}
                                          → {accepted, dup, bad, have, total, state}
GET    /api/sessions/{sid}/download?as=raw|file|zip   token → bytes
DELETE /api/sessions/{sid}                token
GET    /ca.crt                            local CA certificate (PEM)
GET    /                                  tower dashboard
GET    /s/{sid}                           scan page (token arrives in #t=)
```

State machine: `WAITING_MANIFEST → RECEIVING → VERIFYING → READY | FAILED`.
Snapshot includes: state, sender session, name, N, have, bitmap (bit-packed,
base64), decoded fps (server-side rate), connected relays, verdicts
`{gz_sha, orig_sha, bundle}` with expected/actual hashes, bundle summary
`{files, total_bytes}`, available downloads, `dest_path` once written, error.

## Phases

### Phase 0 — Scaffold
Layout above. `CLAUDE.md` (project rules, output style, locked decisions
summary, non-goals). `docs/BUILD-PLAN.md` (these phases). `docs/PROTOCOL.md`
(wire format, base45, loop schedule, verification chain). `docs/API.md`
(above). ADRs: opaque-blob transport; base45/alphanumeric frames;
HTML-player sender; stateless phone relay; server-owned sessions; Go bundle
stage; HTTP-only transport; built-in local CA. Pre-commit config. Makefile.
Empty `sender/`, `cmd/tower/`, `web/` with tooling wired and a passing no-op
test each.

Exit: `pre-commit run --all-files` green. Commit.

### Phase 1 — Protocol + sender
`sender/airlift.py` with subcommands:

- `beam --in FILE --out beam.html [--chunk 600] [--ecc M] [--fps 8]
  [--manifest-every 20] [--seed N]`
  gzip → chunk → frames → base45 → segno (alnum, ECC M) → SVG → HTML player.
  Print: N chunks, QR version chosen, gz ratio, estimated seconds at fps.
- `frames --in FILE --seed N --out frames.json`
  Dumps `{sender_session, manifest, frames:[base45,...]}` for cross-impl
  tests and for `airlift-tower --replay`.
- `decode --frames frames.json --out FILE`
  Pure-Python reassembly from a frames dump. Proves the codec round-trips
  inside the sender alone.

HTML player: full-screen centred QR, max square, black-on-white, no external
assets. Header: sender session, frame i/N, fps. Keys: space pause · ←/→ step
· +/- fps. Auto-loop. Everything inline.

Tests: base45 round-trip + known vectors; frame pack/parse; crc; manifest
JSON; gzip → chunk → frames → decode round-trip on a 50 KB random blob and on
`tools/repobundle.py` itself. Commit `sender/testdata/vectors.json` from
`--seed 1` over a small repobundle. Also commit `testdata/bundles/`: two
small trees (one single-file, one multi-file with a binary and a nested dir)
plus their `text` and `base64` bundles produced by `tools/repobundle.py pack`.

Exit: `beam` output opens in a browser and cycles; `decode` reproduces the
input bit-for-bit; tests green. Commit.

### Phase 2 — Tower core (no camera, no UI)
Everything server-side, driven by tests and `--replay`.

- `internal/proto`: base45 decode, frame parse, crc, manifest. Tested
  against `sender/testdata/vectors.json`.
- `internal/session`: create/get/delete, TTL sweep, ingest with dedup and
  crc rejection, bitmap, completion detection, per-session decoded-fps
  counter, relay tracking.
- `internal/verify`: sha chain + gunzip.
- `internal/bundle`: Go port of `repobundle.py unpack`. Handles both formats,
  relpaths with spaces, modes, per-file sha. Tested against
  `testdata/bundles/`: unpacked tree must match the committed source tree
  byte-for-byte and mode-for-mode. Zip builder preserves paths and modes.
  Sanitiser tested against `../x`, `/abs`, `a/../../b`, and `..` as a
  component.
- `internal/tlsca`: CA generate/load/persist (0600 key), leaf issuance with
  current LAN SANs, `--cert/--key` bypass. Tests: fresh CA created on empty
  dir; existing CA reused; leaf verifies against CA for a given IP; leaf
  rejected for an IP not in SANs.
- `internal/server`: all API routes, token auth, SSE, `/ca.crt`, limits
  (body 256 MiB, frames per POST 500, sessions 32). `GET /` and `/s/{sid}`
  serve placeholders until Phase 3.
- `cmd/tower`: `--dest DIR --bind IP --port 8443 --cert F --key F --ttl 1h`.
  On start prints dashboard URL. On session creation (from the dashboard, or
  `--session` flag for headless use) prints join URL as a terminal QR.
- `--replay FILE [--rate 8] [--drop 0.2] [--shuffle]`: feeds a frames dump
  into a fresh session as if a phone were relaying, with configurable loss
  and reorder. This is the primary dev loop and the CI end-to-end test.
- Tests: units above; httptest end-to-end that creates a session, replays
  vectors with 20% drop across three passes, asserts `READY`, correct
  verdicts, `--dest` contents, and that `download?as=zip` unpacks to the
  fixture tree.

Exit: `airlift-tower --dest /tmp/out --replay sender/testdata/vectors.json
--drop 0.2` reaches `READY`, writes the verified bundle and its unpacked
tree to `--dest`, and exits 0. Tests green. Commit.

### Phase 3 — Web + end-to-end on hardware
Vite project in `web/`, two entries, shared minimal styles, no framework.

`scan` (phone):
- Reads `sid` from path and token from `#t=`. Rejects if either missing.
- Camera: enumerate `videoinput`, selector in UI, default rear camera,
  continuous focus, highest resolution offered. The selector is a first-class
  path — it is how a USB-webcam phone or external camera works on a laptop.
- Decoder: `BarcodeDetector` (`qr_code`) if present, else `zxing-wasm`.
  Decode loop on `requestVideoFrameCallback` with rAF fallback.
- Dedup by string hash within the session; batch every 250 ms or 50 frames;
  POST `/frames`; on failure keep buffering and retry with backoff. Show
  buffered count if it grows.
- Progress from SSE: `have/N`, state, compact bitmap. Screen wake lock. Big,
  glanceable — this page is read from arm's length.

`tower` (dashboard):
- Create session. Shows join URL and a QR of it for the phone to scan. Shows
  a one-line "first time on this phone? install `/ca.crt`" hint with the
  link.
- Live: bitmap grid of chunks, `have/N`, decoded fps, elapsed, ETA, connected
  relays.
- On `READY`: verdicts with hashes, bundle summary (file count, bytes, first
  N paths), download buttons per decision 9, and the `--dest` path written.
- On `FAILED`: which hash failed, expected vs actual, reset action.
- Tests (vitest): dedup/batching logic; SSE state reducer; URL/token parsing.
  Camera and decoder are exercised on hardware only.

Dev: `vite dev` proxies `/api` to a running tower; `@vitejs/plugin-basic-ssl`
so the phone gets a secure context during UI iteration. README section
"First run on a phone": proceed through the interstitial once → open
`/ca.crt` → install as CA certificate (Android shows a persistent "network
may be monitored" notice while a user CA is installed; that is all it means)
→ never again on that phone. mkcert alternative via `--cert/--key`.

Exit: real hardware — `airlift-tower --dest /tmp/restored` on the Mac,
dashboard open on the Mac, Android scans the join QR, then scans `beam.html`
off the monitor; dashboard fills in live, reaches `READY`, zip download
unpacks to a tree matching the source, `--dest` has the same. Second run on
the same phone shows no certificate warning. Tests green. Commit.

### Phase 4 — Hardening
- Fountain mode: `beam --fountain`. LT codes, robust soliton degree
  distribution, `seq` carries the PRNG seed so the server regenerates block
  indices. Server: peeling decoder in `internal/proto`, multi-relay aware.
  Vectors + `--replay` coverage for fountain frames.
- Sender: `--version-target` — compute max chunk for a target QR version
  instead of guessing `--chunk`.
- Web: torch toggle if supported; service worker + web manifest so `scan`
  installs and reloads offline-first when served by tower.
- Review: tokens never logged; TTL sweep tested; body and frame limits
  enforced with clear 413/429; CA key file mode verified on start.
- README: tuning guide (fps vs QR version vs phone), throughput table, full
  workflow: `repobundle pack --format base64` → `airlift beam` →
  `airlift-tower --dest` → scan join QR → scan monitor → `READY`. Zero-hop
  variant: Android 14+ USB webcam mode into the laptop running `scan`
  directly against its own tower.
- GitHub Actions: lint + test all three; release workflow cross-compiles
  tower and attaches binaries.

Exit: 1 MB bundle transfers reliably in fountain mode from a monitor at
≥ 8 fps decoded; two phones on one session measurably faster than one.
Commit.

## Start

Read `tools/repobundle.py` first. Execute Phase 0. Stop.
