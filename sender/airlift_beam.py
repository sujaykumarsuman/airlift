#!/usr/bin/env python3
"""airlift beam — a standalone Python re-implementation of `airlift beam`.

Renders a file or folder as an animated QR loop in one self-contained HTML page,
byte-compatible on the wire with the Go `airlift beam`: a phone scans the loop
and relays the decoded frames to a tower, which reassembles and verifies the
result. This is a convenience alternative to the Go binary for anyone who has
Python 3 but not the compiled `airlift` — it needs nothing but the standard
library (the in-browser QR encoder `qrjs.js` is embedded), so it runs on an
air-gapped machine with a stock Python.

Pipeline (docs/PROTOCOL.md): input → gzip → chunk → frame → base45 → QR
(alphanumeric, ECC M) drawn in the browser from the frames' text and a fixed
per-version plan. A folder becomes a repobundle first (docs/BUNDLE.md).

Usage:
  airlift_beam.py PATH [PATH ...] [flags]

  PATH      a folder (git-aware bundle), one file (sent as-is), or several
            files (bundled — pass --name).

Flags mirror the Go beam's page options:
  --name NAME                     beam name (default: the folder or file name)
  --format auto|text|base64       bundle format (default auto)
  --mode auto|sequential|fountain frame layout (default auto)
  --chunk BYTES                   payload bytes per frame
  --version-target V              largest chunk that QR version V (1..40) holds
  --ecc L|M|Q|H                   QR error correction (default M)
  --fps N                         initial frames per second, 1..60 (default 10)
  --manifest-every K              re-insert the manifest every K frames (default 20)
  --seed N                        derive the sender id from N (reproducible output)
  --out FILE                      where to write the page (default <name>.html)
  --no-open                       do not open the page in a browser

Not a session participant and never talks to the network: it only writes a page.
The Go beam's --to-session (direct HTTP send) is out of scope here.
"""
from __future__ import annotations

import argparse
import base64
import bisect
import gzip
import hashlib
import html
import json
import math
import os
import random
import secrets
import struct
import subprocess
import sys
import webbrowser
import zlib

PROG = "airlift_beam.py"

# ---------------------------------------------------------------------------
# base45 (RFC 9285). The alphabet is exactly QR's alphanumeric character set.
# ---------------------------------------------------------------------------

B45_ALPHABET = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:"


def b45encode(data: bytes) -> str:
    """Two bytes → three characters (least significant first); a trailing byte → two."""
    out = []
    n = len(data)
    for i in range(0, n - 1, 2):
        v = (data[i] << 8) | data[i + 1]
        v, c = divmod(v, 45)
        e, d = divmod(v, 45)
        out.append(B45_ALPHABET[c] + B45_ALPHABET[d] + B45_ALPHABET[e])
    if n & 1:
        d, c = divmod(data[-1], 45)
        out.append(B45_ALPHABET[c] + B45_ALPHABET[d])
    return "".join(out)


# ---------------------------------------------------------------------------
# Frames: 18-byte big-endian header + payload (docs/PROTOCOL.md).
# ---------------------------------------------------------------------------

MAGIC = 0x414C  # "AL"
VERSION = 1
T_MANIFEST, T_DATA, T_FOUNTAIN = 0, 1, 2
HEADER = struct.Struct(">HBBIHHHI")  # magic ver type session seq total len crc32
HEADER_LEN = HEADER.size  # 18
MAX_CHUNKS = 0xFFFF
MAX_PACKETS = 0x10000  # fountain seeds are u16


def frame_text(ftype: int, session: int, seq: int, total: int, payload: bytes) -> str:
    """One frame as it travels: packed header + payload, base45-encoded."""
    crc = zlib.crc32(payload) & 0xFFFFFFFF
    raw = HEADER.pack(MAGIC, VERSION, ftype, session, seq, total, len(payload), crc) + payload
    return b45encode(raw)


# ---------------------------------------------------------------------------
# Manifest (payload of the MANIFEST frame): compact JSON, fixed key order.
# ---------------------------------------------------------------------------


def manifest_total(gz_size: int, chunk: int) -> int:
    """N, the number of source chunks: ceil(gz_size / chunk), at least 1."""
    return max(1, -(-gz_size // chunk))


def manifest_json(name: str, gz_size: int, gz_sha256: str, orig_size: int, orig_sha256: str, chunk: int) -> bytes:
    obj = {
        "name": name,
        "gz_size": gz_size,
        "gz_sha256": gz_sha256,
        "orig_size": orig_size,
        "orig_sha256": orig_sha256,
        "chunk": chunk,
    }
    return json.dumps(obj, separators=(",", ":"), ensure_ascii=True).encode("ascii")


# ---------------------------------------------------------------------------
# Fountain code: LT packets with a robust soliton degree distribution.
# ADR 0009 fixes every constant and operation; the Go decoder reproduces
# fountain_indices() exactly, so this is a cross-language contract.
# ---------------------------------------------------------------------------

FOUNTAIN_C = 0.1
FOUNTAIN_DELTA = 0.5
_TWO32 = 4294967296.0
_cdf_cache: dict[int, list[int]] = {}


def robust_soliton_cdf(n: int) -> list[int]:
    """Cumulative degree distribution over 1..n as thresholds scaled to 2^32.

    Plain left-to-right float arithmetic, no fused multiply-add, so Go and Python
    agree bit for bit (ADR 0009)."""
    cached = _cdf_cache.get(n)
    if cached is not None:
        return cached
    if n == 1:
        cdf = [1 << 32]
    else:
        r = FOUNTAIN_C * math.log(n / FOUNTAIN_DELTA) * math.sqrt(n)
        m = max(1, min(int(n / r), n))
        weights = [0.0] * (n + 1)
        weights[1] = 1.0 / n
        for i in range(2, n + 1):
            weights[i] = 1.0 / (i * (i - 1))
        for i in range(1, m):
            weights[i] += r / (i * n)
        if r > FOUNTAIN_DELTA:
            weights[m] += r * math.log(r / FOUNTAIN_DELTA) / n
        z = 0.0
        for i in range(1, n + 1):
            z += weights[i]
        cdf = []
        acc = 0.0
        for i in range(1, n + 1):
            acc += weights[i] / z
            cdf.append(min(1 << 32, math.floor(acc * _TWO32 + 0.5)))
        cdf[-1] = 1 << 32
    _cdf_cache[n] = cdf
    return cdf


def _xorshift32(x: int) -> int:
    x ^= (x << 13) & 0xFFFFFFFF
    x ^= x >> 17
    x ^= (x << 5) & 0xFFFFFFFF
    return x


def fountain_indices(seed: int, n: int) -> list[int]:
    """Source chunks XORed into fountain packet `seed` (a u16), in draw order."""
    cdf = robust_soliton_cdf(n)
    x = (seed * 2654435761 + 2654435769) & 0xFFFFFFFF
    if x == 0:
        x = 1
    for _ in range(4):
        x = _xorshift32(x)
    x = _xorshift32(x)
    degree = min(n, bisect.bisect_right(cdf, x) + 1)
    chosen: list[int] = []
    seen: set[int] = set()
    while len(chosen) < degree:
        x = _xorshift32(x)
        i = x % n
        if i not in seen:
            seen.add(i)
            chosen.append(i)
    return chosen


def default_packets(total: int) -> int:
    """Packets a fountain beam carries: N + max(48, ceil(3·√N·ln N)), u16-capped."""
    extra = max(48, math.ceil(3.0 * math.sqrt(total) * math.log(total)))
    return min(MAX_PACKETS, total + extra)


def _padded_chunks(blob: bytes, chunk: int, total: int) -> list[int]:
    return [
        int.from_bytes(blob[i * chunk : (i + 1) * chunk].ljust(chunk, b"\0"), "big")
        for i in range(total)
    ]


def fountain_payload(padded: list[int], chunk: int, seed: int) -> bytes:
    acc = 0
    for i in fountain_indices(seed, len(padded)):
        acc ^= padded[i]
    return acc.to_bytes(chunk, "big")


# ---------------------------------------------------------------------------
# Encode: input → gzip → chunk → [MANIFEST, DATA 0…N-1] or [MANIFEST, F0…F(K-1)]
# ---------------------------------------------------------------------------

FOUNTAIN_THRESHOLD = 24  # ModeAuto uses fountain at or above this many chunks
DEFAULT_CHUNK = 1311     # QR version 30 at ECC M (the Go default since 2026-09-14)
DEFAULT_FPS = 10
MAX_CHUNK = 2712         # largest chunk whose DATA frame stays under the wire cap


def sha256hex(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def compress(data: bytes) -> bytes:
    """gzip level 9 with a zeroed mtime; the receiver verifies against the
    gz_sha256 this manifest carries, so cross-tool byte-identity is not needed."""
    return gzip.compress(data, compresslevel=9, mtime=0)


def new_session(seed: int | None) -> int:
    """Sender session id: a random u32, or one derived from --seed for reproducible output."""
    if seed is None:
        return secrets.randbits(32)
    return random.Random(seed).getrandbits(32)


def _frames_from_blob(blob: bytes, orig_size: int, orig_sha256: str, name: str, chunk: int,
                      session: int, fountain: bool, packets: int | None = None):
    """The frame list from an already-gzipped blob. Returns (manifest_dict, frame_texts):
    texts[0] is the MANIFEST; texts[1:] are DATA 0…N-1 or FOUNTAIN 0…K-1."""
    if not 1 <= chunk <= 0xFFFF:
        raise ValueError(f"chunk must be 1..65535, got {chunk}")
    m = {
        "name": name,
        "gz_size": len(blob),
        "gz_sha256": sha256hex(blob),
        "orig_size": orig_size,
        "orig_sha256": orig_sha256,
        "chunk": chunk,
    }
    total = manifest_total(len(blob), chunk)
    if total > MAX_CHUNKS:
        raise ValueError(f"{total} chunks exceeds the u16 limit of {MAX_CHUNKS}; raise --chunk")
    frames = [frame_text(T_MANIFEST, session, 0, total, manifest_json(**m))]
    if fountain:
        k = default_packets(total) if packets is None else packets
        if not 1 <= k <= MAX_PACKETS:
            raise ValueError(f"fountain packets must be 1..{MAX_PACKETS}, got {k}")
        padded = _padded_chunks(blob, chunk, total)
        for seed in range(k):
            frames.append(frame_text(T_FOUNTAIN, session, seed, total, fountain_payload(padded, chunk, seed)))
    else:
        for i in range(total):
            frames.append(frame_text(T_DATA, session, i, total, blob[i * chunk : (i + 1) * chunk]))
    return m, frames


def encode(data: bytes, name: str, chunk: int, session: int, fountain: bool, packets: int | None = None):
    """gzip then frame `data`. Returns (manifest_dict, frame_texts)."""
    return _frames_from_blob(compress(data), len(data), sha256hex(data), name, chunk, session, fountain, packets)


def schedule(total: int, every: int) -> list[int]:
    """Loop order as indices into the frame list (0 = MANIFEST, k+1 = frame k):
    [M, F0 … F(total-1)] with M re-inserted after every `every` frames."""
    if every < 1:
        every = 1
    order = []
    for i in range(total):
        if i % every == 0:
            order.append(0)
        order.append(i + 1)
    return order


# ---------------------------------------------------------------------------
# QR plan: a per-version, per-level description the in-page encoder (qrjs.js)
# needs to render any frame at one fixed symbol — the size, the block structure,
# and two bitmaps: which modules are function/format patterns (occ) and which of
# the function modules are dark (base). Ported from rsc.io/qr/coding's plan, with
# which it is byte-for-byte identical (validated against Go across all versions).
# ---------------------------------------------------------------------------

QUIET_ZONE = 4  # white modules around the symbol (ISO/IEC 18004)
MAX_FRAME_TEXT = 4096  # the tower rejects longer base45 strings
_LEVELS = {"L": 0, "M": 1, "Q": 2, "H": 3}

# vtab[v] = (apos, astride, bytes, pattern, [(nblock, check) for L, M, Q, H]).
# Transcribed from rsc.io/qr/coding/qr.go; the block structure and version info
# are the ISO/IEC 18004 tables.
_VTAB = [
    None,
    (100, 100, 26, 0x0, [(1, 7), (1, 10), (1, 13), (1, 17)]),
    (16, 100, 44, 0x0, [(1, 10), (1, 16), (1, 22), (1, 28)]),
    (20, 100, 70, 0x0, [(1, 15), (1, 26), (2, 18), (2, 22)]),
    (24, 100, 100, 0x0, [(1, 20), (2, 18), (2, 26), (4, 16)]),
    (28, 100, 134, 0x0, [(1, 26), (2, 24), (4, 18), (4, 22)]),
    (32, 100, 172, 0x0, [(2, 18), (4, 16), (4, 24), (4, 28)]),
    (20, 16, 196, 0x7c94, [(2, 20), (4, 18), (6, 18), (5, 26)]),
    (22, 18, 242, 0x85bc, [(2, 24), (4, 22), (6, 22), (6, 26)]),
    (24, 20, 292, 0x9a99, [(2, 30), (5, 22), (8, 20), (8, 24)]),
    (26, 22, 346, 0xa4d3, [(4, 18), (5, 26), (8, 24), (8, 28)]),
    (28, 24, 404, 0xbbf6, [(4, 20), (5, 30), (8, 28), (11, 24)]),
    (30, 26, 466, 0xc762, [(4, 24), (8, 22), (10, 26), (11, 28)]),
    (32, 28, 532, 0xd847, [(4, 26), (9, 22), (12, 24), (16, 22)]),
    (24, 20, 581, 0xe60d, [(4, 30), (9, 24), (16, 20), (16, 24)]),
    (24, 22, 655, 0xf928, [(6, 22), (10, 24), (12, 30), (18, 24)]),
    (24, 24, 733, 0x10b78, [(6, 24), (10, 28), (17, 24), (16, 30)]),
    (28, 24, 815, 0x1145d, [(6, 28), (11, 28), (16, 28), (19, 28)]),
    (28, 26, 901, 0x12a17, [(6, 30), (13, 26), (18, 28), (21, 28)]),
    (28, 28, 991, 0x13532, [(7, 28), (14, 26), (21, 26), (25, 26)]),
    (32, 28, 1085, 0x149a6, [(8, 28), (16, 26), (20, 30), (25, 28)]),
    (26, 22, 1156, 0x15683, [(8, 28), (17, 26), (23, 28), (25, 30)]),
    (24, 24, 1258, 0x168c9, [(9, 28), (17, 28), (23, 30), (34, 24)]),
    (28, 24, 1364, 0x177ec, [(9, 30), (18, 28), (25, 30), (30, 30)]),
    (26, 26, 1474, 0x18ec4, [(10, 30), (20, 28), (27, 30), (32, 30)]),
    (30, 26, 1588, 0x191e1, [(12, 26), (21, 28), (29, 30), (35, 30)]),
    (28, 28, 1706, 0x1afab, [(12, 28), (23, 28), (34, 28), (37, 30)]),
    (32, 28, 1828, 0x1b08e, [(12, 30), (25, 28), (34, 30), (40, 30)]),
    (24, 24, 1921, 0x1cc1a, [(13, 30), (26, 28), (35, 30), (42, 30)]),
    (28, 24, 2051, 0x1d33f, [(14, 30), (28, 28), (38, 30), (45, 30)]),
    (24, 26, 2185, 0x1ed75, [(15, 30), (29, 28), (40, 30), (48, 30)]),
    (28, 26, 2323, 0x1f250, [(16, 30), (31, 28), (43, 30), (51, 30)]),
    (32, 26, 2465, 0x209d5, [(17, 30), (33, 28), (45, 30), (54, 30)]),
    (28, 28, 2611, 0x216f0, [(18, 30), (35, 28), (48, 30), (57, 30)]),
    (32, 28, 2761, 0x228ba, [(19, 30), (37, 28), (51, 30), (60, 30)]),
    (28, 24, 2876, 0x2379f, [(19, 30), (38, 28), (53, 30), (63, 30)]),
    (22, 26, 3034, 0x24b0b, [(20, 30), (40, 28), (56, 30), (66, 30)]),
    (26, 26, 3196, 0x2542e, [(21, 30), (43, 28), (59, 30), (70, 30)]),
    (30, 26, 3362, 0x26a64, [(22, 30), (45, 28), (62, 30), (74, 30)]),
    (24, 28, 3532, 0x27541, [(24, 30), (47, 28), (65, 30), (77, 30)]),
    (28, 28, 3706, 0x28c69, [(25, 30), (49, 28), (68, 30), (81, 30)]),
]

# Per-cell markers while building the function-pattern layout.
_FREE, _FUNC, _FORMAT = 0, 1, 2


def _count_bits(version: int) -> int:
    return 9 if version <= 9 else 11 if version <= 26 else 13


def _data_bytes(version: int, level: int) -> int:
    _, _, total, _, levels = _VTAB[version]
    nblock, check = levels[level]
    return total - nblock * check


def alpha_fits(chars: int, version: int, level: int) -> bool:
    """Whether `chars` alphanumeric characters fit `version` at `level` — the same
    capacity model the encoder enforces (4 + count + ⌈11·chars/2⌉ ≤ data bits)."""
    bits = 4 + _count_bits(version) + (11 * chars + 1) // 2
    return bits <= _data_bytes(version, level) * 8


def text_len(frame_bytes: int) -> int:
    """base45 characters for a frame of the given byte length."""
    return (frame_bytes // 2) * 3 + (2 if frame_bytes % 2 else 0)


def min_version(nchars: int, level: int) -> int:
    for v in range(1, 41):
        if alpha_fits(nchars, v, level):
            return v
    raise BeamError(
        f"a {nchars}-character frame does not fit QR version 40 at ECC "
        f"{'LMQH'[level]}; reduce --chunk"
    )


def chunk_for_version(version: int, level: int) -> int:
    """Largest --chunk whose full DATA frame fits `version` at `level` and stays
    under the wire cap. The Python twin of the Go ChunkForVersion."""
    if not 1 <= version <= 40:
        raise BeamError("--version-target must be 1..40")
    lo, hi = 0, MAX_CHUNK
    while lo < hi:
        mid = (lo + hi + 1) // 2
        chars = text_len(HEADER_LEN + mid)
        if chars <= MAX_FRAME_TEXT and alpha_fits(chars, version, level):
            lo = mid
        else:
            hi = mid - 1
    if lo < 1:
        raise BeamError(f"QR version {version} at ECC {'LMQH'[level]} cannot hold even a 1-byte chunk")
    return lo


def _vplan(version: int) -> tuple[list[list[int]], list[list[bool]], int]:
    """The function-pattern layout for `version`: a role grid and a dark grid over
    n×n modules (n = 17 + 4·version). A faithful port of rsc.io/qr/coding vplan
    followed by its format-cell placement (fplan); the mask and data placement do
    not touch these cells, so occ/base depend only on the version."""
    n = 17 + 4 * version
    role = [[_FREE] * n for _ in range(n)]
    dark = [[False] * n for _ in range(n)]

    def put(y: int, x: int, r: int, black: bool) -> None:
        role[y][x] = r
        dark[y][x] = black

    # Timing strips in row/column 6 (overwritten by the boxes at the corners).
    for i in range(n):
        put(i, 6, _FUNC, i % 2 == 0)
        put(6, i, _FUNC, i % 2 == 0)

    def pos_box(x: int, y: int) -> None:  # x = column, y = row (upper-left)
        for dy in range(7):
            for dx in range(7):
                black = dx in (0, 6) or dy in (0, 6) or (2 <= dx <= 4 and 2 <= dy <= 4)
                put(y + dy, x + dx, _FUNC, black)
        for dy in range(-1, 8):  # white separator border
            if 0 <= y + dy < n:
                if x > 0:
                    put(y + dy, x - 1, _FUNC, False)
                if x + 7 < n:
                    put(y + dy, x + 7, _FUNC, False)
        for dx in range(-1, 8):
            if 0 <= x + dx < n:
                if y > 0:
                    put(y - 1, x + dx, _FUNC, False)
                if y + 7 < n:
                    put(y + 7, x + dx, _FUNC, False)

    pos_box(0, 0)
    pos_box(n - 7, 0)
    pos_box(0, n - 7)

    def align_box(x: int, y: int) -> None:  # upper-left corner
        for dy in range(5):
            for dx in range(5):
                black = dx in (0, 4) or dy in (0, 4) or (dx == 2 and dy == 2)
                put(y + dy, x + dx, _FUNC, black)

    apos, astride = _VTAB[version][0], _VTAB[version][1]
    x = 4
    while x + 5 < n:
        y = 4
        while y + 5 < n:
            overlaps = (
                (x < 7 and y < 7)
                or (x < 7 and y + 5 >= n - 7)
                or (x + 5 >= n - 7 and y < 7)
            )
            if not overlaps:
                align_box(x, y)
            y = apos if y == 4 else y + astride
        x = apos if x == 4 else x + astride

    pattern = _VTAB[version][3]
    if pattern != 0:
        val = pattern
        for x in range(6):
            for y in range(3):
                black = val & 1 != 0
                put(n - 11 + y, x, _FUNC, black)
                put(x, n - 11 + y, _FUNC, black)
                val >>= 1

    put(n - 8, 8, _FUNC, True)  # the lone dark module

    # Format-information cells (fplan): their positions are fixed; the encoder
    # writes their bits per mask, so they are occupied but never counted as base.
    for i in range(15):
        if i < 6:
            ty, tx = i, 8
        elif i < 8:
            ty, tx = i + 1, 8
        elif i < 9:
            ty, tx = 8, 7
        else:
            ty, tx = 8, 14 - i
        role[ty][tx] = _FORMAT
        if i < 8:
            by, bx = 8, n - 1 - i
        else:
            by, bx = n - 1 - (14 - i), 8
        role[by][bx] = _FORMAT

    return role, dark, n


def _pack_bits(flags: list[bool]) -> str:
    """Row-major MSB-first bits → base64, as qrjs.js unpacks them."""
    out = bytearray((len(flags) + 7) // 8)
    for i, on in enumerate(flags):
        if on:
            out[i >> 3] |= 0x80 >> (i & 7)
    return base64.b64encode(bytes(out)).decode("ascii")


def qr_plan(version: int, level: int) -> dict:
    """The PlayerPlan qrjs.js consumes: version, level, size, block structure and
    the occ/base bitmaps. Identical to the Go beam's playerPlan(version, level)."""
    role, dark, n = _vplan(version)
    occ = []
    base = []
    for y in range(n):
        for x in range(n):
            r = role[y][x]
            occ.append(r != _FREE)
            base.append(r == _FUNC and dark[y][x])
    _, _, total, _, levels = _VTAB[version]
    nblock, check = levels[level]
    return {
        "version": version,
        "level": level,
        "n": n,
        "data": total - nblock * check,
        "check": nblock * check,
        "blocks": nblock,
        "occ": _pack_bits(occ),
        "base": _pack_bits(base),
    }


def plan_qr(frame_texts: list[str], level: int) -> tuple[int, int, dict]:
    """Pick one QR version for every frame (the one the longest frame needs) so the
    symbol geometry never changes on screen. Returns (version, tile size, plan)."""
    longest = max(len(t) for t in frame_texts)
    version = min_version(longest, level)
    return version, (17 + 4 * version) + 2 * QUIET_ZONE, qr_plan(version, level)


# ---------------------------------------------------------------------------
# Repobundle: pack a folder (or several files) into one text file the tower can
# unpack (docs/BUNDLE.md). A byte-for-byte match of internal/bundle.Pack.
# ---------------------------------------------------------------------------

BUNDLE_MAGIC = "#repobundle v1"
BUNDLE_BOUND = "@@@FILE@@@"
BUNDLE_END = "@@@END@@@"


class BeamError(Exception):
    """A beam cannot be produced with the given parameters."""


def _git_files(root: str) -> list[str] | None:
    """Files git would track or keep under root (tracked + untracked-not-ignored),
    de-duplicated and sorted; None if root is not a git repo or git is missing."""
    try:
        tracked = subprocess.run(
            ["git", "-C", root, "ls-files", "-z"], check=True, capture_output=True
        ).stdout
        others = subprocess.run(
            ["git", "-C", root, "ls-files", "-z", "--others", "--exclude-standard"],
            check=True, capture_output=True,
        ).stdout
    except (OSError, subprocess.CalledProcessError):
        return None
    seen: set[str] = set()
    out: list[str] = []
    for seg in (tracked + others).split(b"\x00"):
        if not seg:
            continue
        rel = seg.decode("utf-8", "surrogateescape")
        if rel not in seen:
            seen.add(rel)
            out.append(rel)
    return sorted(out)


def _walk_files(root: str) -> list[str]:
    """Fallback file list: a plain walk skipping .git, sorted."""
    out = []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d != ".git"]
        for fn in filenames:
            out.append(os.path.relpath(os.path.join(dirpath, fn), root).replace(os.sep, "/"))
    return sorted(out)


def _is_binary(data: bytes) -> bool:
    if b"\x00" in data:
        return True
    try:
        data.decode("utf-8")
        return False
    except UnicodeDecodeError:
        return True


def _has_boundary_line(data: bytes) -> bool:
    for line in data.split(b"\n"):
        if line.startswith(BUNDLE_BOUND.encode()) or line.startswith(BUNDLE_END.encode()):
            return True
    return False


def _wrap_base64(data: bytes, width: int = 120) -> bytes:
    enc = base64.b64encode(data).decode("ascii")
    return "\n".join(enc[i : i + width] for i in range(0, len(enc), width)).encode("ascii")


def pack_bundle(root: str, fmt: str, files: list[str]) -> tuple[bytes, list[str]]:
    """Write a repobundle for `files` (relative to root) in `fmt` (text or base64),
    matching internal/bundle.Pack byte for byte. A text bundle drops binary files
    (returned as the second value) and raises BeamError on a boundary marker.
    Returns (bytes, skipped)."""
    out = bytearray(f"{BUNDLE_MAGIC} format={fmt}\n".encode("ascii"))
    skipped: list[str] = []
    for rel in files:
        path = os.path.join(root, rel.replace("/", os.sep))
        if not (os.path.isfile(path) and not os.path.islink(path)):
            continue  # skip dirs, symlinks, missing
        with open(path, "rb") as fh:
            data = fh.read()
        if fmt == "text":
            if _is_binary(data):
                skipped.append(rel)
                continue
            if _has_boundary_line(data):
                raise BeamError(f"{rel} contains a bundle boundary marker; re-run with --format base64")
            payload = data
        else:
            payload = _wrap_base64(data)
        mode = oct(os.stat(path).st_mode & 0o777)[2:]
        sha = hashlib.sha256(data).hexdigest()
        out += f"{BUNDLE_BOUND} {len(data)} {sha} {mode} {rel}\n".encode("utf-8")
        out += payload
        out += b"\n"
    out += f"{BUNDLE_END}\n".encode("ascii")
    return bytes(out), skipped


def pack_auto(root: str, fmt: str, files: list[str]) -> tuple[bytes, str]:
    """Pack in `fmt`; 'auto' bundles as text when every file is text and none holds
    a boundary marker, else base64 (so nothing is silently left out). An explicit
    text bundle warns about any binary it drops. Returns (bytes, format used)."""
    if fmt == "base64":
        return pack_bundle(root, "base64", files)[0], "base64"
    if fmt == "text":
        data, skipped = pack_bundle(root, "text", files)
        if skipped:
            print(f"{PROG}: text format skipped {len(skipped)} binary file(s): {', '.join(skipped)}", file=sys.stderr)
        return data, "text"
    try:
        data, skipped = pack_bundle(root, "text", files)
        if not skipped:
            return data, "text"
    except BeamError:
        pass  # a boundary marker in a text file: fall back to base64
    return pack_bundle(root, "base64", files)[0], "base64"


# ---------------------------------------------------------------------------
# The player: one self-contained HTML page. The frames travel as base45 text and
# the embedded encoder (qrjs.js) paints each symbol onto a canvas from the plan,
# so the page weighs about the payload's size, not a picture per frame. Styling
# is deliberately minimal: black page, white QR tile, one dim status line.
# ---------------------------------------------------------------------------

# The __TOKEN__ slots in the template below are filled by player_html; the JS
# keeps its own braces and `$` shorthand, so the template is filled by plain
# string replacement (not str.format / string.Template, which would choke).
#
# QRJS is the in-page QR encoder, a VERBATIM copy of internal/beam/qrjs.js in
# the airlift repo — its source of truth. It is bit-for-bit identical to the Go
# reference encoder (the repo tests it against testdata/qr), so a beam this
# script writes renders the same symbols the Go `airlift beam` would. If
# qrjs.js ever changes upstream, replace the string below with the new file.
QRJS = r"""
// airlift beam - the in-page QR encoder (ADR 0011, amended).
//
// The Go side picks the version and ECC for a beam and ships the plan for that
// symbol: the size, the block structure and two bitmaps - which modules are
// function patterns (occ) and which of those are dark (base). This file turns
// one frame's text into that symbol: alphanumeric data bits -> Reed-Solomon
// check bytes -> block interleave -> zig-zag placement -> the best of the eight
// masks by the ISO/IEC 18004 section 8.8.2 penalty -> format bits. It is a twin of
// rsc.io/qr/coding's Plan.Encode plus internal/beam/penalty.go and is checked
// bit for bit against them (web/src/beam/qrjs.test.ts, testdata/qr).
//
// Plain ES5 on purpose: the beam runs in whatever browser the air-gapped
// machine has. No dependencies, no network. ASCII on purpose too: this file is
// inlined into every beam, and one non-Latin-1 character would make the
// browser keep the whole script (about the page's size) as two-byte text.
function airliftQR(plan) {
  'use strict';
  var n = plan.n, N = n * n;
  var level = plan.level; // rsc.io numbering: L=0, M=1, Q=2, H=3
  var dataBytes = plan.data, checkBytes = plan.check, blocks = plan.blocks;
  var nde = Math.floor(dataBytes / blocks), extra = dataBytes % blocks;
  var ne = checkBytes / blocks;
  var countBits = plan.version <= 9 ? 9 : plan.version <= 26 ? 11 : 13;
  var ALPHA = '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:';

  // --- the plan's bitmaps: occupied (function) modules and their colour ---
  function unpack(b64) {
    var s = atob(b64), out = new Uint8Array(N);
    for (var i = 0; i < N; i++) out[i] = (s.charCodeAt(i >> 3) >> (7 - (i & 7))) & 1;
    return out;
  }
  var occ = unpack(plan.occ), base = unpack(plan.base);

  // Zig-zag placement order over the free modules: pairs of columns from the
  // right, up then down, right module before left, skipping column 6.
  var order = new Int32Array(N), free = 0;
  for (var x = n; x > 0;) {
    var y;
    for (y = n - 1; y >= 0; y--) {
      if (!occ[y * n + x - 1]) order[free++] = y * n + x - 1;
      if (!occ[y * n + x - 2]) order[free++] = y * n + x - 2;
    }
    x -= 2;
    if (x === 7) x--;
    for (y = 0; y < n; y++) {
      if (!occ[y * n + x - 1]) order[free++] = y * n + x - 1;
      if (!occ[y * n + x - 2]) order[free++] = y * n + x - 2;
    }
    x -= 2;
  }

  // The eight mask patterns over the free modules (zero elsewhere).
  var MASK = [
    function (i, j) { return (i + j) % 2 === 0; },
    function (i, j) { return i % 2 === 0; },
    function (i, j) { return j % 3 === 0; },
    function (i, j) { return (i + j) % 3 === 0; },
    function (i, j) { return (Math.floor(i / 2) + Math.floor(j / 3)) % 2 === 0; },
    function (i, j) { return (i * j) % 2 + (i * j) % 3 === 0; },
    function (i, j) { return ((i * j) % 2 + (i * j) % 3) % 2 === 0; },
    function (i, j) { return ((i * j) % 3 + (i + j) % 2) % 2 === 0; }
  ];
  var masks = [];
  for (var m = 0; m < 8; m++) {
    var mb = new Uint8Array(N);
    for (var yy = 0; yy < n; yy++) for (var xx = 0; xx < n; xx++) {
      var idx = yy * n + xx;
      if (!occ[idx] && MASK[m](yy, xx)) mb[idx] = 1;
    }
    masks.push(mb);
  }

  // Format information for each mask: 5 bits (level, mask), BCH remainder,
  // XOR 0x5412, at the two fixed sets of positions.
  var fmtPos = [];
  for (var fi = 0; fi < 15; fi++) {
    var a = fi < 6 ? [fi, 8] : fi < 8 ? [fi + 1, 8] : fi < 9 ? [8, 7] : [8, 14 - fi];
    var b = fi < 8 ? [8, n - 1 - fi] : [n - 1 - (14 - fi), 8];
    fmtPos.push([a[0] * n + a[1], b[0] * n + b[1]]);
  }
  var fmtBits = [];
  for (m = 0; m < 8; m++) {
    var fb = ((level ^ 1) << 13) | (m << 10);
    var rem = fb;
    for (var bi = 14; bi >= 10; bi--) if (rem & (1 << bi)) rem ^= 0x537 << (bi - 10);
    fb = (fb | rem) ^ 0x5412;
    fmtBits.push(fb);
  }

  // --- GF(256) with the QR polynomial, and the Reed-Solomon generator ---
  var EXP = new Uint8Array(512), LOG = new Uint8Array(256);
  var v = 1;
  for (var e = 0; e < 255; e++) {
    EXP[e] = v; LOG[v] = e;
    v <<= 1;
    if (v & 0x100) v ^= 0x11d;
  }
  for (e = 255; e < 512; e++) EXP[e] = EXP[e - 255];
  function mul(p, q) { return p === 0 || q === 0 ? 0 : EXP[LOG[p] + LOG[q]]; }
  var gen = new Uint8Array(ne + 1);
  gen[ne] = 1;
  for (e = 0; e < ne; e++) {
    var c = EXP[e];
    for (var j = 0; j < ne; j++) gen[j] = mul(gen[j], c) ^ gen[j + 1];
    gen[ne] = mul(gen[ne], c);
  }
  var lgen = new Uint16Array(ne + 1);
  for (e = 0; e <= ne; e++) lgen[e] = gen[e] === 0 ? 255 : LOG[gen[e]];

  // rs writes the ne check bytes of data[from..to) into out[at..).
  function rs(data, from, to, out, at) {
    var len = to - from, p = new Uint8Array(len + ne);
    for (var i = 0; i < len; i++) p[i] = data[from + i];
    for (i = 0; i < len; i++) {
      var ci = p[i];
      if (ci === 0) continue;
      var lc = LOG[ci];
      for (var k = 1; k <= ne; k++) if (lgen[k] !== 255) p[i + k] ^= EXP[lc + lgen[k]];
    }
    for (i = 0; i < ne; i++) out[at + i] = p[len + i];
  }

  // --- the data bit stream ---
  var codeBytes = new Uint8Array(dataBytes + checkBytes);
  var nbit;
  function write(val, bits) {
    for (var i = bits - 1; i >= 0; i--) {
      if ((val >>> i) & 1) codeBytes[nbit >> 3] |= 0x80 >> (nbit & 7);
      nbit++;
    }
  }
  function encodeData(text) {
    for (var i = 0; i < codeBytes.length; i++) codeBytes[i] = 0;
    var pad = dataBytes * 8 - (4 + countBits + Math.floor((11 * text.length + 1) / 2));
    if (pad < 0) throw new Error('airlift beam: a ' + text.length + '-character frame does not fit the symbol');
    nbit = 0;
    write(2, 4);
    write(text.length, countBits);
    var a, b;
    for (i = 0; i + 2 <= text.length; i += 2) {
      a = ALPHA.indexOf(text.charAt(i));
      b = ALPHA.indexOf(text.charAt(i + 1));
      if (a < 0 || b < 0) throw new Error('airlift beam: frame text is not base45');
      write(a * 45 + b, 11);
    }
    if (i < text.length) {
      a = ALPHA.indexOf(text.charAt(i));
      if (a < 0) throw new Error('airlift beam: frame text is not base45');
      write(a, 6);
    }
    if (pad <= 4) {
      write(0, pad);
    } else {
      write(0, 4);
      pad -= 4;
      var align = (-nbit) & 7;
      pad -= align;
      write(0, align);
      for (var k = 0, bytes = pad >> 3; k < bytes; k++) write(k % 2 === 0 ? 0xec : 0x11, 8);
    }
    // Check bytes, block by block; the last `extra` blocks carry one more byte.
    var from = 0, at = dataBytes;
    for (var bl = 0; bl < blocks; bl++) {
      var db = bl >= blocks - extra ? nde + 1 : nde;
      rs(codeBytes, from, from + db, codeBytes, at);
      from += db;
      at += ne;
    }
  }

  // Interleave: byte i of every data block in turn, then of every check block,
  // as a sequence of byte offsets into codeBytes.
  var seq = [];
  (function () {
    var starts = [], sizes = [], from = 0;
    for (var bl = 0; bl < blocks; bl++) {
      var db = bl >= blocks - extra ? nde + 1 : nde;
      starts.push(from); sizes.push(db); from += db;
    }
    for (var i = 0; i <= nde; i++) for (bl = 0; bl < blocks; bl++) if (i < sizes[bl]) seq.push(starts[bl] + i);
    for (i = 0; i < ne; i++) for (bl = 0; bl < blocks; bl++) seq.push(dataBytes + bl * ne + i);
  })();
  var totalBits = seq.length * 8;

  // --- the ISO penalty over a 0/1 matrix ---
  function penalty(g) {
    var score = 0, i, j, k, count, cur, prev, dark = 0;
    // rule 1: runs of five or more, rows then columns
    for (i = 0; i < n; i++) {
      count = 1; prev = g[i * n];
      for (j = 1; j < n; j++) {
        cur = g[i * n + j];
        if (cur === prev) { count++; continue; }
        if (count >= 5) score += 3 + (count - 5);
        count = 1; prev = cur;
      }
      if (count >= 5) score += 3 + (count - 5);
    }
    for (j = 0; j < n; j++) {
      count = 1; prev = g[j];
      for (i = 1; i < n; i++) {
        cur = g[i * n + j];
        if (cur === prev) { count++; continue; }
        if (count >= 5) score += 3 + (count - 5);
        count = 1; prev = cur;
      }
      if (count >= 5) score += 3 + (count - 5);
    }
    // rule 2: 2x2 blocks of one colour
    for (i = 0; i < n - 1; i++) for (j = 0; j < n - 1; j++) {
      cur = g[i * n + j];
      if (g[i * n + j + 1] === cur && g[(i + 1) * n + j] === cur && g[(i + 1) * n + j + 1] === cur) score += 3;
    }
    // rule 3: 1:1:3:1:1 finder-like patterns with four light modules to a side
    var PA = [1, 0, 1, 1, 1, 0, 1, 0, 0, 0, 0], PB = [0, 0, 0, 0, 1, 0, 1, 1, 1, 0, 1];
    function at(base, step, start, pat) {
      for (var t = 0; t < 11; t++) if (g[base + (start + t) * step] !== pat[t]) return false;
      return true;
    }
    for (i = 0; i < n; i++) for (k = 0; k + 11 <= n; k++) {
      if (at(i * n, 1, k, PA) || at(i * n, 1, k, PB)) score += 40;
    }
    for (j = 0; j < n; j++) for (k = 0; k + 11 <= n; k++) {
      if (at(j, n, k, PA) || at(j, n, k, PB)) score += 40;
    }
    // rule 4: dark proportion away from 50 %, in 5 % steps
    for (i = 0; i < N; i++) dark += g[i];
    var percent = dark * 100 / N;
    score += 10 * Math.floor(Math.abs(percent - 50) / 5);
    return score;
  }

  var raw = new Uint8Array(N), cand = new Uint8Array(N), best = new Uint8Array(N);

  // encode returns {mask, bits}: bits is one byte per module, row-major, 1 dark.
  // A frame longer than the symbol holds, or outside the base45 alphabet, throws.
  function encode(text) {
    encodeData(text);
    raw.set(base);
    var bit = 0;
    for (var s = 0; s < seq.length; s++) {
      var byte = codeBytes[seq[s]];
      for (var b = 7; b >= 0; b--) raw[order[bit++]] = (byte >> b) & 1;
    }
    for (; bit < free; bit++) raw[order[bit]] = 0; // remainder modules
    var bestMask = -1, bestScore = Infinity;
    for (var m = 0; m < 8; m++) {
      var mb = masks[m];
      for (var i = 0; i < N; i++) cand[i] = raw[i] ^ mb[i];
      var fb = fmtBits[m];
      for (i = 0; i < 15; i++) {
        var bitv = (fb >> i) & 1;
        cand[fmtPos[i][0]] = bitv;
        cand[fmtPos[i][1]] = bitv;
      }
      var sc = penalty(cand);
      if (sc < bestScore) {
        bestScore = sc; bestMask = m;
        best.set(cand);
      }
    }
    return { mask: bestMask, bits: new Uint8Array(best) }; // a copy: the buffers are reused
  }

  return { n: n, free: free, totalBits: totalBits, encode: encode };
}
"""

_PLAYER = """<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>airlift beam &middot; __TITLE__</title>
<style>
html,body{margin:0;height:100%;background:#000;color:#8a929c;overflow:hidden}
body{display:flex;flex-direction:column;-webkit-user-select:none;user-select:none;
font:13px/1.4 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
header{flex:none;display:flex;flex-wrap:wrap;gap:.35em 1.25em;padding:.55em 1em}
header b{color:#c9d0d8}
header .hint{margin-left:auto;color:#4a525c}
main{flex:1;min-height:0;display:flex;align-items:center;justify-content:center}
#tile{background:#fff;line-height:0}
canvas{display:block}
@media (max-width:600px){header .hint{display:none}}
</style>
</head>
<body>
<header>
<b>airlift beam</b>
<span>__NAME__</span>
<span>session __SESSION__</span>
<span>__MODE__</span>
<span id="frame"></span>
<span id="chunk"></span>
<span class="hint">space pause &middot; &larr;/&rarr; step &middot; +/- fps &middot; f fullscreen</span>
</header>
<main id="main"><div id="tile"><canvas id="qr"></canvas></div></main>
<script>
__QRJS__
(function () {
  var PLAN = __PLAN__;
  var FRAMES = __FRAMES__;
  var ORDER = __ORDER__;
  var N = __TOTAL__, fps = __FPS__, LABEL = __LABEL__;
  var i = 0, playing = true, acc = 0, last = null;
  var $ = function (id) { return document.getElementById(id); };
  var main = $('main'), tile = $('tile'), canvas = $('qr'), ctx = canvas.getContext('2d');
  var hFrame = $('frame'), hChunk = $('chunk');
  var KEYS = {32: ' ', 37: 'ArrowLeft', 39: 'ArrowRight', 187: '+', 61: '+', 107: '+',
              189: '-', 173: '-', 109: '-', 70: 'f'};
  var qr = airliftQR(PLAN), n = qr.n, M = n * n;
  var off = document.createElement('canvas');
  off.width = n; off.height = n;
  var octx = off.getContext('2d'), img = octx.createImageData(n, n);
  var cache = new Array(FRAMES.length), shown = -1;
  function symbol(k) {
    var packed = cache[k];
    if (!packed) {
      var bits = qr.encode(FRAMES[k]).bits;
      packed = new Uint8Array((M + 7) >> 3);
      for (var j = 0; j < M; j++) { if (bits[j]) { packed[j >> 3] |= 0x80 >> (j & 7); } }
      cache[k] = packed;
    }
    return packed;
  }
  function paint(k) {
    var packed = symbol(k), d = img.data;
    for (var j = 0, p = 0; j < M; j++, p += 4) {
      var v = (packed[j >> 3] >> (7 - (j & 7))) & 1 ? 0 : 255;
      d[p] = v; d[p + 1] = v; d[p + 2] = v; d[p + 3] = 255;
    }
    octx.putImageData(img, 0, 0);
    ctx.imageSmoothingEnabled = false;
    ctx.fillStyle = '#fff';
    ctx.fillRect(0, 0, canvas.width, canvas.height);
    ctx.drawImage(off, 0, 0, canvas.width, canvas.height);
    shown = k;
  }
  function fit() {
    var dpr = window.devicePixelRatio || 1;
    var s = Math.floor(Math.min(main.clientWidth, main.clientHeight));
    var k = Math.max(1, Math.floor(s * dpr / (n + 8)));  // device pixels/module, 4-module quiet zone
    canvas.width = n * k; canvas.height = n * k;
    canvas.style.width = (n * k / dpr) + 'px';
    canvas.style.height = (n * k / dpr) + 'px';
    tile.style.padding = (4 * k / dpr) + 'px';
    if (shown >= 0) { paint(shown); }
  }
  function show() {
    var k = ORDER[i];
    paint(k);
    hFrame.textContent = 'frame ' + (i + 1) + '/' + ORDER.length;
    hChunk.textContent = k === 0 ? 'manifest' : LABEL + ' ' + k + '/' + N;
  }
  function step(d) { playing = false; acc = 0; i = (i + d + ORDER.length) % ORDER.length; show(); }
  function tick(ts) {
    if (last !== null && playing) {
      acc += ts - last;
      var period = 1000 / fps;
      if (acc >= period) { acc = Math.min(acc - period, period); i = (i + 1) % ORDER.length; show(); }
    }
    last = ts;
    window.requestAnimationFrame(tick);
  }
  document.addEventListener('keydown', function (e) {
    var k = e.key || KEYS[e.keyCode];
    if (k === 'Left') { k = 'ArrowLeft'; }
    if (k === 'Right') { k = 'ArrowRight'; }
    if (k === 'Spacebar') { k = ' '; }
    if (k === ' ') { playing = !playing; acc = 0; }
    else if (k === 'ArrowRight') { step(1); }
    else if (k === 'ArrowLeft') { step(-1); }
    else if (k === '+' || k === '=') { fps = Math.min(60, fps + 1); }
    else if (k === '-' || k === '_') { fps = Math.max(1, fps - 1); }
    else if (k === 'f' || k === 'F') {
      if (document.fullscreenElement) { document.exitFullscreen(); }
      else if (document.documentElement.requestFullscreen) { document.documentElement.requestFullscreen(); }
    }
    else { return; }
    e.preventDefault();
  });
  window.addEventListener('resize', fit);
  document.addEventListener('fullscreenchange', fit);
  fit(); show();
  window.requestAnimationFrame(tick);
  if (navigator.wakeLock && navigator.wakeLock.request) {
    var lock = function () { navigator.wakeLock.request('screen').catch(function () {}); };
    lock();
    document.addEventListener('visibilitychange', function () { if (!document.hidden) { lock(); } });
  }
})();
</script>
</body>
</html>
"""


def _js(obj) -> str:
    return json.dumps(obj, separators=(",", ":")).replace("</", "<\\/")


def player_html(name, session, total, order, frames, plan, fps, fountain) -> str:
    """Fill the player template by plain replacement. Values (JSON, base45 text,
    the embedded JS) never contain a __TOKEN__, so the order of replacement and
    re-scanning are both harmless."""
    mode, label = ("fountain", "packet") if fountain else ("sequential", "chunk")
    repl = {
        "__TITLE__": html.escape(name),
        "__NAME__": html.escape(name),
        "__SESSION__": f"{session:08x}",
        "__MODE__": mode,
        "__QRJS__": QRJS.strip(),
        "__PLAN__": _js(plan),
        "__FRAMES__": _js(frames),
        "__ORDER__": _js(order),
        "__TOTAL__": str(total),
        "__FPS__": str(fps),
        "__LABEL__": _js(label),
    }
    out = _PLAYER
    for token, value in repl.items():
        out = out.replace(token, value)
    return out


# ---------------------------------------------------------------------------
# Build: assemble the whole beam for one payload.
# ---------------------------------------------------------------------------


def build(data: bytes, name: str, *, chunk: int, level: int, fps: int, manifest_every: int,
          seed: int | None, mode: str) -> tuple[str, dict, int, bool]:
    """Encode, fix one QR version, lay out the loop and fill the player.
    Returns (html, manifest, version, fountain)."""
    session = new_session(seed)
    blob = compress(data)  # gzip once; the chunk count decides the auto layout
    total = manifest_total(len(blob), chunk)
    fountain = mode == "fountain" or (mode == "auto" and total >= FOUNTAIN_THRESHOLD)
    manifest, frames = _frames_from_blob(blob, len(data), sha256hex(data), name, chunk, session, fountain)
    version, _size, plan = plan_qr(frames, level)
    order = schedule(len(frames) - 1, manifest_every)
    doc = player_html(name, session, len(frames) - 1, order, frames, plan, fps, fountain)
    return doc, manifest, version, fountain


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def _common_dir(abs_files: list[str]) -> str:
    common = os.path.dirname(abs_files[0]).split(os.sep)
    for f in abs_files[1:]:
        parts = os.path.dirname(f).split(os.sep)
        i = 0
        while i < min(len(common), len(parts)) and common[i] == parts[i]:
            i += 1
        common = common[:i]
    return os.sep.join(common) or os.sep


def resolve_beam(inputs: list[str], name: str, fmt: str):
    """Turn input paths into (payload bytes, beam name, bundle format used).
    One dir → git-aware bundle; one file → sent as-is; several files → bundle."""
    if len(inputs) == 1:
        p = inputs[0]
        if os.path.isdir(p):
            name = name or os.path.basename(os.path.abspath(p))
            files = _git_files(p)
            files = files if files is not None else _walk_files(p)
            data, used = pack_auto(p, fmt, files)
            return data, name, used
        if not os.path.isfile(p):
            raise BeamError(f"{p} is not a regular file")
        name = name or os.path.basename(p)
        with open(p, "rb") as fh:
            return fh.read(), name, ""
    # Several files: bundle them, rooted at their common directory.
    if not name:
        raise BeamError("several files need a name: pass --name NAME")
    abs_files = [os.path.abspath(p) for p in inputs]
    for a in abs_files:
        if not os.path.isfile(a):
            raise BeamError(f"not a file: {a}")
    root = _common_dir(abs_files)
    seen: set[str] = set()
    rels: list[str] = []
    for a in abs_files:
        rel = os.path.relpath(a, root).replace(os.sep, "/")
        if rel not in seen:
            seen.add(rel)
            rels.append(rel)
    data, used = pack_auto(root, fmt, rels)
    return data, name, used


def _html_filename(name: str) -> str:
    base = os.path.basename(name) or "beam"
    stem, ext = os.path.splitext(base)
    return (stem or base) + ".html"


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(
        prog=PROG, add_help=True,
        description="Render a file or folder as an animated QR beam page (airlift-compatible).",
    )
    ap.add_argument("paths", nargs="+", metavar="PATH", help="a folder, one file, or several files")
    ap.add_argument("--name", default="", help="beam name (default: the folder or file name)")
    ap.add_argument("--format", default="auto", choices=("auto", "text", "base64"), help="bundle format")
    ap.add_argument("--mode", default="auto", choices=("auto", "sequential", "fountain"), help="frame layout")
    ap.add_argument("--chunk", type=int, default=0, help="payload bytes per frame")
    ap.add_argument("--version-target", type=int, default=0, metavar="V", help="largest chunk QR version V holds")
    ap.add_argument("--ecc", default="M", choices=("L", "M", "Q", "H"), help="QR error correction (default M)")
    ap.add_argument("--fps", type=int, default=DEFAULT_FPS, help="initial frames per second, 1..60")
    ap.add_argument("--manifest-every", type=int, default=20, help="re-insert the manifest every K frames")
    ap.add_argument("--seed", type=int, default=None, help="derive the sender id from N (reproducible)")
    ap.add_argument("--out", default="", help="where to write the page (default <name>.html)")
    ap.add_argument("--no-open", action="store_true", help="do not open the page in a browser")
    a = ap.parse_args(argv)

    if not 1 <= a.fps <= 60:
        ap.error("--fps must be 1..60")
    if a.manifest_every < 1:
        ap.error("--manifest-every must be >= 1")
    level = _LEVELS[a.ecc]

    try:
        data, name, bundle_format = resolve_beam(a.paths, a.name, a.format)
        if a.version_target:
            chunk = chunk_for_version(a.version_target, level)
        elif a.chunk:
            chunk = a.chunk
        else:
            chunk = chunk_for_version(30, level)  # version-30 symbol, whatever the ECC
        if not 1 <= chunk <= MAX_CHUNK:
            raise BeamError(f"--chunk must be 1..{MAX_CHUNK}")
        doc, manifest, version, fountain = build(
            data, name, chunk=chunk, level=level, fps=a.fps,
            manifest_every=a.manifest_every, seed=a.seed, mode=a.mode,
        )
    except BeamError as exc:
        print(f"{PROG}: {exc}", file=sys.stderr)
        return 2

    out_path = a.out or _html_filename(name)
    with open(out_path, "w", encoding="utf-8") as fh:
        fh.write(doc)

    total = manifest_total(manifest["gz_size"], chunk)
    ratio = 100.0 * manifest["gz_size"] / manifest["orig_size"] if manifest["orig_size"] else 0.0
    modules = 17 + 4 * version
    print(f"airlift beam  {name} -> {out_path}")
    print(f"  input    {manifest['orig_size']:>10} bytes   sha256 {manifest['orig_sha256'][:16]}...")
    if bundle_format:
        print(f"  bundle   {bundle_format} format")
    print(f"  gzip     {manifest['gz_size']:>10} bytes   {ratio:.1f} % of input")
    print(f"  chunks   {total:>10} x {chunk} bytes")
    print(f"  mode     {'fountain' if fountain else 'sequential'}")
    print(f"  qr       version {version} ({modules}x{modules} modules), ECC {a.ecc}, alphanumeric")
    print(f"  output   {len(doc):>10} bytes")

    if not a.no_open:
        try:
            webbrowser.open(f"file://{os.path.abspath(out_path)}")
        except Exception:
            pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
