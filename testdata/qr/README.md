# QR symbol fixture

`matrices.json` is the contract between the Go reference QR encoder
(`internal/beam.encodeSymbol`, on `rsc.io/qr/coding` with the penalty-chosen
mask, ADR 0011) and the player's inline JavaScript encoder
(`internal/beam/qrjs.js`, embedded into every beam): twelve versions — every
level at 1, 10, 30 and 40, M elsewhere — with four frames each (one character,
half capacity, capacity − 1, capacity) over the base45 alphabet, and for each
the mask Go chose and the module matrix (base64, row-major, MSB first,
1 = dark). Each version also carries the plan the page receives: the
function-pattern bitmaps and the block structure.

`web/src/beam/qrjs.test.ts` requires the JavaScript encoder to reproduce every
matrix and mask bit for bit, then reads a spread of them back through
`zxing-wasm`. `TestQRFixtureCurrent` in `internal/beam` fails when the Go
side no longer produces this file; after a deliberate change on the Go side
regenerate it and re-run the web tests:

```bash
go test ./internal/beam -run TestQRFixtureCurrent -update
```

Unlike `testdata/vectors/`, which is frozen from the original Python sender,
this file is Go's output and follows Go.
