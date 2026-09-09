#!/usr/bin/env python3
"""
airlift.py — render a file as an animated QR loop for optical transfer out of
an air-gapped machine.

  beam    --in FILE --out beam.html [--chunk 600 | --version-target V] [--ecc M]
          [--fps 8] [--manifest-every 20] [--seed N] [--fountain [--fountain-packets K]]
  frames  --in FILE --out frames.json [--seed N] [--chunk 600] [--fountain [--fountain-packets K]]
  decode  --frames frames.json --out FILE

Pipeline: input → gzip → chunk → frames → base45 → QR (alphanumeric mode,
ECC M) → inline SVG → one self-contained HTML player. Wire format, loop
schedule, fountain code and verification chain: docs/PROTOCOL.md.

Runs inside the air gap. Python 3.9+. Only dependency: segno (pure Python;
`pip install segno`, or copy the `segno/` package next to this file). The
`frames` and `decode` subcommands need nothing beyond the standard library.
"""

from __future__ import annotations

import argparse
import bisect
import gzip
import hashlib
import html
import json
import math
import os
import random
import secrets
import string
import struct
import sys
import zlib
from collections.abc import Iterable, Sequence
from dataclasses import dataclass, field

__version__ = "0.2.0"

# ---------------------------------------------------------------------------
# base45 (RFC 9285). The alphabet is exactly QR's alphanumeric character set.
# ---------------------------------------------------------------------------

B45_ALPHABET = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:"
_B45_INDEX = {c: i for i, c in enumerate(B45_ALPHABET)}


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


def b45decode(text: str) -> bytes:
    """Strict inverse of b45encode. Raises ValueError on a bad length, character or value."""
    if len(text) % 3 == 1:
        raise ValueError("base45: invalid length")
    try:
        vals = [_B45_INDEX[c] for c in text]
    except KeyError as exc:
        raise ValueError(f"base45: invalid character {exc.args[0]!r}") from None
    out = bytearray()
    for i in range(0, len(vals) - 2, 3):
        v = vals[i] + vals[i + 1] * 45 + vals[i + 2] * 2025
        if v > 0xFFFF:
            raise ValueError("base45: triplet out of range")
        out += v.to_bytes(2, "big")
    if len(vals) % 3 == 2:
        v = vals[-2] + vals[-1] * 45
        if v > 0xFF:
            raise ValueError("base45: pair out of range")
        out.append(v)
    return bytes(out)


# ---------------------------------------------------------------------------
# Frames: 18-byte big-endian header + payload (docs/PROTOCOL.md).
# ---------------------------------------------------------------------------

MAGIC = 0x414C  # "AL"
VERSION = 1
T_MANIFEST, T_DATA, T_FOUNTAIN = 0, 1, 2
TYPE_NAMES = {T_MANIFEST: "MANIFEST", T_DATA: "DATA", T_FOUNTAIN: "FOUNTAIN"}
HEADER = struct.Struct(">HBBIHHHI")  # magic ver type session seq total len crc32
HEADER_LEN = HEADER.size  # 18
MAX_CHUNKS = 0xFFFF
MAX_PACKETS = 0x10000  # fountain seeds are u16


class FrameError(ValueError):
    """A frame that fails to decode, parse or verify."""


@dataclass(frozen=True)
class Frame:
    type: int
    session: int
    seq: int
    total: int
    payload: bytes

    def pack(self) -> bytes:
        crc = zlib.crc32(self.payload) & 0xFFFFFFFF
        return (
            HEADER.pack(
                MAGIC,
                VERSION,
                self.type,
                self.session,
                self.seq,
                self.total,
                len(self.payload),
                crc,
            )
            + self.payload
        )

    @classmethod
    def parse(cls, raw: bytes) -> Frame:
        if len(raw) < HEADER_LEN:
            raise FrameError("short header")
        magic, ver, ftype, session, seq, total, length, crc = HEADER.unpack_from(raw)
        if magic != MAGIC:
            raise FrameError(f"bad magic 0x{magic:04X}")
        if ver != VERSION:
            raise FrameError(f"unsupported version {ver}")
        if ftype not in TYPE_NAMES:
            raise FrameError(f"unknown frame type {ftype}")
        if len(raw) != HEADER_LEN + length:
            raise FrameError(f"length mismatch: header says {length}, got {len(raw) - HEADER_LEN}")
        payload = bytes(raw[HEADER_LEN:])
        if zlib.crc32(payload) & 0xFFFFFFFF != crc:
            raise FrameError("crc mismatch")
        return cls(ftype, session, seq, total, payload)

    def text(self) -> str:
        """The frame as it travels: base45, ready for QR alphanumeric mode."""
        return b45encode(self.pack())

    @classmethod
    def from_text(cls, text: str) -> Frame:
        try:
            raw = b45decode(text)
        except ValueError as exc:
            raise FrameError(str(exc)) from None
        return cls.parse(raw)


# ---------------------------------------------------------------------------
# Manifest (payload of the MANIFEST frame): compact JSON, fixed key order.
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class Manifest:
    name: str
    gz_size: int
    gz_sha256: str
    orig_size: int
    orig_sha256: str
    chunk: int

    @property
    def total(self) -> int:
        """N, the number of source chunks."""
        return max(1, -(-self.gz_size // self.chunk))

    def chunk_len(self, seq: int) -> int:
        """Bytes DATA chunk `seq` carries: the chunk size, or the remainder for the last."""
        last = self.total - 1
        if seq < last:
            return self.chunk
        return self.gz_size - last * self.chunk

    def to_json(self) -> bytes:
        obj = {
            "name": self.name,
            "gz_size": self.gz_size,
            "gz_sha256": self.gz_sha256,
            "orig_size": self.orig_size,
            "orig_sha256": self.orig_sha256,
            "chunk": self.chunk,
        }
        return json.dumps(obj, separators=(",", ":"), ensure_ascii=True).encode("ascii")

    @classmethod
    def from_json(cls, raw: bytes) -> Manifest:
        try:
            obj = json.loads(raw.decode("utf-8"))
            m = cls(
                name=str(obj["name"]),
                gz_size=int(obj["gz_size"]),
                gz_sha256=str(obj["gz_sha256"]),
                orig_size=int(obj["orig_size"]),
                orig_sha256=str(obj["orig_sha256"]),
                chunk=int(obj["chunk"]),
            )
        except (UnicodeDecodeError, ValueError, KeyError, TypeError) as exc:
            raise FrameError(f"manifest: {exc}") from None
        if m.chunk < 1 or m.gz_size < 0 or m.orig_size < 0:
            raise FrameError("manifest: field out of range")
        if len(m.gz_sha256) != 64 or len(m.orig_sha256) != 64:
            raise FrameError("manifest: malformed sha256")
        return m


# ---------------------------------------------------------------------------
# Fountain code: LT packets with a robust soliton degree distribution.
# ADR 0009 fixes every constant and operation here; the Go decoder in
# internal/proto reproduces fountain_indices() exactly.
# ---------------------------------------------------------------------------

FOUNTAIN_C = 0.1
FOUNTAIN_DELTA = 0.5
_TWO32 = 4294967296.0
_cdf_cache: dict[int, list[int]] = {}


def robust_soliton_cdf(n: int) -> list[int]:
    """Cumulative degree distribution over 1..n as thresholds scaled to 2^32.

    Degree d is drawn when a 32-bit sample x satisfies cdf[d-2] <= x < cdf[d-1]
    (cdf[-1] taken as 0). Plain left-to-right float arithmetic, no fused
    multiply-add, so Go and Python agree bit for bit."""
    if n < 1:
        raise ValueError("n must be >= 1")
    cached = _cdf_cache.get(n)
    if cached is not None:
        return cached
    if n == 1:
        cdf = [1 << 32]
    else:
        r = FOUNTAIN_C * math.log(n / FOUNTAIN_DELTA) * math.sqrt(n)
        m = int(n / r)
        m = max(1, min(m, n))
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
    """Packets a fountain beam carries: N + max(48, 3·√N·ln N), capped by the u16 seed.

    Measured with this distribution, decoding needs up to ~2.7 N packets at
    N = 24 but only ~1.2 N at N = 1200; the surplus term tracks that curve."""
    extra = max(48, math.ceil(3.0 * math.sqrt(total) * math.log(total)))
    return min(MAX_PACKETS, total + extra)


def _padded_chunks(blob: bytes, chunk: int, total: int) -> list[int]:
    return [
        int.from_bytes(blob[i * chunk : (i + 1) * chunk].ljust(chunk, b"\0"), "big")
        for i in range(total)
    ]


def fountain_payload(padded: Sequence[int], chunk: int, seed: int) -> bytes:
    acc = 0
    for i in fountain_indices(seed, len(padded)):
        acc ^= padded[i]
    return acc.to_bytes(chunk, "big")


class Peeler:
    """Belief-propagation (peeling) decoder. DATA frames enter as degree-1
    packets, so one decoder serves sequential and fountain beams alike."""

    def __init__(self, total: int, chunk: int) -> None:
        self.total = total
        self.chunk = chunk
        self.blocks: list[int | None] = [None] * total
        self.decoded = 0
        self._pending: dict[int, tuple[set[int], int]] = {}
        self._by_block: dict[int, set[int]] = {}
        self._next = 0

    @property
    def complete(self) -> bool:
        return self.decoded == self.total

    @property
    def pending(self) -> int:
        return len(self._pending)

    def have(self, i: int) -> bool:
        return self.blocks[i] is not None

    def add(self, indices: Iterable[int], payload: bytes) -> bool:
        """Feeds one packet; returns True when it decoded at least one new block."""
        acc = int.from_bytes(payload.ljust(self.chunk, b"\0"), "big")
        remaining: set[int] = set()
        for i in indices:
            if not 0 <= i < self.total:
                raise ValueError(f"block index {i} out of range")
            known = self.blocks[i]
            if known is None:
                remaining.add(i)
            else:
                acc ^= known
        if not remaining:
            return False
        pid = self._next
        self._next += 1
        self._pending[pid] = (remaining, acc)
        for i in remaining:
            self._by_block.setdefault(i, set()).add(pid)
        if len(remaining) > 1:
            return False
        before = self.decoded
        ripple = [pid]
        while ripple:
            entry = self._pending.pop(ripple.pop(), None)
            if entry is None:
                continue
            rem, value = entry
            (i,) = rem
            self._solve(i, value, ripple)
        return self.decoded > before

    def _solve(self, i: int, value: int, ripple: list[int]) -> None:
        self.blocks[i] = value
        self.decoded += 1
        for pid in self._by_block.pop(i, ()):
            entry = self._pending.get(pid)
            if entry is None:
                continue
            rem, acc = entry
            rem.discard(i)
            acc ^= value
            if not rem:
                del self._pending[pid]
            else:
                self._pending[pid] = (rem, acc)
                if len(rem) == 1:
                    ripple.append(pid)

    def blob(self, size: int) -> bytes:
        if not self.complete:
            raise ValueError("not complete")
        out = b"".join(int(b).to_bytes(self.chunk, "big") for b in self.blocks)  # type: ignore[arg-type]
        return out[:size]


# ---------------------------------------------------------------------------
# Encode: input → gzip → chunk → [MANIFEST, DATA 0 … DATA N-1]
#                              or [MANIFEST, FOUNTAIN 0 … FOUNTAIN K-1]
# ---------------------------------------------------------------------------


def sha256hex(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def compress(data: bytes) -> bytes:
    """gzip with a zeroed mtime: the same input gives the same blob on a given zlib."""
    return gzip.compress(data, compresslevel=9, mtime=0)


def new_session(seed: int | None = None) -> int:
    """Sender session id: a random u32, or one derived from --seed for reproducible dumps."""
    if seed is None:
        return secrets.randbits(32)
    return random.Random(seed).getrandbits(32)


def encode(
    data: bytes,
    name: str,
    chunk: int,
    session: int,
    fountain: bool = False,
    packets: int | None = None,
) -> tuple[Manifest, list[Frame]]:
    if not 1 <= chunk <= 0xFFFF:
        raise ValueError(f"chunk must be 1..65535, got {chunk}")
    blob = compress(data)
    manifest = Manifest(
        name=name,
        gz_size=len(blob),
        gz_sha256=sha256hex(blob),
        orig_size=len(data),
        orig_sha256=sha256hex(data),
        chunk=chunk,
    )
    total = manifest.total
    if total > MAX_CHUNKS:
        raise ValueError(f"{total} chunks exceeds the u16 limit of {MAX_CHUNKS}; raise --chunk")
    frames = [Frame(T_MANIFEST, session, 0, total, manifest.to_json())]
    if fountain:
        k = default_packets(total) if packets is None else packets
        if not 1 <= k <= MAX_PACKETS:
            raise ValueError(f"fountain packets must be 1..{MAX_PACKETS}, got {k}")
        padded = _padded_chunks(blob, chunk, total)
        for seed in range(k):
            frames.append(
                Frame(T_FOUNTAIN, session, seed, total, fountain_payload(padded, chunk, seed))
            )
    else:
        for i in range(total):
            frames.append(Frame(T_DATA, session, i, total, blob[i * chunk : (i + 1) * chunk]))
    return manifest, frames


def schedule(total: int, every: int) -> list[int]:
    """Loop order as indices into the frame list (0 = MANIFEST, k+1 = frame k):
    [M, F0 … F(N-1)] with M re-inserted after every `every` frames."""
    if every < 1:
        raise ValueError("manifest-every must be >= 1")
    order = []
    for i in range(total):
        if i % every == 0:
            order.append(0)
        order.append(i + 1)
    return order


# ---------------------------------------------------------------------------
# Decode: the reference reassembly the Go tower must agree with.
# ---------------------------------------------------------------------------


@dataclass
class DecodeResult:
    session: int | None = None
    manifest: Manifest | None = None
    data: bytes | None = None
    accepted: int = 0  # distinct frames held for the bound sender session
    dup: int = 0  # repeats of a frame already held
    bad: int = 0  # undecodable, malformed or crc-failing frames
    foreign: int = 0  # frames from a sender session other than the bound one
    packets: int = 0  # fountain packets fed to the decoder
    pending: int = 0  # packets still unresolved when decoding stopped
    missing: list[int] = field(default_factory=list)
    gz_sha256: str | None = None  # actual, once all chunks are present
    orig_sha256: str | None = None  # actual, once gunzip succeeds
    error: str | None = None

    @property
    def ok(self) -> bool:
        return self.error is None and self.data is not None


def decode(texts: Iterable[str]) -> DecodeResult:
    """Order-independent: frames are held per sender session and deduplicated by
    (type, seq); the first MANIFEST seen binds the session. DATA chunks and
    FOUNTAIN packets feed one peeling decoder. Then: sha256 vs gz_sha256 →
    gunzip → sha256 vs orig_sha256."""
    r = DecodeResult()
    held: dict[int, dict[tuple[int, int], Frame]] = {}
    for text in texts:
        try:
            fr = Frame.from_text(text)
        except FrameError:
            r.bad += 1
            continue
        bucket = held.setdefault(fr.session, {})
        key = (fr.type, fr.seq)
        if key in bucket:
            r.dup += 1
            continue
        bucket[key] = fr
        if fr.type == T_MANIFEST and r.session is None:
            r.session = fr.session
    if r.session is None:
        r.error = "no manifest frame"
        return r
    frames = held[r.session]
    r.accepted = len(frames)
    r.foreign = sum(len(b) for s, b in held.items() if s != r.session)
    try:
        r.manifest = Manifest.from_json(frames[(T_MANIFEST, 0)].payload)
    except FrameError as exc:
        r.error = str(exc)
        return r
    m = r.manifest
    peeler = Peeler(m.total, m.chunk)
    for (ftype, seq), fr in frames.items():
        if fr.total != m.total:
            r.bad += 1
            r.accepted -= 1
            continue
        if ftype == T_DATA:
            if seq >= m.total or len(fr.payload) != m.chunk_len(seq):
                r.bad += 1
                r.accepted -= 1
                continue
            peeler.add([seq], fr.payload)
        elif ftype == T_FOUNTAIN:
            if len(fr.payload) != m.chunk:
                r.bad += 1
                r.accepted -= 1
                continue
            r.packets += 1
            peeler.add(fountain_indices(seq, m.total), fr.payload)
    r.pending = peeler.pending
    if not peeler.complete:
        r.missing = [i for i in range(m.total) if not peeler.have(i)]
        r.error = f"missing {len(r.missing)} of {m.total} chunks"
        return r
    blob = peeler.blob(m.gz_size)
    r.gz_sha256 = sha256hex(blob)
    if len(blob) != m.gz_size or r.gz_sha256 != m.gz_sha256:
        r.error = "gzip blob sha256 mismatch"
        return r
    try:
        data = gzip.decompress(blob)
    except (OSError, EOFError, zlib.error) as exc:
        r.error = f"gunzip: {exc}"
        return r
    r.orig_sha256 = sha256hex(data)
    if len(data) != m.orig_size or r.orig_sha256 != m.orig_sha256:
        r.error = "original sha256 mismatch"
        return r
    r.data = data
    return r


# ---------------------------------------------------------------------------
# Render: base45 text → QR (segno) → SVG path → HTML player
# ---------------------------------------------------------------------------

QUIET_ZONE = 4  # modules of white around the symbol, per ISO/IEC 18004


class BeamError(Exception):
    """A beam cannot be produced with the given parameters."""


def _segno():  # noqa: ANN202 - the module type is not worth importing at runtime
    try:
        import segno
    except ImportError:
        raise BeamError(
            "segno is required for `beam`: pip install segno, "
            "or copy the segno/ package next to airlift.py"
        ) from None
    return segno


def text_len(frame_bytes: int) -> int:
    """base45 characters for a frame of the given size."""
    return (frame_bytes // 2) * 3 + (2 if frame_bytes % 2 else 0)


def chunk_for_version(version: int, ecc: str) -> int:
    """The largest --chunk whose full DATA frame still fits QR `version` at `ecc`."""
    segno = _segno()
    if not 1 <= version <= 40:
        raise BeamError("--version-target must be 1..40")

    def fits(chunk: int) -> bool:
        try:
            segno.make(
                "0" * text_len(HEADER_LEN + chunk),
                error=ecc,
                mode="alphanumeric",
                micro=False,
                version=version,
            )
            return True
        except segno.DataOverflowError:
            return False

    lo, hi = 0, 2300
    while lo < hi:
        mid = (lo + hi + 1) // 2
        if fits(mid):
            lo = mid
        else:
            hi = mid - 1
    if lo < 1:
        raise BeamError(f"QR version {version} at ECC {ecc} cannot hold even a 1-byte chunk")
    return lo


def svg_path(matrix: Sequence[Sequence[int]], border: int = QUIET_ZONE) -> str:
    """Module matrix (1 = dark) → compact SVG path: one 1-unit-wide stroke per
    horizontal run of dark modules, relative moves in between."""
    parts: list[str] = []
    px = py = 0
    for y, row in enumerate(matrix):
        yy = y + border
        x, n = 0, len(row)
        while x < n:
            if not row[x]:
                x += 1
                continue
            x0 = x
            while x < n and row[x]:
                x += 1
            run = x - x0
            xx = x0 + border
            if parts:
                parts.append(f"m{xx - px} {yy - py}h{run}")
            else:
                parts.append(f"M{xx} {yy}.5h{run}")
            px, py = xx + run, yy
    return "".join(parts)


def render_qr(texts: Sequence[str], ecc: str) -> tuple[int, int, list[str]]:
    """Encode every frame at one QR version (the one the longest frame needs) so the
    symbol geometry never changes on screen; shorter frames get a free ECC boost.
    Returns (version, viewbox size in modules, one SVG path per frame)."""
    segno = _segno()
    ecc = ecc.upper()
    longest = max(texts, key=len)
    try:
        version = segno.make(longest, error=ecc, mode="alphanumeric", micro=False).version
    except segno.DataOverflowError:
        raise BeamError(
            f"a {len(longest)}-character frame does not fit QR version 40 at ECC {ecc}; "
            "reduce --chunk"
        ) from None
    paths = []
    for text in texts:
        qr = segno.make(text, error=ecc, mode="alphanumeric", micro=False, version=version)
        paths.append(svg_path(qr.matrix))
    modules = 17 + 4 * version
    return version, modules + 2 * QUIET_ZONE, paths


_PLAYER = string.Template(
    """<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>airlift beam · $title</title>
<style>
html,body{margin:0;height:100%;background:#fff;color:#000;overflow:hidden}
body{display:flex;flex-direction:column;
font:14px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
header{flex:none;display:flex;flex-wrap:wrap;gap:0 1.5em;padding:.3em 1em;color:#333}
header .hint{margin-left:auto;color:#999}
main{flex:1;min-height:0;display:flex;align-items:center;justify-content:center}
svg{display:block}
</style>
</head>
<body>
<header>
<span>$name</span>
<span>session $session</span>
<span>$mode</span>
<span id="frame"></span>
<span id="chunk"></span>
<span id="fps"></span>
<span id="state"></span>
<span class="hint">space pause · ←/→ step · +/- fps · f fullscreen</span>
</header>
<main id="main"><svg id="qr" viewBox="0 0 $size $size" shape-rendering="crispEdges">
<path id="path" stroke="#000" stroke-width="1" fill="none" d=""/></svg></main>
<script>
(function () {
  var FRAMES = $frames;
  var ORDER = $order;
  var N = $total, fps = $fps, LABEL = $label;
  var i = 0, playing = true, acc = 0, last = null;
  var main = document.getElementById('main'), svg = document.getElementById('qr');
  var path = document.getElementById('path');
  var hFrame = document.getElementById('frame'), hChunk = document.getElementById('chunk');
  var hFps = document.getElementById('fps'), hState = document.getElementById('state');
  var KEYS = {32: ' ', 37: 'ArrowLeft', 39: 'ArrowRight', 187: '+', 61: '+', 107: '+',
              189: '-', 173: '-', 109: '-', 70: 'f'};
  function fit() {
    var s = Math.min(main.clientWidth, main.clientHeight);
    svg.style.width = s + 'px';
    svg.style.height = s + 'px';
  }
  function show() {
    var k = ORDER[i];
    path.setAttribute('d', FRAMES[k]);
    hFrame.textContent = 'frame ' + (i + 1) + '/' + ORDER.length;
    hChunk.textContent = k === 0 ? 'manifest' : LABEL + ' ' + k + '/' + N;
  }
  function status() {
    hFps.textContent = fps + ' fps · ' + (ORDER.length / fps).toFixed(1) + ' s/pass';
    hState.textContent = playing ? 'playing' : 'paused';
  }
  function step(d) {
    playing = false;
    acc = 0;
    i = (i + d + ORDER.length) % ORDER.length;
    show();
  }
  function tick(ts) {
    if (last !== null && playing) {
      acc += ts - last;
      var period = 1000 / fps;
      if (acc >= period) {
        acc = Math.min(acc - period, period);
        i = (i + 1) % ORDER.length;
        show();
      }
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
      else if (document.documentElement.requestFullscreen) {
        document.documentElement.requestFullscreen();
      }
    }
    else { return; }
    e.preventDefault();
    status();
  });
  window.addEventListener('resize', fit);
  fit();
  show();
  status();
  window.requestAnimationFrame(tick);
  if (navigator.wakeLock && navigator.wakeLock.request) {
    var lock = function () { navigator.wakeLock.request('screen').catch(function () {}); };
    lock();
    document.addEventListener('visibilitychange', function () {
      if (!document.hidden) { lock(); }
    });
  }
})();
</script>
</body>
</html>
"""
)


def player_html(
    name: str,
    session: int,
    total: int,
    order: Sequence[int],
    paths: Sequence[str],
    size: int,
    fps: int,
    fountain: bool = False,
) -> str:
    """One self-contained page: inline SVG path per frame, inline player, no assets."""

    def js(obj: object) -> str:
        return json.dumps(obj, separators=(",", ":")).replace("</", "<\\/")

    return _PLAYER.substitute(
        title=html.escape(name),
        name=html.escape(name),
        session=f"{session:08x}",
        mode="fountain" if fountain else "sequential",
        size=size,
        frames=js(list(paths)),
        order=js(list(order)),
        total=total,
        fps=fps,
        label=js("packet" if fountain else "chunk"),
    )


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def _read(path: str) -> bytes:
    with open(path, "rb") as fh:
        return fh.read()


def _human(n: float) -> str:
    for unit in ("B", "KB", "MB", "GB"):
        if n < 1024 or unit == "GB":
            return f"{n:.0f} {unit}" if unit == "B" else f"{n:.1f} {unit}"
        n /= 1024
    return f"{n:.1f} GB"


def _chunk_from_args(a: argparse.Namespace) -> int:
    if getattr(a, "version_target", None):
        chunk = chunk_for_version(a.version_target, a.ecc)
        print(f"  target   QR version {a.version_target} at ECC {a.ecc} → chunk {chunk} bytes")
        return chunk
    return a.chunk


def cmd_beam(a: argparse.Namespace) -> int:
    data = _read(a.inp)
    name = os.path.basename(a.inp)
    session = new_session(a.seed)
    print(f"airlift beam  {name} → {a.out}")
    chunk = _chunk_from_args(a)
    manifest, frames = encode(data, name, chunk, session, a.fountain, a.fountain_packets)
    texts = [f.text() for f in frames]
    version, size, paths = render_qr(texts, a.ecc)
    order = schedule(len(frames) - 1, a.manifest_every)
    doc = player_html(name, session, len(frames) - 1, order, paths, size, a.fps, a.fountain)
    with open(a.out, "w", encoding="utf-8") as fh:
        fh.write(doc)
    ratio = 100.0 * manifest.gz_size / manifest.orig_size if manifest.orig_size else 0.0
    modules = 17 + 4 * version
    print(f"  input    {manifest.orig_size:>10} bytes   sha256 {manifest.orig_sha256[:16]}…")
    print(f"  gzip     {manifest.gz_size:>10} bytes   {ratio:.1f} % of input")
    print(f"  chunks   {manifest.total:>10} × {chunk} bytes")
    if a.fountain:
        print(f"  fountain {len(frames) - 1:>10} packets   (LT, robust soliton)")
    print(f"  qr       version {version} ({modules}×{modules} modules), ECC {a.ecc}, alphanumeric")
    print(
        f"  loop     {len(order):>10} frames   manifest every {a.manifest_every}   "
        f"{len(order) / a.fps:.1f} s per pass at {a.fps} fps"
    )
    print(f"  session  0x{session:08x}")
    print(f"  output   {_human(len(doc.encode('utf-8')))}")
    return 0


def cmd_frames(a: argparse.Namespace) -> int:
    data = _read(a.inp)
    session = new_session(a.seed)
    manifest, frames = encode(
        data, os.path.basename(a.inp), a.chunk, session, a.fountain, a.fountain_packets
    )
    dump: dict[str, object] = {
        "sender_session": session,
        "manifest": json.loads(manifest.to_json()),
        "frames": [f.text() for f in frames],
    }
    if a.fountain:
        dump["fountain"] = {
            "packets": len(frames) - 1,
            "indices": [fountain_indices(seed, manifest.total) for seed in range(len(frames) - 1)],
        }
    with open(a.out, "w", encoding="utf-8") as fh:
        json.dump(dump, fh, indent=1)
        fh.write("\n")
    kind = f"{len(frames) - 1} fountain packets" if a.fountain else f"{len(frames) - 1} data frames"
    print(
        f"wrote {a.out}: manifest + {kind} ({manifest.total} chunks × {a.chunk} bytes), "
        f"session 0x{session:08x}"
    )
    return 0


def cmd_decode(a: argparse.Namespace) -> int:
    with open(a.frames, encoding="utf-8") as fh:
        dump = json.load(fh)
    texts = dump.get("frames") if isinstance(dump, dict) else None
    if not isinstance(texts, list) or not all(isinstance(t, str) for t in texts):
        print(f"{a.frames}: expected an object with a 'frames' list of strings", file=sys.stderr)
        return 2
    r = decode(texts)
    print(
        f"frames   {len(texts)}: accepted {r.accepted}, dup {r.dup}, "
        f"bad {r.bad}, foreign {r.foreign}"
        + (f", fountain packets {r.packets}" if r.packets else "")
    )
    if r.session is not None:
        print(f"session  0x{r.session:08x}")
    if r.manifest is not None:
        m = r.manifest
        print(
            f"manifest {m.name}: {m.total} chunks × {m.chunk} bytes, "
            f"{m.gz_size} → {m.orig_size} bytes"
        )
    if not r.ok:
        print(f"FAILED   {r.error}", file=sys.stderr)
        if r.missing:
            shown = ", ".join(map(str, r.missing[:10])) + (" …" if len(r.missing) > 10 else "")
            print(f"missing  {shown}  ({r.pending} packets unresolved)", file=sys.stderr)
        if r.gz_sha256 is not None and r.manifest is not None:
            print(f"gz_sha256    expected {r.manifest.gz_sha256}")
            print(f"             actual   {r.gz_sha256}")
        if r.orig_sha256 is not None and r.manifest is not None:
            print(f"orig_sha256  expected {r.manifest.orig_sha256}")
            print(f"             actual   {r.orig_sha256}")
        return 1
    assert r.data is not None and r.manifest is not None
    with open(a.out, "wb") as fh:
        fh.write(r.data)
    print(f"gz_sha256    OK {r.gz_sha256}")
    print(f"orig_sha256  OK {r.orig_sha256}")
    print(f"wrote    {a.out} ({len(r.data)} bytes)")
    return 0


def build_parser() -> argparse.ArgumentParser:
    ap = argparse.ArgumentParser(
        prog="airlift",
        description="Render a file as an animated QR loop for optical transfer out of an air gap.",
    )
    ap.add_argument("--version", action="version", version=f"airlift {__version__}")
    sub = ap.add_subparsers(dest="cmd", required=True)

    def common(p: argparse.ArgumentParser) -> None:
        p.add_argument("--in", dest="inp", required=True, metavar="FILE", help="file to send")
        p.add_argument(
            "--chunk",
            type=int,
            default=600,
            metavar="BYTES",
            help="payload bytes per frame (default 600; at most 2242 at ECC M)",
        )
        p.add_argument(
            "--seed",
            type=int,
            default=None,
            metavar="N",
            help="derive the sender session id from N instead of at random",
        )
        p.add_argument(
            "--fountain",
            action="store_true",
            help="emit LT fountain packets instead of sequential chunks",
        )
        p.add_argument(
            "--fountain-packets",
            type=int,
            default=None,
            metavar="K",
            help="fountain packets to emit (default 1.5 N + 16, at most 65536)",
        )

    b = sub.add_parser("beam", help="write a self-contained HTML player for FILE")
    common(b)
    b.add_argument("--out", default="beam.html", metavar="HTML", help="output (default beam.html)")
    b.add_argument("--ecc", default="M", type=str.upper, choices=list("LMQH"), help="QR ECC level")
    b.add_argument(
        "--version-target",
        type=int,
        default=None,
        metavar="V",
        help="pick the largest chunk that fits QR version V (1..40) instead of --chunk",
    )
    b.add_argument("--fps", type=int, default=8, help="initial frames per second (1..60)")
    b.add_argument(
        "--manifest-every",
        type=int,
        default=20,
        metavar="K",
        help="re-insert the manifest frame after every K frames",
    )

    f = sub.add_parser("frames", help="dump {sender_session, manifest, frames} as JSON")
    common(f)
    f.add_argument("--out", default="frames.json", metavar="JSON")

    d = sub.add_parser("decode", help="reassemble a file from a frames dump")
    d.add_argument("--frames", required=True, metavar="JSON", help="dump written by `frames`")
    d.add_argument("--out", required=True, metavar="FILE")
    return ap


def main(argv: list[str] | None = None) -> int:
    a = build_parser().parse_args(argv)
    try:
        if a.cmd == "beam":
            if not 1 <= a.fps <= 60:
                raise BeamError("--fps must be 1..60")
            if a.manifest_every < 1:
                raise BeamError("--manifest-every must be >= 1")
            return cmd_beam(a)
        if a.cmd == "frames":
            return cmd_frames(a)
        return cmd_decode(a)
    except (BeamError, ValueError, OSError) as exc:
        print(f"airlift {a.cmd}: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
