# Sender vectors

`vectors.json` is `airlift.py frames --seed 1` over
`testdata/bundles/multi/bundle-base64.txt`, and `vectors-fountain.json` the
same with `--fountain`: the frames dump format described in
`docs/PROTOCOL.md`, consumed by the Go protocol, session and server tests
and by `airlift-tower --replay`. The fountain dump also lists every packet's
index set, which the Go decoder must reproduce exactly.

Regenerate only when the multi bundle or the wire format changes:

```bash
make vectors
```

The gzip stream depends on the zlib in use, so a different machine may
produce different bytes for the same input. The committed file is still
valid wherever it is decoded; `test_vectors_regenerate_from_seed` skips
rather than fails in that case.
