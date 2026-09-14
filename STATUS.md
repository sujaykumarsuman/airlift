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

## Phase 9 — reopen-by-link, suspended access, max-age grants (done, ADR 0018)

Reworks the recovery half of the lifecycle so a session dropped for inactivity is
brought back by whoever holds the link, without the operator in the loop:

- **Suspended = reopenable TERMINATED** (no new state): a `system` terminate for
  `idle_ttl`/`inactive_ttl` sets a snapshot `reopenable` flag; while it holds, all
  access is revoked (downloads `409` too). Registering/joining calls a shared
  `Reopen` (reuses ADR 0014's clock-reset reopen), so opening the link revives it
  with no review. Deliberate (session/airlift-admin) terminations and the `max_age`
  cap keep the ADR 0014 request→review flow and stay downloadable.
- **Session-admin max-age grant**: `POST …/max-age` (s-admin) adds an hour to the
  cap via a per-session `maxAgeBonus`; the dashboard shows a **+1 h** button. The
  24 h cap stays the default; the grant (and a reopen) reset it.
- **Defaults**: `idle_ttl` 10m→30m (one 30-minute inactivity rule),
  `terminated_ttl` 30m→1h (the reopen window). Beam default **fps 8→5**.
- **Web**: scan + dashboard show a "Session paused — Reopen" panel for a suspended
  session (vs the ended/review overlay for deliberate terminations); the dashboard
  gains the +1 h button and a **"Scan with this camera"** button that opens the
  scan page for the session (the zero-hop variant) in a new tab.
- **Deploy**: the `admin_token` moves to `AIRLIFT_ADMIN_TOKEN` in an `0600`
  `/etc/default/airlift` loaded by the systemd unit (env beats the config file);
  bootstrap migrates any existing token so a re-run never rotates it.

ADR 0018 + API.md/HOSTING.md/CLAUDE.md/README updated. Go gates green under
`-race` (new session + server lifecycle-9 tests); web tsc/eslint/vitest green.
Verified in-browser: suspend → Reopen, the +1 h grant (server-logged), and the
scan-here button opening the scan page.

- **Phase 9 complete.**

## Phase 10 — shared session, scan on demand, presence-keeps-alive (done, ADR 0019)

Reshapes the join/role model and the expiry model from user feedback on the live
site:

- **Shared session, scan on demand**: `JoinURL` is now the dashboard deep link
  (`…/#s=<sid>&t=<token>`), so the QR/link/print all open the shared dashboard —
  every client watches, downloads and invites; the scanner is opened on demand by a
  **Scan a beam** button (any client) that `window.open`s the scan page. CLAUDE.md
  roles updated (dashboard is the landing, scanning is opt-in).
- **Presence keeps a connected session alive**: while any stream is open only the
  `max_age` cap bounds it; the idle grace runs when everyone leaves, then suspends →
  reopen by link. **`inactive_ttl` removed** across config/session/server/web +
  `/api/info` caps + create options; idle is the sole everyone-left grace, and only
  `idle_ttl` is reopenable.
- **Countdown + +1 h only near the cap**: the dashboard hides the expiry countdown
  and the session-admin +1 h until the last 30 min before the `max_age` cap, so the
  control acts on the clock it sits beside.
- **Scanner self-stops + closes**: on the RECEIVING→READY edge (all packets
  received) it stops the camera and shows **Close** (`window.close()`) + **Scan
  another**.
- **Session-admin delete**: `DELETE …?hard` purges the session and its files at
  once (`Store.Delete`); the dashboard gives admins **End session** (soft) and
  **Delete now** (hard, confirmed).

ADR 0019 + API.md/CLAUDE.md/HOSTING.md/README updated. Go gates green under `-race`
(TestLifecycleClocks/TestPresenceKeepsAlive rewritten for presence, TestHardDelete,
join_url format); web tsc/eslint/vitest green (44). Verified in-browser: the
dashboard-link QR + Scan a beam opening the scanner, the +1 h hiding the countdown
past 30 min, End/Delete now, and a live hard-delete → 404.

- **Phase 10 complete.**

## Phase 11 — session URLs, human ids, password gate, admission (done, ADR 0020/0021)

Staged: stage 1 is identity + joining (ADR 0020); stage 2 is the admission/knock
flow (ADR 0021).

- **Human ids + URLs**: a session id is `xxx-xxx-xxx` (three lowercase triples,
  `Store.freshIDLocked`, `session.ValidID`); every session lives at `<base>/<sid>`
  (`GET /{sid}` → dashboard, `sessionPage` 404s a mis-shaped id). The 16-hex id and
  the `#s=<sid>&t=<token>` deep link are gone.
- **Token gates access; password is its human alternative** (fixes the dead
  password): `JoinURL` is `…/<sid>#t=<token>` for a public session, `…/<sid>` (id
  only) for a password session. The dashboard reads the sid from the path, keeps
  the token in per-session storage (not the URL bar), and for a token-less id
  probes `POST /join` (401 → password form, 404 → needs-its-link/missing). Snapshot
  gains `has_password`.
- **Home**: create (Join password + Joiners-admin only — Label dropped from the
  form) plus a **Join a session** box (id → `<base>/<sid>`).
- **Layout**: a **Session** panel (id, a labelled Participants list, End/Delete,
  +1 h) separate from the beam cards.

Go gates green under `-race` (id format, routing, join_url, the tokens-never-logged
split); web tsc/eslint/vitest green (46). Verified in-browser: create → path URL +
token-in-fragment stripped to storage; a password session's id-only link → password
prompt → join; a public id without its token → "needs its link"; Participants list.

- **Stage 2 — admission/knock** (done, ADR 0021): opening a public id without its
  token now goes to a knock flow — `POST …/knock {name?}` (public, rate-limited) →
  pending request keyed by address (a password/missing session 404s, so a bare id
  reveals nothing); the knocker polls `GET …/knock` (pending/admitted-with-token/
  denied); a session admin `POST …/knock/{id} {decision}` admits (poll returns the
  token) or denies. Snapshot carries `knocks:[{id,name,at}]` (no address). The
  dashboard shows Requests-to-join (Admit/Deny) and the knocker a
  Waiting-to-be-let-in screen. Go `-race` green (knock/admit/deny + password-404);
  web green. Verified in-browser end-to-end: knock gate → waiting → admin admits →
  the browser is let into the dashboard.

- **Phase 11 complete.**

## Phase 12 — dark UI revamp (done)

A presentation-only revamp — no protocol, API, or lifecycle change, so no ADR.
The web UI is now one committed dark theme: near-black ground (`#0b0d10`), teal
accent (`#35d0c0`), system sans with mono for machine text, and inline stroke
symbols throughout (no icon font, no new runtime dep — CLAUDE.md holds).

- **Design canvas** authored with Claude Design (Home, Dashboard, Scanner, Admin,
  States, Tokens) as the reference for the build; kept out of the repo.
- **`web/src/shared/icons.ts`** (new): an `icon(name)` helper returning inline
  SVG (`currentColor`, 1.75 stroke) — the single symbol set.
- **`web/src/shared/style.css`** rewritten: dark tokens + every component (nav,
  cards, sections, pills/badges, participants, knocks, beams, verdicts, the scan
  HUD, the admin table). `[hidden]` made authoritative so the flex/grid rules do
  not resurrect toggled controls.
- **Tower** is now a two-column dashboard (share + session panel | beams): the
  status render splits across a left `#place` and right `#status`, with a
  `mode-home`/`mode-dash` switch on `#app`; the home/gate screens stay a single
  centred column. Scanner HUD reshaped (status line + state pill, mono stats,
  frosted controls); admin reskinned (sessions + live config). Symbols on every
  action.
- Verified in-browser against the mockups: Home, Dashboard (create → QR/link,
  participants, End/Delete), Admin (sign-in → sessions + live config, token
  masked), Scanner HUD. Go `-race` + web tsc/eslint/vitest (46) green.
- **Responsive pass**: mobile-first (one column; the dashboard splits at ≥1000px),
  nav and content on one container, the share card's nested grids (which let the
  join URL overflow the card) made flex. The progress-bar `.bar` rule collided with
  the nav `<header class="bar">` and clipped it to 8px — removed.
- **Chunk marks** (`web/src/shared/chunks.ts`, tested): one thin tally tick per
  chunk, a row per page (as many as fit the width), a tappable minimap of page
  pills (grouped past 48), a pager line; the view follows the newest chunk unless
  a tap pins a page for 8 s. Replaces the square grid on the dashboard and the
  bar on the scanner; `drawBitmap` is gone. The scanner gained a square
  viewfinder with a darkened surround and corner brackets (visual guide only —
  the decoder still reads the whole frame). `airliftScan.demo(total, have)`
  previews the HUD without a camera.
- **Client per device** (ADR 0022): `RegisterClient(addr, name, admin, resume)`
  mints per registration and resumes only a same-address id; the dashboard sends
  its stored id, the scan link carries `&c=<client_id>`, and a session-admin
  eviction of a same-address client drops just that client. Two devices behind
  one NAT are now two participants. `EvictClientByID(cid, byAddr)`.
- **Nav**: the brand sits at one place on every page; End / Delete moved into a
  power menu in the nav (session admins only).
- **Beam player** revamped (`internal/beam/player.go`): black page, the QR alone
  bright on a white quiet-zone tile, on-screen controls (step, pause, fps, size,
  fullscreen, hide chrome) with keys, responsive; still one offline file.

- **Scanner polish**: completion also fires on the frames reply's `completed_beams`
  (a small beam is READY before its first snapshot — the SSE edge never came); a
  reopened scanner ignores beams already finished when it opened (and ones it
  dismissed) so it waits clean; a persistent ✕ closes the tab; "already received"
  hint when the loop on screen is a known beam. Chunk marks: one fixed pitch,
  minimap + pager always, identical on scanner and dashboard. Home: a Create /
  Join switch, one card at a time.

- **Motion** (`web/src/shared/motion.ts` + tokens in style.css): one easing and
  three durations; cards rise into a view once (and a beam card the first time
  its bid appears — never on the per-second rebuild); the home switch is a pill
  whose thumb slides without a re-render; the power menu pops; buttons press;
  inputs ring on focus; ticks light in; the scanner overlay fades and its
  viewfinder corners breathe while waiting. `prefers-reduced-motion` honoured.
  Canvas: a Motion spec board (Version 6).
- **Landing + docs**: the home is a landing around the Create / Join card — hero,
  how-it-works, and *Get airlift* (GitHub link, clone by SSH / HTTPS / ZIP with
  copy buttons, per-platform release binaries, `go install`), with the tower's
  version in the footer. A **docs page** at `/docs` (`web/docs.html`, static HTML
  + `web/src/docs/main.ts` for copy buttons and the live TOC) walks through
  install → make a beam → show it → receive it → sessions → tips → reference, with
  real screenshots captured headlessly from the running tower
  (`web/public/docs/*.webp`, embedded in the binary; `GET /docs` and `/docs/`
  routes). The repo is public; releases are `airlift-<os>-<arch>` from `v*` tags.

- **Phase 12 complete.**

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
- The per-session `max_age` override deferred from Phase 7 shipped in Phase 9 as a
  session-admin **+1 h** grant (`POST …/max-age`, ADR 0018), not an airlift-admin
  override.

## Open questions

- Fountain auto-threshold is `FountainThreshold = 24` chunks; tune once real
  phone runs show where sequential stops being snappy enough.
- Whether Go's gzip ever crosses a chunk boundary on some toolchain and changes
  N for the multi fixture is guarded by the fountain vector test, which fails
  loudly if the packet count diverges.
