# Bundle fixtures

Two source trees and the bundles `tools/repobundle.py pack` produces from
them. Go tests (Phase 2) unpack each bundle and compare the result with the
committed tree byte for byte and mode for mode.

| Fixture | Tree | `bundle-text.txt` | `bundle-base64.txt` |
| --- | --- | --- | --- |
| `single/` | one file | 1 file | 1 file |
| `multi/` | executable, binary, empty file, path with spaces, UTF-8, nested dir | 6 files (`data/noise.bin` skipped: the text format drops binaries) | 7 files |

Regenerate only when a tree changes, from the repository root, and commit
tree and bundles together:

```bash
for t in single multi; do for f in text base64; do
  python3 tools/repobundle.py pack --root testdata/bundles/$t/tree --format $f --out testdata/bundles/$t/bundle-$f.txt
done; done
```

`multi/tree/data/noise.bin` is 12 000 bytes from `random.Random(20260909)`.
The pre-commit whitespace hooks skip this directory; `.gitattributes` marks
it `-text` so no checkout ever rewrites a line ending.
