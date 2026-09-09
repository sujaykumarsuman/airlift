# Build plan

Trunk-based; one short-lived branch per phase, squashed to `main`. Each phase
ends with the pre-commit gate green, `STATUS.md` updated, and a stop for
review. Locked decisions live in [`adr/`](adr/) and are not revisited here.

## Phase 0 — Scaffold ✔

Repository layout, `CLAUDE.md`, this plan, `PROTOCOL.md`, `API.md`, ADRs
0001–0008, pre-commit config, Makefile. Empty `sender/`, `cmd/tower/`, `web/`
with tooling wired and a passing no-op test each.

Exit: `pre-commit run --all-files` green. Commit.

## Phase 1 — Protocol + sender ✔

`sender/airlift.py` with subcommands:

- `beam --in FILE --out beam.html [--chunk 600] [--ecc M] [--fps 8]
  [--manifest-every 20] [--seed N]` — gzip → chunk → frames → base45 → segno
  (alphanumeric, ECC M) → SVG → HTML player. Prints N chunks, QR version
  chosen, gzip ratio, estimated seconds at the given fps.
- `frames --in FILE --seed N --out frames.json` — dumps
  `{sender_session, manifest, frames:[base45,...]}` for cross-implementation
  tests and for `airlift-tower --replay`.
- `decode --frames frames.json --out FILE` — pure-Python reassembly from a
  frames dump; proves the codec round-trips inside the sender alone.

HTML player: full-screen centred QR, maximum square, black on white, no
external assets. Header shows sender session, frame i/N, fps. Keys: space
pause · ←/→ step · +/- fps. Auto-loop. Everything inline.

Tests: base45 round-trip and known vectors; frame pack/parse; CRC; manifest
JSON; gzip → chunk → frames → decode round-trip on a 50 KB random blob and on
`tools/repobundle.py` itself. Commit `sender/testdata/vectors.json` from
`--seed 1` over a small repobundle. Commit `testdata/bundles/`: two small trees
(one single-file, one multi-file with a binary and a nested directory) plus
their `text` and `base64` bundles produced by `tools/repobundle.py pack`.

Exit: `beam` output opens in a browser and cycles; `decode` reproduces the
input bit for bit; tests green. Commit.

## Phase 2 — Tower core (no camera, no UI) ✔

Everything server-side, driven by tests and `--replay`.

- `internal/proto`: base45 decode, frame parse, CRC, manifest. Tested against
  `sender/testdata/vectors.json`.
- `internal/session`: create/get/delete, TTL sweep, ingest with dedup and CRC
  rejection, bitmap, completion detection, per-session decoded-fps counter,
  relay tracking.
- `internal/verify`: sha chain + gunzip.
- `internal/bundle`: Go port of `repobundle.py unpack`. Both formats, relative
  paths with spaces, modes, per-file sha. Tested against `testdata/bundles/`:
  the unpacked tree must match the committed source tree byte for byte and
  mode for mode. Zip builder preserves paths and modes. Sanitiser tested
  against `../x`, `/abs`, `a/../../b`, and `..` as a component.
- `internal/tlsca`: CA generate/load/persist (0600 key), leaf issuance with
  current LAN SANs, `--cert/--key` bypass. Tests: fresh CA created on an empty
  directory; existing CA reused; leaf verifies against CA for a given IP;
  leaf rejected for an IP not in SANs.
- `internal/server`: all API routes, token auth, SSE, `/ca.crt`, limits (body
  256 MiB, 500 frames per POST, 32 sessions). `GET /` and `/s/{sid}` serve
  placeholders until Phase 3.
- `cmd/tower`: `--dest DIR --bind IP --port 8443 --cert F --key F --ttl 1h`.
  Prints the dashboard URL on start. On session creation (from the dashboard,
  or the `--session` flag for headless use) prints the join URL as a terminal
  QR.
- `--replay FILE [--rate 8] [--drop 0.2] [--shuffle]`: feeds a frames dump
  into a fresh session as if a phone were relaying, with configurable loss
  and reordering. Primary dev loop and CI end-to-end test. The HTTP replay
  client lives in `internal/replay`, shared by the CLI and the end-to-end
  test; a file that is not a dump is encoded on the fly.
- Tests: the units above; an `httptest` end-to-end that creates a session,
  replays vectors with 20 % drop across three passes, asserts `READY`,
  correct verdicts, `--dest` contents, and that `download?as=zip` unpacks to
  the fixture tree.

Exit: `airlift-tower --dest /tmp/out --replay sender/testdata/vectors.json
--drop 0.2` reaches `READY`, writes the verified bundle and its unpacked tree
to `--dest`, and exits 0. Tests green. Commit.

## Phase 3 — Web + end-to-end on hardware ✔ (hardware run pending)

Vite project in `web/`, two entries, shared minimal styles, no framework.

`scan` (phone):

- Reads `sid` from the path and the token from `#t=`. Rejects if either is
  missing.
- Camera: enumerate `videoinput`, selector in the UI, default rear camera,
  continuous focus, highest resolution offered. The selector is a first-class
  path — it is how a USB-webcam phone or an external camera works on a laptop.
- Decoder: `BarcodeDetector` (`qr_code`) if present, else `zxing-wasm`. Decode
  loop on `requestVideoFrameCallback` with a rAF fallback.
- Dedup by string hash within the session; batch every 250 ms or 50 frames;
  POST `/frames`; on failure keep buffering and retry with backoff. Show the
  buffered count if it grows.
- Progress from SSE: `have/N`, state, compact bitmap. Screen wake lock. Big
  and glanceable — this page is read from arm's length.

`tower` (dashboard):

- Create session. Shows the join URL and a QR of it for the phone to scan.
  One-line "first time on this phone? install `/ca.crt`" hint with the link.
- Live: bitmap grid of chunks, `have/N`, decoded fps, elapsed, ETA, connected
  relays.
- On `READY`: verdicts with hashes, bundle summary (file count, bytes, first N
  paths), download buttons per ADR 0006, and the `--dest` path written.
- On `FAILED`: which hash failed, expected vs actual, reset action.
- Tests (vitest): dedup/batching logic; SSE state reducer; URL/token parsing.
  Camera and decoder are exercised on hardware only.

Dev: `vite dev` proxies `/api` to a running tower; `@vitejs/plugin-basic-ssl`
so the phone gets a secure context during UI iteration. README section "First
run on a phone": proceed through the interstitial once → open `/ca.crt` →
install as a CA certificate (Android shows a persistent "network may be
monitored" notice while a user CA is installed; that is all it means) → never
again on that phone. mkcert alternative via `--cert/--key`.

Exit: real hardware — `airlift-tower --dest /tmp/restored` on the Mac,
dashboard open on the Mac, Android scans the join QR, then scans `beam.html`
off the monitor; the dashboard fills in live, reaches `READY`, the zip
download unpacks to a tree matching the source, and `--dest` has the same. A
second run on the same phone shows no certificate warning. Tests green.
Commit.

Done without hardware: unit tests for the relay, SSE client, join and deep
links, reducer and bitmap; the built UI served by the tower over TLS; the
dashboard driven in a browser through `vite dev`, fed by `--replay --into`
and by frames injected into the scan page, through to `READY`, verdicts and
downloads. Still to run by hand: the Android camera path (decoder choice,
resolution, focus) and the certificate bootstrap.

## Phase 4 — Hardening

- Fountain mode: `beam --fountain`. LT codes, robust soliton degree
  distribution, `seq` carries the PRNG seed so the server regenerates block
  indices. Server: peeling decoder in `internal/proto`, multi-relay aware.
  Vectors and `--replay` coverage for fountain frames.
- Sender: `--version-target` — compute the maximum chunk for a target QR
  version instead of guessing `--chunk`.
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

Exit: a 1 MB bundle transfers reliably in fountain mode from a monitor at
≥ 8 fps decoded; two phones on one session are measurably faster than one.
Commit.
