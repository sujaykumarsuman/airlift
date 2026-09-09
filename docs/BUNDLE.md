# Repobundle format

A repobundle is one text file that carries a folder — the primary payload
airlift moves across the air gap in a single copy-paste. `airlift pack` writes
one; `airlift unpack`, and the tower's bundle stage after verification, read
one. The format is a byte-for-byte port of the retired `tools/repobundle.py`
(ADR 0010); the four fixtures under `testdata/bundles/` pin it, and
`internal/bundle` is the reference implementation.

## Layout

```
#repobundle v1 format=<text|base64>\n
@@@FILE@@@ <nbytes> <sha256> <mode> <relpath>\n
<payload>\n
@@@FILE@@@ …\n
<payload>\n
@@@END@@@\n
```

- **Header** — the exact bytes `#repobundle v1 format=text` or
  `#repobundle v1 format=base64`, then a newline. Any token after the magic of
  the form `key=value` is read; only `format` is defined.
- **Entry header** — `@@@FILE@@@`, then single-space-separated: the original
  byte length, the lowercase hex sha256 of the original bytes, the octal
  permission bits (`644`, `755`), and the slash-separated path relative to the
  pack root. The path is last and may contain spaces; it runs to the newline.
- **Payload** — for `text`, the file's bytes verbatim; for `base64`, standard
  base64 of the bytes wrapped at 120 columns with newlines. A single newline
  always follows the payload (an empty file is just that newline). The payload
  runs to the next line that begins with `@@@FILE@@@` or `@@@END@@@`; on unpack
  the declared length and, for text, the exact byte count decide where the
  content ends, so the trailing newline is cosmetic.
- **Terminator** — a line `@@@END@@@`.

`@@@FILE@@@` and `@@@END@@@` are recognised only at the start of a line. A
decoder skips any line before the terminator that is neither an entry header
nor the end marker.

## What `pack` includes

- The file list is what git would track or keep: `git ls-files -z` plus
  `git ls-files -z --others --exclude-standard` (so tracked ignore files are
  included and ignored files are not), de-duplicated and sorted. Outside a git
  repository, or without git, it falls back to a plain directory walk that
  skips `.git`, also sorted. Explicit `PATHS` (and `--files-from`) override the
  list and keep their given order.
- Symlinks, directories and other non-regular files are skipped, as is the
  output file itself.
- **text** is human-readable and copy-paste-friendly but skips binary files (a
  file with a NUL byte or invalid UTF-8) and refuses one whose content has a
  line starting with a boundary marker — re-run with `--format base64`.
- **base64** survives whitespace and line-ending mangling and carries binaries;
  it is the format for the actual optical transfer.

## What `unpack` guarantees

Every entry is checked: the payload must have the declared length and sha256,
and the path must be safe (relative, no `..`, no absolute or drive-letter
prefix; `\` is treated as a separator). Only verified entries are written,
each with its recorded mode, under `--dest`. A failed entry is reported `BAD`
and not written, and the command exits non-zero. `--dry-run` verifies and
writes nothing. This is stricter than the original script, which wrote
corrupt content and warned; airlift never writes an entry it could not verify.

The tower runs the same parse and the same path sanitiser for zip entries and
for `--dest`, so a malicious bundle cannot escape the destination.
