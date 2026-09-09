#!/usr/bin/env python3
"""
airlift.py — render a file as an animated QR loop for optical transfer out of
an air-gapped machine.

Subcommands (Phase 1):
  beam    --in FILE --out beam.html   gzip → chunk → frames → base45 → QR → HTML
  frames  --in FILE --seed N --out frames.json
  decode  --frames frames.json --out FILE

Runs inside the air gap. Python 3.9+. Only dependency: segno (pure Python).
Wire format and loop schedule: docs/PROTOCOL.md.
"""

from __future__ import annotations

import argparse
import sys

__version__ = "0.0.0"


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="airlift", description=__doc__.split("\n\n")[0].strip())
    ap.add_argument("--version", action="version", version=f"airlift {__version__}")
    ap.add_subparsers(dest="cmd")
    ap.parse_args(argv)
    print("airlift: not implemented yet (Phase 1)", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main())
