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
- **Tower** — the Go binary that hosts sessions and the `web/` UI: on the
  operator's laptop for a LAN run, or — the deployed form — on the VPS behind
  Caddy TLS at `projects.sujaykumar.dev/airlift` (Phase 8, `docs/HOSTING.md`).
  Exactly one per session.
- **Dashboard** — the shared session view (the `tower` page). The join link/QR
  opens it (ADR 0019), so **every client lands here**: watch progress, download
  results, invite others. Multiple clients share one session.
- **Scanner** — the camera relay (the `scan` page), opened **on demand** from the
  dashboard's *Scan a beam* button, never by the join link itself. Any
  camera-bearing client can open it; it relays decoded frames and stops itself
  once the beam is received. Multiple scanners may feed one session.
- **Direct sender** — `airlift beam PATH -s LINK` on a machine that is **not**
  air-gapped (ADR 0023): the CLI joins the session as a `sender` participant and
  relays the frames over HTTP once a session admin approves that one beam on the
  dashboard. The approval is consent for the command-line path, not an access
  control — any token holder can relay frames as a scanner does. It never
  replaces the beam page for an air-gapped machine.

A device's role is decided by capability and page, never by a "sender mode": the
join link always lands on the dashboard, and a camera-bearing client *can* open
the scanner on demand. Inside an air gap the sender is the offline beam, by
construction, and nothing joins the session on its behalf. A machine that is
already on the network can send directly with `--to-session` — as an explicit,
admin-approved participant, never silently.

## Components

- `cmd/airlift/` — Go, single static binary `airlift`. Module at repo root.
  Two user-facing subcommands only (ADR 0010): `beam` (bundle a folder/file(s)
  into a named QR page and open it — inside the air gap; or, with `-s LINK`, send
  it straight to a session from a connected machine, ADR 0023) and `tower` (host
  the server on the operator's laptop). Everything else is an internal process,
  not a command. `tower` owns sessions, protocol decode, reassembly,
  verification, bundle unpack, downloads, TLS; it serves the embedded web UI.
- `internal/beam` — the shared encoder (gzip → chunk → frame, sequential or
  fountain, `ModeAuto` picking per size), the QR plan (`rsc.io/qr/coding`
  fixes the version; ADR 0011), the embedded HTML player with its inline QR
  encoder `qrjs.js` (the page encodes the frames' text itself — ADR 0011
  amended; bit-exact with the Go reference via `testdata/qr/matrices.json` and
  `web/src/beam/qrjs.test.ts`), `Build` (the whole beam pipeline) and `Decode`
  (offline reassembly, used by tests). `beam` and `internal/replay` share it.
- `internal/bundle` — repobundle `Pack`/`Parse`, tree/zip writers, the one
  path sanitiser. `Pack` is a byte-for-byte port of the retired
  `tools/repobundle.py` (`docs/BUNDLE.md`).
- `internal/replay` — the simulated scanner (loop, loss, reordering, batched
  POSTs) that drives a tower without a camera; internal, for the dev loop and
  the end-to-end tests.
- `web/` — vanilla TypeScript + Vite, five entries: `tower` (`index.html` — the
  landing + the session dashboard), `scan` (the phone scanner), `admin` (the
  operator console), `docs` (`docs.html`, static walkthrough with real
  screenshots in `web/public/docs/*.webp`) and `legal` (`legal.html`: the MIT
  licence, terms of use and privacy notes for the hosted tower — keep it true
  to what the tower actually stores). No framework. Embedded into the Go
  binary via `embed.go`. Shared modules of note: `shared/icons.ts` (the inline
  SVG symbol set), `shared/chunks.ts` (tally chunk marks + minimap, tested),
  `shared/motion.ts` (the one entry-animation hook), `shared/copy.ts` (copy
  buttons), `shared/style.css` (tokens + every component).
- `internal/term` — the terminal check and echo control the CLI's questions use
  (standard library only: termios on Unix, the console mode on Windows).
- `web/tools/scan-e2e.mjs` (`make scan-e2e`) — the scanner's end-to-end check
  without a phone: records the beam player's frames into an MJPEG, feeds it to
  headless Chrome as a fake camera on the real scan page of a throw-away tower,
  and waits for READY. Run it for any encoder/scanner change.
- `web/tools/docs-shots.mjs` (`make docs-shots`) — regenerates the docs
  screenshots from the real app with headless Chrome over CDP; rebuild after.

## Locked decisions (summary — each has an ADR in `docs/adr/`; do not revisit)

1. Opaque blob transport: input → gzip → chunk → frames; the QR layer never
   parses repobundle. (ADR 0001)
2. 18-byte big-endian frame header; MANIFEST / DATA / FOUNTAIN types; CRC-32
   over payload; compact-JSON manifest. (`docs/PROTOCOL.md`)
3. Frame bytes → base45 → QR alphanumeric mode, ECC M. (ADR 0002)
4. Sender output is one self-contained HTML file. (ADR 0003)
5. Loop schedule `[M, D0..D(N-1)]`, M re-inserted every 20 data frames.
6. The phone is a stateless relay: decode → dedup → batch → POST. (ADR 0004)
7. The server is the source of truth; in-memory sessions with TTL (ADR 0005). A
   session is a *place* holding a list of beams keyed by the sender u32
   (`bid` = its hex); each beam runs RECEIVING → VERIFYING → READY | FAILED
   independently. (ADR 0015)
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
14. Per beam on READY, `<data_dir>/<sid>/<bid>/{raw/<name>, tree/, <stem>.zip,
    meta.json}`, staged and renamed into place; downloads stream from those
    files, the in-memory copies freed; a persist failure keeps the beam READY
    from memory, a FAILED beam writes nothing; cleanup on delete/sweep. (ADR 0016)
15. Open multi-user access: a client registry (`X-Airlift-Client`; per address
    here, per device since ADR 0022), four auth tiers (public/token/client/admin),
    open creation with clamped options, a salted-SHA-256 password join,
    per-address/session rate limits (429 + `Retry-After`), address eviction, and
    operator beam removal + auto-evict of the oldest terminal beam at the cap.
    (ADR 0017)
16. Session lifecycle: `status` is OPEN or TERMINATED; a session-admin DELETE or a
    clock soft-terminates (freeze + keep files), then the two-phase sweep deletes
    after `terminated_ttl`; a session-level `session.json` receipt. Expiry is
    revised by ADR 0019 — presence keeps a connected session alive; idle + max_age
    only. (ADR 0013)
17. Admin surface: the lifecycle completes with TERMINATING (a `warning_ttl`
    grace window, `Live() = OPEN||TERMINATING`), a client extension request →
    PENDING_REVIEW, and an airlift-admin review (accept→reopen / reject→REJECTED).
    A fifth, orthogonal auth tier gates `/api/admin/*` on `admin_token` (404 when
    unset, else constant-time compare then `rate_admin`); the admin console lives
    at `/admin`. Live config keys are PATCH-able at runtime (atomic 0600 overrides,
    applied without a restart); the CLI stays `beam` + `tower` — `sessions`/`fetch`
    fold into `/admin`, so ADR 0010 stands. (ADR 0014)
18. Reopen by link: a session suspended by the idle grace (`system` terminate for
    `idle_ttl`; snapshot `reopenable`) revokes all access (downloads
    409 too) and is revived — every clock reset — simply by opening its link
    (register/join call `Reopen`); no admin review. Deliberate (session/airlift-
    admin) terminations and the `max_age` cap keep the ADR 0014 request→review flow
    and stay downloadable. A session admin extends the cap an hour at a time
    (`POST …/max-age`, the dashboard "+1 h"). Defaults: `idle_ttl` 30m (the
    everyone-left grace), `terminated_ttl` 1h (the reopen window). Beam default fps
    5 (10 since the 2026-09-14 decode-speed pass, with chunk 1311 = QR version 30
    and `--format auto`). (ADR 0018)
19. Shared session, scan on demand: the join link/QR opens the shared dashboard
    (`…/#s=<sid>&t=<token>`) — every client watches/downloads there and opens the
    scanner on demand via a *Scan a beam* button; the scanner self-stops on READY
    and can close its own tab. Presence keeps a connected session alive (bounded
    only by `max_age`); `inactive_ttl` is removed, idle is the sole everyone-left
    grace; the expiry countdown and the session-admin +1 h show only in the last
    30 min before the cap. A session admin can hard-delete (`DELETE …?hard` → purge
    session + files at once) beside the soft End. (ADR 0019)
20. Session URLs + the password gate: a session id is a human `xxx-xxx-xxx`
    (three lowercase triples) and the session lives at `<base>/<sid>`; the token
    stays in the fragment (public link) or is absent (password session), never the
    path. The token gates access — a public link carries it for one-tap join; a
    password session's link is the id alone and the dashboard prompts for the
    password (`/join` → token, loaded on the device). Guessing an id grants
    nothing. The home page is Create (password + joiners-admin only) + Join-by-id;
    the snapshot carries `has_password`; the dashboard shows a Participants list and
    keeps session controls separate from beam actions. Opening a public id without
    the token goes to the admission flow below. (ADR 0020)
21. Admission (knock): a client with a public session's id but not its token
    `POST …/knock {name?}` (public, rate-limited) → a pending request keyed by
    address; a password/missing/non-live session 404s (like `/join`, so a bare id
    reveals nothing). The knocker polls `GET …/knock` (pending/admitted-with-token/
    denied); a session admin `POST …/knock/{id} {decision}` admits (poll returns
    the token) or denies. The snapshot carries `knocks:[{id,name,at}]` (no address);
    the dashboard shows Requests-to-join (Admit/Deny) and the knocker a
    Waiting-to-be-let-in screen. The token is issued only on admit. (ADR 0021)
22. A client per device: `POST …/clients` mints a new client every time unless
    `X-Airlift-Client` names one of the session's clients bound to the same
    address, which is then returned (name kept, admin upgradable only) — so a
    reload keeps its identity and a second device behind the same NAT is a second
    participant. A resume from another address is refused (the id is public). The
    dashboard's scan link carries `&c=<client_id>` so the scanner resumes the
    dashboard's client. Eviction still bars the address, except a session admin
    evicting a client at their own address drops only that client. Amended
    2026-09-14: identity is a **resume key** (returned by create/join/register,
    kept in localStorage, sent as `X-Airlift-Client-Key` on every client-tier
    call and as `resume_key` on a resume) — it resumes and re-binds from any
    address, a keyless call passes only from the bound address; a client idle
    with no stream for 10 min is parked (hidden, kept) until its next keyed
    call. (ADR 0022)
23. Direct send (not air-gapped): `airlift beam PATH -s|--to-session LINK` relays
    the frames over HTTP instead of writing a page. The CLI gets in by the
    link's token, the password (asked on a terminal, echo off) or a knock;
    registers with `role: "sender"` (fresh or keyed only); and `POST …/uploads
    {name, bytes, chunks, sender_session}` asks leave — approved at once for a
    session-admin sender, else pending until a session admin approves/denies it
    on the dashboard (Upload requests; snapshot `uploads`, no address). A
    sender's frames are `403` without an approved request and `bad` unless they
    build exactly the declared beam (u32, manifest name/size/chunks, a beam the
    approval created); an approved request cannot change; the approval ends
    with its beam (READY/FAILED/removed) or a revoke, and a withdrawn, revoked
    or expired one discards the unfinished beam; 10 min expiry, 10 pending per
    session and 3 per address. `--wait` (default 3m) bounds the CLI's waits;
    any early end withdraws the request (`DELETE …/uploads/{uid}`). A sender
    with no stream and no open request is parked. The approval is consent for
    the CLI path, not an access control (a token holder can relay frames). The
    CLI asks for what is missing only on a terminal and draws progress on
    stderr. (ADR 0023)

## Non-goals

- Persistence across tower restarts. Sessions are memory-only; `data_dir` is
  emptied on start.
- Ecosystem features (AirDrop, Quick Share, Continuity) anywhere in the main
  path.

(The prompt-001 non-goals "Hosted / VPS deployment" and "Multi-user" are
overturned by prompt 002: the tower is a hosted, multi-user service. Hosting
transport landed in ADR 0012; the multi-beam place in ADR 0015; the open
multi-user access layer in ADR 0017; the session lifecycle in ADR 0013; and the
admin surface, review flow and runtime overrides in ADR 0014. The VPS deployment
landed in Phase 8 and the tower has been live and hardware-validated since; the
repo is public and releases are cut from `v*` tags.)

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
  there is no `replay` command. `beam`'s fountain choice is automatic by
  default (`--mode auto`, fountain from `FountainThreshold` chunks) and
  `--mode sequential|fountain` overrides it (ADR 0010, amended 2026-09-15).
- Shared fixtures: `testdata/bundles/` (trees plus the bundles
  `internal/bundle.Pack` reproduces from them, the byte-for-byte contract of
  ADR 0010), `testdata/vectors/vectors*.json` (frames dumps for the multi
  base64 bundle, sequential and fountain layouts) and `testdata/qr/matrices.json`
  (the Go reference encoder's symbols the player's JS encoder must reproduce
  bit for bit; `go test ./internal/beam -run TestQRFixtureCurrent -update`
  after a deliberate Go-side change). The vectors are frozen from the original
  Python sender and are never regenerated from Go; the bundles are regenerated
  only when a tree changes (see `testdata/bundles/README.md`).
- The fountain packet construction is a cross-language contract (ADR 0009):
  it lives in `internal/proto/fountain.go`, keeps its arithmetic free of fused
  multiply-add, and is checked seed-by-seed against the frozen vectors.
- Releases: tag `vX.Y.Z` on `main` → the release workflow builds
  `airlift-<os>-<arch>` (+ `.exe`, `SHA256SUMS`) and publishes a GitHub release;
  the landing's download links point at `releases/latest`. The binary is stamped
  with `VERSION` (`git describe --tags`, or the tag in the workflow) and reports
  it at `GET /api/info` and in the landing footer. **Deploy from a tag**: `make
  deploy` prints the version and notes an untagged HEAD — tag first (after any
  post-release fixes, cut the next patch) so the live tower shows a clean version.
- Web UI: one committed dark theme (tokens in `shared/style.css`), symbols are
  inline SVG from `shared/icons.ts` (no icon font, no CDN), motion is CSS on the
  shared tokens with `prefers-reduced-motion` honoured, layout is mobile-first
  (the dashboard splits at ≥1000px). Two gotchas the pages must respect: the
  tower injects `<base href>` into every page, so in-page links are
  `page#fragment`, never bare `#fragment`; and `[hidden]` is made authoritative
  with `!important` because components set their own `display`. Verify UI work
  in-browser at several widths; `airliftScan.demo(total, have)` previews the
  scanner HUD without a camera, and `internal/replay` / the frozen vectors drive a
  real beam through a local tower.
- The design reference is a Claude Design canvas (link in `STATUS.md`); its
  working files are not in the repo — re-seed from the artifact (`--extract`) to
  change it. It records intent; the code is the truth.
- `STATUS.md` updated at the end of every phase: done / next / open questions.
- British English in docs.
- Tokens are never logged.

## Output style

Ultra-concise. No file listings, no step recaps, no summaries of what was
done. Report outcome + blockers only. Stop after each phase for review.
