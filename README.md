# airlift

Optical file transfer out of an air-gapped machine.

A sender inside the air gap renders a file as an animated QR loop on its
monitor. A phone on the operator's LAN scans the loop and relays decoded
frames to `airlift-tower`, a single Go binary on the laptop, which reassembles
the file, verifies it hash by hash, unpacks it if it is a
[`repobundle`](tools/repobundle.py), and serves the result to a dashboard and
to disk.

Status: **Phase 0 (scaffold)**. Nothing transfers yet. See
[`STATUS.md`](STATUS.md) and [`docs/BUILD-PLAN.md`](docs/BUILD-PLAN.md).

## Pieces

| Piece | Where it runs | What it is |
| --- | --- | --- |
| `sender/airlift.py` | air-gapped machine | Python 3.9+, one file, depends only on `segno`. Emits one self-contained HTML player. |
| `airlift-tower` | operator's laptop | Go, one static binary. Sessions, decode, verification, bundle unpack, TLS, web UI. |
| `web/` | phone and laptop browsers | `scan` (camera → relay) and `tower` (dashboard), embedded into the binary. |

Design: [`docs/PROTOCOL.md`](docs/PROTOCOL.md), [`docs/API.md`](docs/API.md),
[`docs/adr/`](docs/adr/).

## Developing

Requires Go 1.26+, Node 20+, Python 3.9+ with [`uv`](https://docs.astral.sh/uv/),
and [`pre-commit`](https://pre-commit.com/).

```bash
make setup        # npm ci, uv sync, pre-commit install
make lint test    # everything the pre-commit gate runs
make tower        # builds web/dist then bin/airlift-tower
```
