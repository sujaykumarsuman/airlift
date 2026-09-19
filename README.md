# airlift

Optical file transfer out of an air-gapped machine.

A machine inside the air gap renders a file as an animated QR loop on its
monitor (the *beam*). A phone on the operator's LAN scans the loop and relays
decoded frames to the *tower*, and the tower reassembles the file, verifies it
hash by hash, unpacks it if it is a [`repobundle`](docs/BUNDLE.md), and serves
the result to a dashboard for download. One static Go binary, `airlift`, with
two commands: `beam` inside the air gap, `tower` on the laptop.

Status: **hosted and released.** The tower runs behind Caddy TLS on a VPS at
[projects.sujaykumar.dev/airlift](https://projects.sujaykumar.dev/airlift)
([`docs/HOSTING.md`](docs/HOSTING.md)); the current release is **v0.1.1** (`v*`
tags build the binaries) and the repo is public. The tower is plain HTTP behind a
TLS-terminating proxy, config-driven from `~/.airlift` (ADR 0012); multi-beam
sessions, clients (one per device, ADR 0022), the session lifecycle and the admin
surface (`/admin`, gated by `admin_token`, with live config overrides) are in
place. Each session has a human id (`qkf-mzt-bwp`) and lives at `…/<id>`; a
public session's link carries its token, a password session's does not (you
enter the password) — ADR 0020; someone with only the id can knock and be
admitted (ADR 0021). A session dropped for inactivity is suspended and reopened
by simply opening its link (ADR 0018). The UI is a dark, symbol-led design with a
landing page (downloads, clone commands) and a docs walkthrough with real
screenshots. Plan and history:
[`prompts/002-go-cli-and-hosting.md`](prompts/002-go-cli-and-hosting.md),
[`STATUS.md`](STATUS.md).

**Using it:** the hosted tower's home page has the downloads and clone commands,
and a walkthrough with screenshots lives at
[projects.sujaykumar.dev/airlift/docs](https://projects.sujaykumar.dev/airlift/docs)
(served by the tower itself from `web/docs.html`).

## Install

Only the air-gapped machine needs the binary (to make beams); the tower side is
hosted. Pick one:

- **Release binary** — from the
  [latest release](https://github.com/sujaykumarsuman/airlift/releases/latest):
  `airlift-darwin-arm64`, `airlift-darwin-amd64`, `airlift-linux-amd64`,
  `airlift-linux-arm64`, `airlift-windows-amd64.exe` (+ `SHA256SUMS`). Make it
  executable and put it on your path; on macOS, `xattr -d com.apple.quarantine`
  if Gatekeeper objects.
- **Go** — `go install github.com/sujaykumarsuman/airlift/cmd/airlift@latest`.
- **Source** — clone, then `make airlift` (Go 1.26 + Node) → `bin/airlift`.

`airlift --help` lists the two commands; the hosted tower's home page carries the
same links and a walkthrough at `/docs`.

## The two commands

```
airlift beam PATH [PATH...]          bundle a folder/file(s) into a named QR page and open it
airlift beam PATH [PATH...] -s LINK  or send it straight to a session (not air-gapped)
airlift tower                        host the server that scanners relay to
```

Bundling, the frame codec, QR rendering, reassembly and the camera-free dev
loop are all internal — the binary exposes only what a transfer needs.
Receiving-side downloads happen in the tower's dashboard, not on the command
line.

## Beam (inside the air gap)

```bash
airlift beam .
```

That bundles the current folder (git-aware, so `.gitignore` is respected),
writes a self-contained `<name>.html`, and opens it in your browser. Make it
full-screen and point the phone at it.

- **A folder** is bundled and named after the folder.
- **One file** is sent as-is, named after the file.
- **Several files** are bundled and need a name: `--name NAME`, a `name:` line
  in a `--files-from LIST`, or you are prompted for one.

Every beam has a name, which the tower shows and which lets one session carry
several beams. `beam` prints the chunk count, QR version, compression ratio and
loop timing. Tuning flags: `--chunk` (payload bytes per frame; the default is
what QR version 30 holds at `--ecc`, 1311 at M; at most 2712), `--ecc L|M|Q|H`, `--fps` (default 10),
`--manifest-every`, `--name`, `--format auto|text|base64` (auto bundles a
folder as text when every file is text — about 30 % less gzip than base64 —
and falls back to base64 when a file is binary or holds a boundary marker),
`--mode auto|sequential|fountain` (the layout; auto picks by size, below),
`--out FILE`, `--no-open`, `--seed`. The player is a
black page with only the QR bright (on a white quiet-zone tile) and on-screen
controls; keys: space pause · ←/→ step · +/- fps · [/] size · f fullscreen ·
h hide chrome. The page carries the frames as text and encodes each QR itself,
so a beam is about 1.5 × the bytes of its frames — roughly 1.5 × the gzip for
a sequential beam and 2 × for a fountain one (a 7 MB file makes a 14 MB page).

### Straight to a session (not air-gapped)

On a machine that has a network, skip the page, the screen and the camera:

```bash
airlift beam ./my-repo -s 'https://projects.sujaykumar.dev/airlift/qkf-mzt-bwp#t=…'
```

`-s LINK` (`--to-session`) bundles and encodes exactly as for a page, joins the
session as a **sender** and asks its session admins for leave to upload: they
see the beam's name and size under **Upload requests** on the dashboard and
tap Approve or Deny (a sender that is itself a session admin is approved at
once), and the tower then takes exactly that beam and verifies it as it does
a scanned one. The CLI shows a countdown while it waits, a progress bar while
it sends, and the tower's verdicts at the end, then prints the dashboard link
to download from. The link decides how it gets in: a share link carries the
token; a password session's link makes it ask for the password (typed without
echo); a public session's bare id makes it knock and wait to be let in. Quote
the link — its `#` and `&` mean something to a shell. `--wait 5m` changes how
long it waits for an admin (default 3m). If the run ends early for any reason —
the wait runs out, Ctrl-C, a lost connection — the request is withdrawn and a
half-sent beam discarded; it exits 0 only when the beam is READY on the tower.
The approval is the admins' say over what the command line adds, not a lock:
anyone holding the session's token can already relay frames, as a phone's
scanner does. Run `airlift beam` with no arguments on a terminal and it asks
what to beam and where. The beam page stays the only way out of an air gap —
`-s` is for machines that are already connected (ADR 0023).

### Robust by default

Small payloads ship the chunks in order and repeat. Larger ones automatically
switch to an LT fountain code: any ~1.2×N distinct frames rebuild the file, so
loss only delays completion by the frames lost (a plain sequential loop waits a
whole pass for every straggler), and a second phone on the same session roughly
halves the time. By default `beam` picks the layout from the payload size
(fountain from 24 chunks) and prints which it used; `--mode sequential` or
`--mode fountain` overrides it — sequential for the smallest page and a
predictable pass, fountain when frames get lost or two phones are scanning.

### Tuning

Throughput is chunk bytes × decoded frames per second. Bigger QR versions
carry more per frame but need more pixels per module on the camera, and a
phone decodes small symbols more reliably. `--version-target V` picks the
largest chunk for a version; these are the numbers at ECC M:

| QR version | modules | bytes/frame | KB/s at 10 fps | KB/s at 15 fps | 1 MB gzip at 10 fps |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 10 | 57×57 | 189 | 1.8 | 2.8 | 9.2 min |
| 15 | 77×77 | 382 | 3.7 | 5.6 | 4.6 min |
| 20 | 97×97 | 628 | 6.1 | 9.2 | 2.8 min |
| 25 | 117×117 | 949 | 9.3 | 13.9 | 1.8 min |
| 30 | 137×137 | 1311 | 12.8 | 19.2 | 1.3 min |
| 40 | 177×177 | 2242 | 21.9 | 32.8 | 0.8 min |

The default is 1311 bytes (version 30, 137×137 modules) at 10 fps. The scanner
decodes only what is inside its viewfinder, so fill the square; its stats line
shows decoded/s against tries/s. If it decodes every frame, raise `--fps` with
the `+` key until it starts missing, then back off — 12 and 15 sit cleanly on a
60 Hz screen, 8 does not; if it misses at 10 fps, drop to 5 or to
`--version-target 25`, or move the phone closer. Larger modules matter more
than more of them. `--ecc L` gains about a quarter more capacity at the cost of
glare tolerance; `--ecc Q` or `H` the reverse (the default chunk follows the
ECC: a version-30 symbol either way).

### On the phone

Add the scan page to the home screen when the browser offers it: it installs
with an icon, opens full-screen, works offline once loaded, and reopens the
session it last joined. A torch button appears on cameras that have one; it
rarely helps with a monitor.

### Without the Go binary — a standalone Python beam

[`sender/airlift_beam.py`](sender/airlift_beam.py) writes the same beam page
using nothing but the Python 3 standard library — no `pip install`, no build —
so it runs on an air-gapped machine that has Python but not the compiled
`airlift`:

```bash
python3 airlift_beam.py ./my-repo
```

It is wire-compatible with `airlift beam`: identical frames (header, base45,
manifest, fountain) and the same in-browser QR encoder, so a tower reassembles
its beams exactly as it does the Go binary's. It mirrors the page flags
(`--name --format --mode --chunk --version-target --ecc --fps --manifest-every
--seed --out --no-open`). It only writes a page — the tower, the scanner and the
direct `-s` send stay the Go binary's job.

## Tower (on the laptop, or hosted)

The hosted tower is at [projects.sujaykumar.dev/airlift](https://projects.sujaykumar.dev/airlift);
to run your own:

```bash
make airlift
```

```bash
./bin/airlift tower
```

It reads (and, on first run, creates) `~/.airlift/config`, serves **plain HTTP**
on `listen` (default `127.0.0.1:8443`), and prints the dashboard URL plus, for
every session, the join link with a terminal QR code. Every config key is also
a flag; the common ones are `--public_url`, `--listen` and `--admin_token`, and
`--session` opens a session at start for headless use. See
[`docs/API.md`](docs/API.md) and ADR 0012.

Open the home page (the landing, with a **Create / Join** switch), press
**Create session**, and point the phone's camera app at the QR code it shows: the
QR opens the **shared session dashboard** on the phone (ADR 0019). There, tap **Scan a beam** to open the camera and point it at
a beam page; the phone relays what it decodes, the camera stops once the beam is
received, and the dashboard fills in live and, once every hash
matches, offers the downloads (raw file, or the unpacked tree as a zip) named
after the beam.

### Secure context and TLS

`getUserMedia` needs a secure context. On `localhost` (development) that is
satisfied without TLS. In production the tower runs **behind a reverse proxy
that terminates real TLS** and forwards to `listen` over plain HTTP;
`public_url` carries the public scheme, host and any path prefix (e.g.
`https://host/airlift`), which the tower injects as `<base href>` and uses for
every join link. There is no built-in certificate authority any more — the phone
simply trusts the proxy's certificate. The live tower runs on a single-node
**k3s** cluster, deployed by **GitOps** (Flux + Helm) with **Traefik +
cert-manager** TLS — see [`docs/HOSTING.md`](docs/HOSTING.md) and the
`sujaykumarsuman/infra` repo.

### Zero-hop variant

Android 14+ can act as a USB webcam (Settings → Connected devices → USB →
Webcam). Plug the phone into the laptop, run `airlift tower`, open the
dashboard *and* the scan link on the laptop itself, and pick the phone in the
scan page's camera selector; the same works with any external camera. Nothing
crosses the LAN.

## Web UI development

```bash
npm --prefix web run dev
```

`vite dev` serves the four entries (`/`, `/s/{sid}`, `/admin`, `/docs`) with hot
reload and proxies `/api` to a running tower (`AIRLIFT_TOWER`, default
`https://127.0.0.1:8443`). A self-signed
plugin is kept only to give a real phone on the LAN a secure context: run `npm
--prefix web run dev:lan` and open `https://<laptop>:5173/`; `AIRLIFT_HTTP=1`
turns it off for plain-HTTP `localhost` work. Camera and decoder only exist on
hardware; everything else is unit-tested, and
the scan page exposes `window.airliftScan.inject([...frames])` to push decoded
strings by hand.

## Developing

Requires Go 1.26+, Node 20+, and [`pre-commit`](https://pre-commit.com/).

```bash
make setup        # npm ci, pre-commit install
make lint test    # everything the pre-commit gate runs
make airlift      # builds web/dist then bin/airlift
make airlift-all  # cross-compiles the release binaries into bin/
make docs-shots   # regenerates the docs page's screenshots from the real app (headless Chrome)
make scan-e2e     # scans a real beam with the real scanner, no phone: player frames → MJPEG → headless Chrome's fake camera → tower READY
```

The binary is stamped with its version (`git describe --tags`, or the tag in the
release workflow) and reports it at `GET /api/info` and in the landing footer.
Releases: `git tag -a vX.Y.Z -m "…" && git push origin vX.Y.Z` — the workflow
builds and publishes the binaries and the GHCR image, and Flux auto-deploys the
image to the k3s cluster (see [`docs/HOSTING.md`](docs/HOSTING.md)).

`go test` covers the whole pipeline end to end — bundle a tree, beam it, relay
the frames through loss into a real tower, and check the restored tree — so no
camera or phone is needed to exercise the transfer.

## Licence

MIT — see [`LICENSE`](LICENSE). The hosted tower's terms of use and privacy
notes are at [projects.sujaykumar.dev/airlift/legal](https://projects.sujaykumar.dev/airlift/legal)
(`web/legal.html`): no accounts, nothing stored beyond a session's short life.
