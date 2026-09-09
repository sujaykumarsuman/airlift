# airlift

Optical file transfer out of an air-gapped machine.

A machine inside the air gap renders a file as an animated QR loop on its
monitor (the *beam*). A phone on the operator's LAN scans the loop and relays
decoded frames to the *tower*, and the tower reassembles the file, verifies it
hash by hash, unpacks it if it is a [`repobundle`](docs/BUNDLE.md), and serves
the result to a dashboard and to disk. One static Go binary, `airlift`, does
all of it — the beam inside the air gap, the tower on the laptop.

Status: **Phase 5 (one binary)**. The Python sender is retired; `airlift`
packs, beams, hosts and replays. Everything is built and verified end to end
without a camera, fountain mode included; the runs on real hardware and the
hosted, multi-user tower are what remain. See [`STATUS.md`](STATUS.md) and
[`prompts/002-go-cli-and-hosting.md`](prompts/002-go-cli-and-hosting.md).

## The whole workflow

On the air-gapped machine, with just the `airlift` binary, in the repository
to move:

```bash
airlift beam --root . --fountain --out beam.html
```

That packs the tree into a base64 repobundle, writes it next to the page, and
renders `beam.html`. Open it in any browser, make it full-screen.

On the laptop:

```bash
airlift tower --dest ~/airlift-in
```

Open the dashboard it prints, create a session, scan the join code with the
phone, then point the phone at `beam.html` on the air-gapped monitor. The
dashboard reaches `READY` when every hash matches; the unpacked tree is under
`~/airlift-in/<name>/` and downloadable as a zip. Without a phone, a laptop
with a webcam can run the scan page itself; see the zero-hop variant below.

## The binary

One command, `airlift`, with subcommands:

| Subcommand | What it does |
| --- | --- |
| `pack` | pack a folder or files into a repobundle text file |
| `unpack` | restore files from a repobundle, checking every sha256 |
| `beam` | write a self-contained HTML QR player for a file or a tree |
| `frames` | dump `{sender_session, manifest, frames}` as JSON |
| `decode` | reassemble a file from a frames dump |
| `tower` | host a session, decode relayed frames, verify and serve |
| `replay` | feed a frames dump into a tower session (dev loop, no camera) |

`pack`, `beam`, `frames` and `decode` need no network and run inside the air
gap. The web UI (`scan`, `tower`) is TypeScript, embedded into the binary and
served by the tower. Design: [`docs/PROTOCOL.md`](docs/PROTOCOL.md),
[`docs/BUNDLE.md`](docs/BUNDLE.md), [`docs/API.md`](docs/API.md),
[`docs/adr/`](docs/adr/).

## Beam (inside the air gap)

`airlift beam` takes either a single file (`--in FILE`) or a tree
(`--root DIR [PATHS...]`, packed to a base64 bundle first, written next to the
page unless `--no-bundle`):

```bash
airlift beam --in repo-bundle.txt --out beam.html
```

Open `beam.html`, make it full-screen, and point the phone at it. `beam`
prints the chunk count, the QR version, the compression ratio and the seconds
per pass. Tuning: `--chunk` (payload bytes per frame, default 600, at most
2242 at ECC M), `--ecc L|M|Q|H`, `--fps`, `--manifest-every`, `--seed`.
Keys in the player: space pause · ←/→ step · +/- fps · f fullscreen.

`airlift frames` dumps the frames as JSON and `airlift decode` rebuilds the
file from such a dump, no camera involved. `airlift pack` and `airlift unpack`
are the bundle stage on their own.

### Sequential or fountain

The default beam shows the chunks in order and repeats; a missed frame costs
another pass of the loop, so long transfers spend most of their time waiting
for stragglers. `--fountain` shows LT-coded packets instead: any roughly
1.2 N distinct packets rebuild the file, so loss only delays completion by the
frames lost, and a second phone on the same session halves the time. Fountain
beams carry about `N + 3·√N·ln N` packets, so the HTML is larger and, for
small files, a pass is longer than the file warrants; use it for anything over
a few hundred chunks.

### Tuning

Throughput is chunk bytes × decoded frames per second. Bigger QR versions
carry more per frame but need more pixels per module on the camera, and a
phone decodes small symbols more reliably. `--version-target V` picks the
largest chunk for a version; these are the numbers at ECC M:

| QR version | modules | bytes/frame | KB/s at 8 fps | KB/s at 12 fps | 1 MB gzip at 8 fps |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 10 | 57×57 | 189 | 1.5 | 2.2 | 11.6 min |
| 15 | 77×77 | 382 | 3.0 | 4.5 | 5.7 min |
| 20 | 97×97 | 628 | 4.9 | 7.4 | 3.5 min |
| 25 | 117×117 | 949 | 7.4 | 11.1 | 2.3 min |
| 30 | 137×137 | 1311 | 10.2 | 15.4 | 1.7 min |
| 40 | 177×177 | 2242 | 17.5 | 26.3 | 1.0 min |

Start with the default (600 bytes, version 20) at 8 fps. If the phone decodes
every frame (its stats line shows the decode rate), raise `--fps` with the `+`
key until it starts missing, then back off; if it misses at 8 fps, try a
smaller version or move the phone closer. Larger modules matter more than more
of them. `--ecc L` gains ~15 % capacity at the cost of glare tolerance;
`--ecc Q` or `H` the reverse.

### Zero-hop variant

Android 14+ can act as a USB webcam (Settings → Connected devices → USB →
Webcam). Plug the phone into the laptop, run `airlift tower`, open the
dashboard *and* the scan link on the laptop itself, and pick the phone in the
scan page's camera selector; the same works with any external camera. Nothing
crosses the LAN.

### On the phone

Add the scan page to the home screen when the browser offers it: it installs
with an icon, opens full-screen, works offline once loaded, and reopens the
session it last joined. A torch button appears on cameras that have one; it
rarely helps with a monitor.

## Tower (on the laptop)

```bash
make airlift
```

```bash
./bin/airlift tower --dest ~/airlift-in
```

It binds the LAN address on port 8443, prints the dashboard URL and, for every
session, the join link with a terminal QR code for the phone. Flags: `--bind`,
`--port`, `--cert`/`--key` for mkcert users, `--ttl`, `--ca-dir`, and
`--session` to open a session at start for headless use.

Open the dashboard on the laptop, press **Create session**, and point the
phone's camera app at the QR code it shows. The phone opens the scan page,
asks for the camera, and relays what it decodes; the dashboard fills in live
and, once every hash matches, offers the downloads and names the `--dest` path
it wrote.

### First run on a phone

The tower serves HTTPS from a certificate authority it created on first start,
so the phone trusts nothing yet:

1. Open the join link; the browser shows a certificate warning. Proceed
   through it once (Chrome: Advanced → Proceed).
2. On the scan page, or at `https://<tower>:8443/ca.crt`, open `/ca.crt` and
   install it as a **CA certificate** (Android: Settings → Security →
   Encryption & credentials → Install a certificate → CA certificate; iOS:
   install the profile, then enable full trust under Settings → General →
   About → Certificate Trust Settings).
3. That is it for this phone: no warnings on any later run, and the tower's IP
   changing with DHCP does not matter because every run's certificate is
   signed by the same CA.

Android shows a persistent "network may be monitored" notice while a user CA
is installed; that is all it means, and removing the certificate ends it. If
you already use [mkcert](https://github.com/FiloSottile/mkcert), run the tower
with `--cert`/`--key` instead and skip the above.

(The hosted, HTTP-behind-a-proxy tower of prompt 002 replaces this local CA in
a later phase.)

Dev loop without a camera:

```bash
./bin/airlift replay testdata/vectors/vectors.json --dest /tmp/airlift-out --drop 0.2
```

`replay` accepts a frames dump from `airlift frames`, or any file, which it
encodes on the fly. `--rate`, `--drop`, `--shuffle`, `--passes` and `--seed`
shape the simulated scanner.

To watch it on the dashboard instead, create a session there and feed that
session on the running tower with `--into` and its join link:

```bash
./bin/airlift replay testdata/vectors/vectors.json --into 'https://192.168.1.10:8443/s/SID#t=TOKEN'
```

## Web UI development

```bash
npm --prefix web run dev
```

`vite dev` serves the two entries with hot reload and proxies `/api` and
`/ca.crt` to a running tower (`AIRLIFT_TOWER`, default
`https://127.0.0.1:8443`). It uses a self-signed certificate so a phone on the
LAN gets a secure context for the camera: run `npm --prefix web run dev:lan`
to expose it and open `https://<laptop>:5173/`. On `localhost`,
`AIRLIFT_HTTP=1` turns TLS off; localhost is a secure context regardless. The
dashboard builds join links for its own origin in dev, so the phone talks to
Vite, which forwards to the tower. Camera and decoder only exist on hardware;
everything else is unit-tested, and the scan page exposes
`window.airliftScan.inject([...frames])` to push decoded strings by hand.

## Developing

Requires Go 1.26+, Node 20+, and [`pre-commit`](https://pre-commit.com/).

```bash
make setup        # npm ci, pre-commit install
make lint test    # everything the pre-commit gate runs
make airlift      # builds web/dist then bin/airlift
make airlift-all  # cross-compiles the release binaries into bin/
```
