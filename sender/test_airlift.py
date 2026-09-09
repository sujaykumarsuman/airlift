"""Tests for airlift.py. Run with `uv run pytest` from sender/."""

from __future__ import annotations

import json
import random
import re
import zlib
from pathlib import Path

import pytest

import airlift as al

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent
REPOBUNDLE = ROOT / "tools" / "repobundle.py"
VECTORS = HERE / "testdata" / "vectors.json"
VECTORS_INPUT = ROOT / "testdata" / "bundles" / "multi" / "bundle-base64.txt"
VECTORS_SEED = 1
SESSION = 0x01020304


def _texts(data: bytes, chunk: int = 600, session: int = SESSION, name: str = "blob"):
    manifest, frames = al.encode(data, name, chunk, session)
    return manifest, [f.text() for f in frames]


# ---- base45 ---------------------------------------------------------------

RFC9285 = [
    (b"AB", "BB8"),
    (b"Hello!!", "%69 VD92EX0"),
    (b"base-45", "UJCLQE7W581"),
    (b"ietf!", "QED8WEX0"),
    (b"", ""),
]


@pytest.mark.parametrize(("raw", "text"), RFC9285)
def test_base45_known_vectors(raw, text):
    assert al.b45encode(raw) == text
    assert al.b45decode(text) == raw


def test_base45_round_trip_every_length():
    rng = random.Random(45)
    for n in range(80):
        data = rng.randbytes(n)
        text = al.b45encode(data)
        assert len(text) == (n // 2) * 3 + (2 if n % 2 else 0)
        assert set(text) <= set(al.B45_ALPHABET)
        assert al.b45decode(text) == data
    assert al.b45decode(al.b45encode(b"\xff\xff\xff")) == b"\xff\xff\xff"
    assert al.b45decode(al.b45encode(b"\x00\x00\x00")) == b"\x00\x00\x00"


def test_base45_alphabet_is_qr_alphanumeric():
    assert al.B45_ALPHABET == "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:"
    assert len(set(al.B45_ALPHABET)) == 45


@pytest.mark.parametrize("bad", ["A", "GGGA", "abc", ":::", "::", "BB8\n"])
def test_base45_rejects_malformed(bad):
    with pytest.raises(ValueError):
        al.b45decode(bad)


# ---- crc / frames ---------------------------------------------------------


def test_crc32_is_ieee():
    assert zlib.crc32(b"123456789") == 0xCBF43926
    raw = al.Frame(al.T_DATA, SESSION, 0, 1, b"123456789").pack()
    assert raw[14:18] == (0xCBF43926).to_bytes(4, "big")


def test_frame_header_layout():
    fr = al.Frame(al.T_DATA, 0xDEADBEEF, 7, 9, b"hello")
    raw = fr.pack()
    assert len(raw) == al.HEADER_LEN + 5 == 23
    assert raw[0:2] == b"AL" and raw[2] == 1 and raw[3] == al.T_DATA
    assert int.from_bytes(raw[4:8], "big") == 0xDEADBEEF
    assert int.from_bytes(raw[8:10], "big") == 7
    assert int.from_bytes(raw[10:12], "big") == 9
    assert int.from_bytes(raw[12:14], "big") == 5
    assert int.from_bytes(raw[14:18], "big") == zlib.crc32(b"hello")
    assert raw[18:] == b"hello"
    assert al.Frame.parse(raw) == fr
    assert al.Frame.from_text(fr.text()) == fr


def test_frame_text_is_qr_alphanumeric():
    text = al.Frame(al.T_DATA, SESSION, 1, 2, bytes(range(256))).text()
    assert set(text) <= set(al.B45_ALPHABET)


def _corrupt(raw: bytes, offset: int, value: int) -> bytes:
    b = bytearray(raw)
    b[offset] = value
    return bytes(b)


@pytest.mark.parametrize(
    ("mutate", "message"),
    [
        (lambda r: r[:10], "short header"),
        (lambda r: _corrupt(r, 0, 0x00), "bad magic"),
        (lambda r: _corrupt(r, 2, 2), "unsupported version"),
        (lambda r: _corrupt(r, 3, 9), "unknown frame type"),
        (lambda r: r[:-1], "length mismatch"),
        (lambda r: r + b"x", "length mismatch"),
        (lambda r: _corrupt(r, 18, r[18] ^ 0xFF), "crc mismatch"),
        (lambda r: _corrupt(r, 17, r[17] ^ 0x01), "crc mismatch"),
    ],
)
def test_frame_rejects_corruption(mutate, message):
    raw = al.Frame(al.T_DATA, SESSION, 3, 4, b"payload").pack()
    with pytest.raises(al.FrameError, match=message):
        al.Frame.parse(mutate(raw))


def test_frame_from_text_rejects_bad_base45():
    with pytest.raises(al.FrameError):
        al.Frame.from_text("not base45!")


# ---- manifest -------------------------------------------------------------


def _manifest_json(**over: object) -> bytes:
    obj = {
        "name": "x",
        "gz_size": 10,
        "gz_sha256": "a" * 64,
        "orig_size": 20,
        "orig_sha256": "b" * 64,
        "chunk": 600,
    }
    obj.update(over)
    return json.dumps(obj).encode()


def test_manifest_json_is_compact_and_ordered():
    m = al.Manifest("a b.txt", 1201, "a" * 64, 5000, "b" * 64, 600)
    assert m.to_json() == (
        b'{"name":"a b.txt","gz_size":1201,"gz_sha256":"'
        + b"a" * 64
        + b'","orig_size":5000,"orig_sha256":"'
        + b"b" * 64
        + b'","chunk":600}'
    )
    assert al.Manifest.from_json(m.to_json()) == m
    assert m.total == 3
    assert al.Manifest("x", 600, "a" * 64, 1, "b" * 64, 600).total == 1
    assert al.Manifest("x", 601, "a" * 64, 1, "b" * 64, 600).total == 2


@pytest.mark.parametrize(
    "raw",
    [
        b"",
        b"nope",
        b"[]",
        b'{"name":"x"}',
        _manifest_json(gz_sha256="short"),
        _manifest_json(chunk=0),
        _manifest_json(gz_size=-1),
        _manifest_json(orig_size="many"),
    ],
)
def test_manifest_rejects_garbage(raw):
    with pytest.raises(al.FrameError):
        al.Manifest.from_json(raw)


# ---- schedule -------------------------------------------------------------


def test_schedule_reinserts_manifest():
    assert al.schedule(45, 20) == [0, *range(1, 21), 0, *range(21, 41), 0, *range(41, 46)]
    assert al.schedule(20, 20) == [0, *range(1, 21)]
    assert al.schedule(1, 20) == [0, 1]
    assert al.schedule(3, 1) == [0, 1, 0, 2, 0, 3]
    assert al.schedule(0, 20) == []
    with pytest.raises(ValueError):
        al.schedule(5, 0)


# ---- encode / decode ------------------------------------------------------


def test_round_trip_random_50kb():
    data = random.Random(7).randbytes(50 * 1024)
    manifest, texts = _texts(data, name="noise.bin")
    assert manifest.name == "noise.bin"
    assert manifest.orig_size == len(data) and manifest.orig_sha256 == al.sha256hex(data)
    assert manifest.gz_size == len(al.compress(data)) and manifest.chunk == 600
    assert len(texts) == manifest.total + 1
    first = al.Frame.from_text(texts[0])
    assert (first.type, first.seq, first.total) == (al.T_MANIFEST, 0, manifest.total)
    assert al.Manifest.from_json(first.payload) == manifest
    last = al.Frame.from_text(texts[-1])
    assert (last.type, last.seq) == (al.T_DATA, manifest.total - 1)
    assert len(last.payload) == manifest.gz_size - 600 * (manifest.total - 1)
    assert all(len(al.Frame.from_text(t).payload) == 600 for t in texts[1:-1])
    r = al.decode(texts)
    assert r.ok and r.data == data
    assert (r.accepted, r.dup, r.bad, r.foreign, r.missing) == (len(texts), 0, 0, 0, [])
    assert (r.gz_sha256, r.orig_sha256) == (manifest.gz_sha256, manifest.orig_sha256)


def test_round_trip_repobundle_py_shuffled_with_noise():
    data = REPOBUNDLE.read_bytes()
    _, texts = _texts(data, name="repobundle.py")
    _, other = _texts(b"another sender", session=SESSION + 1)
    feed = texts * 2 + [other[1], "GARBAGE", "", texts[1][:-1] + "!"]
    random.Random(3).shuffle(feed)
    r = al.decode(feed)
    assert r.ok and r.data == data
    assert r.session == SESSION
    assert (r.accepted, r.dup, r.bad, r.foreign) == (len(texts), len(texts), 3, 1)


def test_round_trip_empty_file():
    _, texts = _texts(b"")
    assert len(texts) == 2
    r = al.decode(texts)
    assert r.ok and r.data == b""


def test_decode_binds_first_manifest():
    _, a = _texts(b"A" * 10, session=1)
    _, b = _texts(b"B" * 10, session=2)
    r = al.decode(b[:1] + a)
    assert not r.ok and r.session == 2 and r.missing == [0] and r.foreign == len(a)


def test_decode_reports_missing_chunks():
    data = random.Random(8).randbytes(5000)
    _, texts = _texts(data, chunk=500)
    feed = [t for k, t in enumerate(texts) if k not in (4, 6)]  # drop DATA 3 and DATA 5
    r = al.decode(feed)
    assert not r.ok and r.missing == [3, 5] and r.data is None
    assert r.error is not None and r.error.startswith("missing 2 of")


def test_decode_detects_corrupted_chunk_with_valid_crc():
    data = random.Random(9).randbytes(3000)
    manifest, frames = al.encode(data, "x", 500, SESSION)
    victim = frames[2]
    evil = al.Frame(
        victim.type,
        victim.session,
        victim.seq,
        victim.total,
        bytes([victim.payload[0] ^ 1]) + victim.payload[1:],
    )
    texts = [f.text() for f in frames[:2]] + [evil.text()] + [f.text() for f in frames[3:]]
    r = al.decode(texts)
    assert not r.ok and r.error == "gzip blob sha256 mismatch" and r.data is None
    assert r.gz_sha256 is not None and r.gz_sha256 != manifest.gz_sha256


def test_decode_without_manifest():
    _, texts = _texts(b"hello")
    r = al.decode(texts[1:])
    assert not r.ok and r.error == "no manifest frame"


def test_encode_limits():
    with pytest.raises(ValueError):
        al.encode(b"x", "x", 0, SESSION)
    with pytest.raises(ValueError):
        al.encode(b"x", "x", 70000, SESSION)
    with pytest.raises(ValueError, match="u16"):
        al.encode(random.Random(1).randbytes(70000), "x", 1, SESSION)


def test_new_session():
    assert al.new_session(1) == al.new_session(1)
    assert al.new_session(1) != al.new_session(2)
    assert 0 <= al.new_session() <= 0xFFFFFFFF


# ---- rendering ------------------------------------------------------------


def test_svg_path_runs():
    matrix = [[1, 0, 1, 1], [0, 0, 0, 0], [1, 1, 1, 1]]
    assert al.svg_path(matrix, border=1) == "M1 1.5h1m1 0h2m-4 2h4"
    assert al.svg_path([[0, 0], [0, 0]]) == ""
    assert al.svg_path([[1]], border=0) == "M0 0.5h1"


def test_render_qr_uses_one_version_and_alnum_mode():
    segno = pytest.importorskip("segno")
    _, texts = _texts(random.Random(2).randbytes(2500))  # manifest + 4 full + 1 short
    version, size, paths = al.render_qr(texts, "M")
    assert len(paths) == len(texts) and all(p.startswith("M") for p in paths)
    longest = max(texts, key=len)
    auto = segno.make(longest, error="M", micro=False)  # no mode given: must auto-select
    assert auto.mode == "alphanumeric" and auto.version == version
    assert size == 17 + 4 * version + 2 * al.QUIET_ZONE
    assert paths[0].startswith(f"M{al.QUIET_ZONE} {al.QUIET_ZONE}.5h7")  # top-left finder
    dys = [int(m) for m in re.findall(r"m-?\d+ (-?\d+)h", paths[0])]
    assert sum(dys) == 17 + 4 * version - 1  # first to last row spans the whole symbol


def test_render_qr_rejects_oversized_frames():
    _, texts = _texts(random.Random(2).randbytes(6000), chunk=3000)
    with pytest.raises(al.BeamError, match="reduce --chunk"):
        al.render_qr(texts, "M")


# ---- CLI ------------------------------------------------------------------


def test_beam_writes_self_contained_player(tmp_path, capsys):
    src = tmp_path / "in.txt"
    src.write_bytes(random.Random(4).randbytes(1500))
    out = tmp_path / "beam.html"
    argv = ["beam", "--in", str(src), "--out", str(out), "--seed", "3", "--chunk", "300"]
    assert al.main(argv + ["--fps", "5", "--manifest-every", "2"]) == 0
    doc = out.read_text(encoding="utf-8")
    manifest, _ = al.encode(src.read_bytes(), "in.txt", 300, al.new_session(3))
    frames = re.search(r"var FRAMES = (\[.*?\]);", doc)
    order = re.search(r"var ORDER = (\[.*?\]);", doc)
    assert frames and order
    assert len(json.loads(frames.group(1))) == manifest.total + 1
    assert json.loads(order.group(1)) == al.schedule(manifest.total, 2)
    assert f"session {al.new_session(3):08x}" in doc
    assert f"var N = {manifest.total}, fps = 5;" in doc
    assert "<script>" in doc and "requestAnimationFrame" in doc
    for needle in ("href", "src=", "url(", "@import", "http:", "https:"):
        assert needle not in doc
    stdout = capsys.readouterr().out
    assert "qr       version" in stdout
    assert "× 300 bytes" in stdout and "s per pass at 5 fps" in stdout


def test_beam_rejects_bad_parameters(tmp_path, capsys):
    src = tmp_path / "in.txt"
    src.write_bytes(random.Random(6).randbytes(6000))  # incompressible: 2 × 3000-byte chunks
    out = str(tmp_path / "beam.html")
    assert al.main(["beam", "--in", str(src), "--out", out, "--fps", "0"]) == 2
    assert al.main(["beam", "--in", str(src), "--out", out, "--chunk", "3000"]) == 2
    assert al.main(["beam", "--in", str(tmp_path / "missing"), "--out", out]) == 2
    assert "reduce --chunk" in capsys.readouterr().err


def test_frames_dump_and_decode_cli(tmp_path):
    src = tmp_path / "input.bin"
    src.write_bytes(random.Random(5).randbytes(4000))
    dump = tmp_path / "frames.json"
    assert al.main(["frames", "--in", str(src), "--out", str(dump), "--seed", "5"]) == 0
    obj = json.loads(dump.read_text())
    assert set(obj) == {"sender_session", "manifest", "frames"}
    assert obj["sender_session"] == al.new_session(5)
    first = al.Frame.from_text(obj["frames"][0])
    assert first.type == al.T_MANIFEST and first.session == obj["sender_session"]
    assert obj["manifest"] == json.loads(first.payload)
    assert list(obj["manifest"]) == [
        "name",
        "gz_size",
        "gz_sha256",
        "orig_size",
        "orig_sha256",
        "chunk",
    ]
    assert obj["manifest"]["name"] == "input.bin"
    assert len(obj["frames"]) == al.Manifest.from_json(first.payload).total + 1
    again = tmp_path / "again.json"
    assert al.main(["frames", "--in", str(src), "--out", str(again), "--seed", "5"]) == 0
    assert again.read_bytes() == dump.read_bytes()
    restored = tmp_path / "restored.bin"
    assert al.main(["decode", "--frames", str(dump), "--out", str(restored)]) == 0
    assert restored.read_bytes() == src.read_bytes()


def test_decode_cli_fails_on_missing_chunk(tmp_path, capsys):
    obj = json.loads(VECTORS.read_text())
    del obj["frames"][5]
    dump = tmp_path / "d.json"
    dump.write_text(json.dumps(obj))
    out = tmp_path / "x"
    assert al.main(["decode", "--frames", str(dump), "--out", str(out)]) == 1
    assert "missing 1 of" in capsys.readouterr().err
    assert not out.exists()


def test_decode_cli_rejects_malformed_dump(tmp_path):
    dump = tmp_path / "d.json"
    dump.write_text('{"frames": "nope"}')
    assert al.main(["decode", "--frames", str(dump), "--out", str(tmp_path / "x")]) == 2


# ---- committed vectors ----------------------------------------------------


def test_vectors_decode_to_committed_bundle():
    obj = json.loads(VECTORS.read_text())
    r = al.decode(obj["frames"])
    assert r.ok and r.manifest is not None
    assert r.data == VECTORS_INPUT.read_bytes()
    assert r.session == obj["sender_session"] == al.new_session(VECTORS_SEED)
    assert json.loads(al.Frame.from_text(obj["frames"][0]).payload) == obj["manifest"]
    assert r.manifest.name == VECTORS_INPUT.name
    assert len(obj["frames"]) == r.manifest.total + 1


def test_vectors_regenerate_from_seed():
    data = VECTORS_INPUT.read_bytes()
    manifest, frames = al.encode(data, VECTORS_INPUT.name, 600, al.new_session(VECTORS_SEED))
    obj = json.loads(VECTORS.read_text())
    committed = obj["manifest"]
    assert obj["sender_session"] == frames[0].session
    assert (committed["name"], committed["orig_size"], committed["orig_sha256"]) == (
        manifest.name,
        manifest.orig_size,
        manifest.orig_sha256,
    )
    assert committed["chunk"] == manifest.chunk == 600
    if committed["gz_sha256"] != manifest.gz_sha256:
        pytest.skip("this zlib emits a different gzip stream; the committed vectors stay valid")
    assert obj["frames"] == [f.text() for f in frames]
