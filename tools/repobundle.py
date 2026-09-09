#!/usr/bin/env python3
"""
repobundle.py — pack a repo into one text file and unpack it back.

Goal: carry a repo across an air-gapped boundary with a single copy-paste.

pack:
  - Includes exactly the files git would (respects .gitignore) PLUS the ignore
    files themselves (.gitignore/.dockerignore are tracked, so they're included).
    Falls back to a plain walk (skipping .git) if not a git repo.
  - text format  (default): human-readable; skips binary files.
  - base64 format          : copy-paste-proof (survives tab/space/rewrapping
    mangling and includes binaries). RECOMMENDED for the actual transfer.
  - Every file carries a sha256, so unpack DETECTS any copy-paste corruption.

Usage:
  python3 repobundle.py pack   [--root DIR] [--out FILE] [--format text|base64]
  python3 repobundle.py unpack [--in FILE]  [--dest DIR] [--dry-run]

Examples:
  # in the air-gapped repo root:
  python3 repobundle.py pack --format base64 --out repo-bundle.txt
  # copy-paste repo-bundle.txt across, then in an empty dir:
  python3 repobundle.py unpack --in repo-bundle.txt --dest ./restored
"""
import argparse
import base64
import hashlib
import os
import subprocess
import sys

MAGIC = "#repobundle v1"
BOUND = "@@@FILE@@@"          # line-start marker; never appears in base64, rare in source
END = "@@@END@@@"


def list_files(root):
    """Files git would track/keep (respects .gitignore, includes tracked ignore files)."""
    try:
        tracked = subprocess.run(
            ["git", "-C", root, "ls-files", "-z"],
            check=True, capture_output=True).stdout
        others = subprocess.run(
            ["git", "-C", root, "ls-files", "-z", "--others", "--exclude-standard"],
            check=True, capture_output=True).stdout
        rels = [p for p in (tracked + others).split(b"\x00") if p]
        seen, out = set(), []
        for b in rels:
            r = b.decode("utf-8", "surrogateescape")
            if r not in seen:
                seen.add(r)
                out.append(r)
        return sorted(out), True
    except Exception:
        # fallback: walk everything except .git
        out = []
        for dp, dns, fns in os.walk(root):
            dns[:] = [d for d in dns if d != ".git"]
            for fn in fns:
                out.append(os.path.relpath(os.path.join(dp, fn), root))
        return sorted(out), False


def is_binary(data):
    if b"\x00" in data:
        return True
    try:
        data.decode("utf-8")
        return False
    except UnicodeDecodeError:
        return True


def resolve_explicit(root, paths):
    """Normalise an explicit path list to unique files relative to root."""
    seen, out = set(), []
    for p in paths:
        ap = os.path.abspath(p if os.path.isabs(p) else os.path.join(root, p))
        if not os.path.isfile(ap):
            sys.exit(f"ERROR: not a file: {p}")
        rel = os.path.relpath(ap, root)
        if rel.startswith(".."):
            sys.exit(f"ERROR: {p} is outside --root ({root}); set --root to a common parent.")
        if rel not in seen:
            seen.add(rel)
            out.append(rel)
    return out


def pack(root, out, fmt, explicit=None):
    if explicit:
        files, source = explicit, "explicit-list"
    else:
        files, used_git = list_files(root)
        source = "git" if used_git else "walk-fallback"
    out_abs = os.path.abspath(out)
    packed, skipped = 0, []
    with open(out, "wb") as f:
        f.write(f"{MAGIC} format={fmt}\n".encode())
        for rel in files:
            path = os.path.join(root, rel)
            if not os.path.isfile(path) or os.path.islink(path):
                continue
            if os.path.abspath(path) == out_abs:  # never bundle our own output file
                continue
            with open(path, "rb") as fh:
                data = fh.read()
            if fmt == "text":
                if is_binary(data):
                    skipped.append(rel)
                    continue
                if any(line.startswith(BOUND.encode()) or line.startswith(END.encode())
                       for line in data.split(b"\n")):
                    sys.exit(f"ERROR: {rel} contains the boundary marker; "
                             f"re-run with --format base64.")
                payload = data
            else:
                payload = base64.b64encode(data)  # ascii, wrap for readability
                payload = b"\n".join(payload[i:i + 120] for i in range(0, len(payload), 120))
            sha = hashlib.sha256(data).hexdigest()
            mode = oct(os.stat(path).st_mode & 0o777)[2:]
            f.write(f"{BOUND} {len(data)} {sha} {mode} {rel}\n".encode("utf-8", "surrogateescape"))
            f.write(payload)
            f.write(b"\n")
            packed += 1
        f.write(f"{END}\n".encode())
    print(f"packed {packed} files -> {out} ({os.path.getsize(out)} bytes, format={fmt}, {source})")
    if skipped:
        print(f"skipped {len(skipped)} binary file(s) (use --format base64 to include them):")
        for r in skipped:
            print(f"  - {r}")


def _next_boundary(data, start):
    """Index of the next line starting with BOUND/END at or after `start`."""
    i = start
    n = len(data)
    while i < n:
        if data[i:i + len(BOUND)] == BOUND.encode() or data[i:i + len(END)] == END.encode():
            if i == 0 or data[i - 1:i] == b"\n":
                return i
        nl = data.find(b"\n", i)
        if nl == -1:
            return n
        i = nl + 1
    return n


def unpack(inp, dest, dry):
    data = open(inp, "rb").read()
    nl = data.find(b"\n")
    header = data[:nl].decode("utf-8", "surrogateescape") if nl != -1 else ""
    if not header.startswith(MAGIC):
        sys.exit("ERROR: not a repobundle file (missing header).")
    fmt = "text"
    for tok in header.split():
        if tok.startswith("format="):
            fmt = tok.split("=", 1)[1]
    pos = nl + 1
    written, bad = 0, []
    while pos < len(data):
        if data[pos:pos + len(END)] == END.encode():
            break
        if data[pos:pos + len(BOUND)] != BOUND.encode():
            pos = data.find(b"\n", pos)
            if pos == -1:
                break
            pos += 1
            continue
        hdr_nl = data.find(b"\n", pos)
        hdr = data[pos:hdr_nl].decode("utf-8", "surrogateescape")
        # BOUND nbytes sha mode relpath   (relpath may contain spaces -> split 5)
        _, nbytes_s, sha, mode_s, rel = hdr.split(" ", 4)
        nbytes = int(nbytes_s)
        cstart = hdr_nl + 1
        bstart = _next_boundary(data, cstart)
        region = data[cstart:bstart]
        if fmt == "base64":
            content = base64.b64decode(b"".join(region.split()))
        else:
            content = region[:nbytes]  # exact length; ignore the cosmetic trailing \n
        ok = (len(content) == nbytes and hashlib.sha256(content).hexdigest() == sha)
        outp = os.path.join(dest, rel)
        if not dry:
            os.makedirs(os.path.dirname(outp) or ".", exist_ok=True)
            with open(outp, "wb") as fh:
                fh.write(content)
            try:
                os.chmod(outp, int(mode_s, 8))
            except OSError:
                pass
        print(f"  {'OK ' if ok else 'BAD'}  {rel}")
        if not ok:
            bad.append(rel)
        written += 1
        pos = bstart
    print(f"{'(dry-run) ' if dry else ''}restored {written} files into {dest}")
    if bad:
        print(f"WARNING: {len(bad)} file(s) failed the sha256 check (copy-paste corruption?): {bad}")
        sys.exit(1)


def main():
    ap = argparse.ArgumentParser(description="pack/unpack a repo as one text file")
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("pack")
    p.add_argument("paths", nargs="*",
                   help="explicit files to bundle (abs or relative to --root). "
                        "If omitted, the whole repo is bundled (git-aware).")
    p.add_argument("--files-from", help="read the file list from this file, one path per line (# comments ok)")
    p.add_argument("--root", default=".")
    p.add_argument("--out", default="repo-bundle.txt")
    p.add_argument("--format", choices=["text", "base64"], default="text")
    u = sub.add_parser("unpack")
    u.add_argument("--in", dest="inp", default="repo-bundle.txt")
    u.add_argument("--dest", default=".")
    u.add_argument("--dry-run", action="store_true")
    a = ap.parse_args()
    if a.cmd == "pack":
        root = os.path.abspath(a.root)
        explicit = list(a.paths)
        if a.files_from:
            with open(a.files_from) as fh:
                explicit += [ln.strip() for ln in fh if ln.strip() and not ln.lstrip().startswith("#")]
        pack(root, a.out, a.format, resolve_explicit(root, explicit) if explicit else None)
    else:
        unpack(a.inp, os.path.abspath(a.dest), a.dry_run)


if __name__ == "__main__":
    main()
