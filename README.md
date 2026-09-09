# airlift

Optical file transfer out of an air-gapped machine.

A sender inside the air gap renders a file as an animated QR loop on its
monitor. A phone on the operator's LAN scans the loop and relays decoded
frames to `airlift-tower`, a single Go binary on the laptop, which reassembles
the file, verifies it hash by hash, unpacks it if it is a
[`repobundle`](tools/repobundle.py), and serves the result to a dashboard and
to disk.

Status: **Phase 2 (tower core)**. Sender and tower work end to end through
`--replay`; the phone page and dashboard are next. See
[`STATUS.md`](STATUS.md) and [`docs/BUILD-PLAN.md`](docs/BUILD-PLAN.md).

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
every session, the join link with a terminal QR code for the phone. First
run on a phone: proceed through the certificate warning once, open
`/ca.crt`, install it as a CA certificate; after that there are no warnings
(the phone walkthrough arrives with the web UI in Phase 3). Flags: `--bind`,
`--port`, `--cert`/`--key` for mkcert users, `--ttl`, `--ca-dir`, and
`--session` to open a session at start for headless use.

Dev loop without a camera:

```bash
./bin/airlift-tower --dest /tmp/airlift-out --replay sender/testdata/vectors.json --drop 0.2
```

`--replay` accepts a frames dump from `airlift.py frames`, or any file, which
it encodes on the fly. `--rate`, `--drop`, `--shuffle`, `--passes` and
`--seed` shape the simulated scanner. `make replay` runs the line above.

## Developing

Requires Go 1.26+, Node 20+, Python 3.9+ with [`uv`](https://docs.astral.sh/uv/),
and [`pre-commit`](https://pre-commit.com/).

```bash
make setup        # npm ci, uv sync, pre-commit install
make lint test    # everything the pre-commit gate runs
make tower        # builds web/dist then bin/airlift-tower
```
