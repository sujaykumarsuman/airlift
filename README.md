# airlift

Optical file transfer out of an air-gapped machine.

A sender inside the air gap renders a file as an animated QR loop on its
monitor. A phone on the operator's LAN scans the loop and relays decoded
frames to `airlift-tower`, a single Go binary on the laptop, which reassembles
the file, verifies it hash by hash, unpacks it if it is a
[`repobundle`](tools/repobundle.py), and serves the result to a dashboard and
to disk.

Status: **Phase 3 (web UI)**. Sender, tower, dashboard and phone page are
built and verified end to end without a camera; the run on real hardware is
the last step. See [`STATUS.md`](STATUS.md) and
[`docs/BUILD-PLAN.md`](docs/BUILD-PLAN.md).

## Pieces

| Piece | Where it runs | What it is |
| --- | --- | --- |
| `sender/airlift.py` | air-gapped machine | Python 3.9+, one file, depends only on `segno`. Emits one self-contained HTML player. |
| `airlift-tower` | operator's laptop | Go, one static binary. Sessions, decode, verification, bundle unpack, TLS, web UI. |
| `web/` | phone and laptop browsers | `scan` (camera → relay) and `tower` (dashboard), embedded into the binary. |

Design: [`docs/PROTOCOL.md`](docs/PROTOCOL.md), [`docs/API.md`](docs/API.md),
[`docs/adr/`](docs/adr/).

## Sender (inside the air gap)

Copy [`sender/airlift.py`](sender/airlift.py) in. It is one file; it needs
Python 3.9+ and the pure-Python `segno` package, which can be vendored next to
it. Then:

```bash
python3 repobundle.py pack --format base64 --out repo-bundle.txt
```

```bash
python3 airlift.py beam --in repo-bundle.txt --out beam.html
```

Open `beam.html` in any browser, make it full-screen, and point the phone at
it. `beam` prints the chunk count, the QR version, the compression ratio and
the seconds per pass. Tuning: `--chunk` (payload bytes per frame, default
600, at most 2242 at ECC M), `--ecc L|M|Q|H`, `--fps`, `--manifest-every`.
Keys in the player: space pause · ←/→ step · +/- fps · f fullscreen.

`airlift.py frames` dumps the frames as JSON and `airlift.py decode` rebuilds
the file from such a dump, no camera involved; both need only the standard
library.

## Tower (on the laptop)

```bash
make tower
```

```bash
./bin/airlift-tower --dest ~/airlift-in
```

It binds the LAN address on port 8443, prints the dashboard URL and, for
every session, the join link with a terminal QR code for the phone. Flags:
`--bind`, `--port`, `--cert`/`--key` for mkcert users, `--ttl`, `--ca-dir`,
and `--session` to open a session at start for headless use.

Open the dashboard on the laptop, press **Create session**, and point the
phone's camera app at the QR code it shows. The phone opens the scan page,
asks for the camera, and relays what it decodes; the dashboard fills in
live and, once every hash matches, offers the downloads and names the
`--dest` path it wrote.

### First run on a phone

The tower serves HTTPS from a certificate authority it created on first
start, so the phone trusts nothing yet:

1. Open the join link; the browser shows a certificate warning. Proceed
   through it once (Chrome: Advanced → Proceed).
2. On the scan page, or at `https://<tower>:8443/ca.crt`, open `/ca.crt`
   and install it as a **CA certificate** (Android: Settings → Security →
   Encryption & credentials → Install a certificate → CA certificate;
   iOS: install the profile, then enable full trust under Settings → General
   → About → Certificate Trust Settings).
3. That is it for this phone: no warnings on any later run, and the tower's
   IP changing with DHCP does not matter because every run's certificate is
   signed by the same CA.

Android shows a persistent "network may be monitored" notice while a user
CA is installed; that is all it means, and removing the certificate ends it.
If you already use [mkcert](https://github.com/FiloSottile/mkcert), run the
tower with `--cert`/`--key` instead and skip the above.

Dev loop without a camera:

```bash
./bin/airlift-tower --dest /tmp/airlift-out --replay sender/testdata/vectors.json --drop 0.2
```

`--replay` accepts a frames dump from `airlift.py frames`, or any file, which
it encodes on the fly. `--rate`, `--drop`, `--shuffle`, `--passes` and
`--seed` shape the simulated scanner. `make replay` runs the line above.

To watch it on the dashboard instead, create a session there and feed that
session on the running tower with `--into` and its join link:

```bash
./bin/airlift-tower --replay sender/testdata/vectors.json --into 'https://192.168.1.10:8443/s/SID#t=TOKEN'
```

## Web UI development

```bash
npm --prefix web run dev
```

`vite dev` serves the two entries with hot reload and proxies `/api` and
`/ca.crt` to a running tower (`AIRLIFT_TOWER`, default
`https://127.0.0.1:8443`). It uses a self-signed certificate so a phone on
the LAN gets a secure context for the camera: run `npm --prefix web run
dev:lan` to expose it and open `https://<laptop>:5173/`. On `localhost`,
`AIRLIFT_HTTP=1` turns TLS off; localhost is a secure context regardless.
The dashboard builds join links for its own origin in dev, so the phone
talks to Vite, which forwards to the tower. Camera and decoder only exist
on hardware; everything else is unit-tested, and the scan page exposes
`window.airliftScan.inject([...frames])` to push decoded strings by hand.

## Developing

Requires Go 1.26+, Node 20+, Python 3.9+ with [`uv`](https://docs.astral.sh/uv/),
and [`pre-commit`](https://pre-commit.com/).

```bash
make setup        # npm ci, uv sync, pre-commit install
make lint test    # everything the pre-commit gate runs
make tower        # builds web/dist then bin/airlift-tower
```
