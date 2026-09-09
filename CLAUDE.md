# airlift — project rules

Optical file transfer out of an air-gapped machine. A sender renders a file as
an animated QR loop on a monitor; a phone browser scans the loop and relays
decoded frames to a server on the operator's laptop, which reassembles,
verifies, unpacks and serves the result. Primary payload is a `repobundle`
text file (`tools/repobundle.py`). Personal tooling; device agnostic; no
ecosystem features (AirDrop, Quick Share, Continuity) anywhere in the main
path.

Canonical documents: `docs/BUILD-PLAN.md` (phases), `docs/PROTOCOL.md` (wire
format), `docs/API.md` (HTTP API), `docs/adr/` (locked decisions),
`STATUS.md` (where we are). The originating prompt is `prompts/001-init.md`.

## Roles (canonical — do not let these blur)

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

## Components

- `sender/airlift.py` — Python 3.9+, single file, only dependency `segno`.
  Runs inside the air gap. Emits a self-contained HTML player.
- `cmd/tower/` — Go, single static binary `airlift-tower`. Module at repo
  root. Owns sessions, protocol decode, reassembly, verification, bundle
  unpack, downloads, TLS. Serves the embedded web UI.
- `web/` — vanilla TypeScript + Vite, entries `scan` (phone) and `tower`
  (dashboard). No framework. Embedded into the Go binary via `embed.go`.
- `tools/repobundle.py` — provided; read it, do not modify it.

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
   downloads; one path sanitiser for zip entries and `--dest`. (ADR 0006)
10. Browser ↔ server is HTTP only: batched POST up, SSE down. (ADR 0007)
11. Auth is the session token: 128-bit random, base64url, URL fragment on the
    join link, header on every API call.
12. TLS via a built-in local CA persisted under `os.UserConfigDir()/airlift/`;
    short-lived leaf per run; `GET /ca.crt`; `--cert/--key` overrides. (ADR 0008)
13. Bind to the detected LAN interface, not `0.0.0.0`; `--bind IP` overrides.
    `--dest DIR` always writes verified output to disk.

## Non-goals

- Hosted / VPS deployment. Do not add flags, modes or docs for it.
- Persistence across tower restarts. Sessions are memory-only.
- Multi-user. One operator, one laptop, one or more phones.

## Conventions

- Trunk-based. One short-lived branch per phase (`phase/N-name`), squash to
  `main`. Conventional Commits.
- Pre-commit gate (`pre-commit run --all-files`): `ruff` + `pytest` for
  sender; `gofmt` + `go vet` + `go test ./...` for tower; `tsc --noEmit` +
  `eslint` + `vitest` for web. Keep it fast.
- `make web` builds `web/dist`; `make tower` builds the binary; `make
  tower-all` cross-compiles. `web/dist/.gitkeep` must survive so `embed.go`
  compiles on a fresh clone.
- Python tooling runs through `uv` (`uv run --directory sender ...`).
- Shared fixtures: `testdata/bundles/` (trees plus the bundles
  `tools/repobundle.py` packs from them) and `sender/testdata/vectors.json`
  (`airlift.py frames --seed 1` over the multi base64 bundle). Regenerate
  only when their inputs change, with the commands in their READMEs, and
  commit inputs and outputs together.
- `STATUS.md` updated at the end of every phase: done / next / open questions.
- British English in docs.
- Tokens are never logged.

## Output style

Ultra-concise. No file listings, no step recaps, no summaries of what was
done. Report outcome + blockers only. Stop after each phase for review.
