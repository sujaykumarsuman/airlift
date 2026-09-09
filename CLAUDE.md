# airlift — project rules

Optical file transfer out of an air-gapped machine. A sender renders a file as
an animated QR loop on a monitor; a phone browser scans the loop and relays
decoded frames to a server on the operator's laptop, which reassembles,
verifies, unpacks and serves the result. Primary payload is a `repobundle`
text file (`docs/BUNDLE.md`, produced by `airlift beam`). One Go binary,
`airlift`, does all of it (ADR 0010). Personal tooling; device agnostic; no
ecosystem features (AirDrop, Quick Share, Continuity) anywhere in the main
path.

Canonical documents: `prompts/002-go-cli-and-hosting.md` (the current plan and
phases), `docs/PROTOCOL.md` (wire format), `docs/BUNDLE.md` (repobundle
format), `docs/API.md` (HTTP API), `docs/adr/` (locked decisions), `STATUS.md`
(where we are). `prompts/001-init.md` is the original plan, kept for history.

## Roles (canonical — do not let these blur)

- **Beam** — the HTML file produced by `airlift beam`, opened in a browser
  on the **air-gapped machine**. It displays the animated QR loop. It is fully
  offline: no network, no hosting, no dependency on tower or the `web/` build.
  Its player JS stays inline in the Go-emitted HTML. The beam is *not* a
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

## Components

- `cmd/airlift/` — Go, single static binary `airlift`. Module at repo root.
  Two user-facing subcommands only (ADR 0010): `beam` (bundle a folder/file(s)
  into a named QR page and open it — runs inside the air gap) and `tower` (host
  the server on the operator's laptop). Everything else is an internal process,
  not a command. `tower` owns sessions, protocol decode, reassembly,
  verification, bundle unpack, downloads, TLS; it serves the embedded web UI.
- `internal/beam` — the shared encoder (gzip → chunk → frame, sequential or
  fountain, `ModeAuto` picking per size), QR rendering (`rsc.io/qr/coding`,
  ADR 0011), the embedded HTML player, `Build` (the whole beam pipeline) and
  `Decode` (offline reassembly, used by tests). `beam` and `internal/replay`
  share it.
- `internal/bundle` — repobundle `Pack`/`Parse`, tree/zip writers, the one
  path sanitiser. `Pack` is a byte-for-byte port of the retired
  `tools/repobundle.py` (`docs/BUNDLE.md`).
- `internal/replay` — the simulated scanner (loop, loss, reordering, batched
  POSTs) that drives a tower without a camera; internal, for the dev loop and
  the end-to-end tests.
- `web/` — vanilla TypeScript + Vite, entries `scan` (phone) and `tower`
  (dashboard). No framework. Embedded into the Go binary via `embed.go`.

## Locked decisions (summary — each has an ADR in `docs/adr/`; do not revisit)

1. Opaque blob transport: input → gzip → chunk → frames; the QR layer never
   parses repobundle. (ADR 0001)
2. 18-byte big-endian frame header; MANIFEST / DATA / FOUNTAIN types; CRC-32
   over payload; compact-JSON manifest. (`docs/PROTOCOL.md`)
3. Frame bytes → base45 → QR alphanumeric mode, ECC M. (ADR 0002)
4. Sender output is one self-contained HTML file. (ADR 0003)
5. Loop schedule `[M, D0..D(N-1)]`, M re-inserted every 20 data frames.
6. The phone is a stateless relay: decode → dedup → batch → POST. (ADR 0004)
7. The server is the source of truth; in-memory sessions with TTL. (ADR 0005)
8. Verification chain: concat → sha256 → gunzip → sha256 → per-file sha256.
9. Bundle stage in Go after verification; zip / bare file / raw bundle
   downloads; one path sanitiser for zip entries and the `data_dir` tree. (ADR 0006)
10. Browser ↔ server is HTTP only: batched POST up, SSE down. (ADR 0007)
11. Auth is the session token: 128-bit random, base64url, URL fragment on the
    join link, header on every API call.
12. Plain HTTP behind a TLS-terminating reverse proxy (ADR 0012, supersedes
    0008): `public_url` carries the path prefix, `<base href>` is injected at
    serve time, `GET /api/info` is public. The built-in CA and `/ca.crt` are
    gone.
13. Plain HTTP on `listen` (default `127.0.0.1:8443`); the proxy strips the
    prefix and the router stays rooted; verified output is always written under
    `data_dir` (no `--dest`, no `--bind`); `X-Forwarded-For` is trusted only
    from `trusted_proxies`, read right-to-left. (ADR 0012)

## Non-goals

- Persistence across tower restarts. Sessions are memory-only; `data_dir` is
  emptied on start.
- Ecosystem features (AirDrop, Quick Share, Continuity) anywhere in the main
  path.

(The prompt-001 non-goals "Hosted / VPS deployment" and "Multi-user" are
overturned by prompt 002: the tower is a hosted, multi-user service. Hosting
transport landed in ADR 0012; the open multi-user session model lands in
ADR 0013.)

## Conventions

- Trunk-based. One short-lived branch per phase (`phase/N-name`), squash to
  `main`. Conventional Commits.
- Pre-commit gate (`pre-commit run --all-files`): `gofmt` + `go vet` +
  `go test ./...` for Go; `tsc --noEmit` + `eslint` + `vitest` for web. Keep
  it fast.
- `make web` builds `web/dist`; `make airlift` builds the binary; `make
  airlift-all` cross-compiles. `make` never runs the application.
  `web/dist/.gitkeep` must survive so `embed.go` compiles on a fresh clone.
- Go dependencies: the standard library plus `rsc.io/qr` — `rsc.io/qr` for the
  terminal join QR and `rsc.io/qr/coding` for the beam QR (ADR 0011). Web
  runtime dependencies: `zxing-wasm` (decoder fallback, wasm served from
  `/assets/`, never a CDN) and `qrcode` (join QR). Nothing else without an ADR.
- Browsers talk to the API with `fetch` only: SSE through a streaming fetch
  and downloads through blobs, because the token travels in a header.
- The dashboard is exercised without a camera by `internal/replay` inside
  `go test` (bundle → beam → replay through loss → READY → the tree restored);
  there is no `replay` command. `beam`'s fountain choice is automatic (ADR
  0010), so there is no `--fountain` flag.
- Shared fixtures: `testdata/bundles/` (trees plus the bundles
  `internal/bundle.Pack` reproduces from them, the byte-for-byte contract of
  ADR 0010) and `testdata/vectors/vectors*.json` (frames dumps for the multi
  base64 bundle, sequential and fountain layouts). The vectors are frozen from
  the original Python sender and are never regenerated from Go; the bundles are
  regenerated only when a tree changes (see `testdata/bundles/README.md`).
- The fountain packet construction is a cross-language contract (ADR 0009):
  it lives in `internal/proto/fountain.go`, keeps its arithmetic free of fused
  multiply-add, and is checked seed-by-seed against the frozen vectors.
- `STATUS.md` updated at the end of every phase: done / next / open questions.
- British English in docs.
- Tokens are never logged.

## Output style

Ultra-concise. No file listings, no step recaps, no summaries of what was
done. Report outcome + blockers only. Stop after each phase for review.
