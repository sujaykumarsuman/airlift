# Sender vectors

`vectors.json` is `airlift.py frames --seed 1` over
`testdata/bundles/multi/bundle-base64.txt`: the frames dump format described
in `docs/PROTOCOL.md`, consumed by the Go protocol tests and by
`airlift-tower --replay`.

Regenerate only when the multi bundle or the wire format changes:

```bash
uv run --directory sender python airlift.py frames --in ../testdata/bundles/multi/bundle-base64.txt --seed 1 --out testdata/vectors.json
```

The gzip stream depends on the zlib in use, so a different machine may
produce different bytes for the same input. The committed file is still
valid wherever it is decoded; `test_vectors_regenerate_from_seed` skips
rather than fails in that case.
