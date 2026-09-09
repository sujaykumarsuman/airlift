# STATUS

## Phase 3 — Web + end-to-end: built and verified without hardware

- `web/`: `scan` (camera selector, BarcodeDetector or zxing-wasm, relay
  with dedup/batching/retry, SSE progress, wake lock) and `tower`
  (session, join QR and CA hint, live grid/fps/elapsed/ETA/relays, verdicts,
  bundle summary, downloads, reset). Embedded in the binary.
- Verified in a browser via `vite dev` against the tower: create session,
  inject frames on the scan page, `--replay --into` over TLS, `READY`,
  downloads with the right bytes and names, `--dest` written.
- `airlift-tower --replay FILE --into JOIN_URL` added for that dev loop.

## Pending — the Phase 3 hardware run

Run on the Mac + Android, per `docs/BUILD-PLAN.md` Phase 3 exit: install
`/ca.crt` once, scan the join QR, scan `beam.html` off the monitor, reach
`READY`, compare the zip and `--dest` with the source. Watch for: which
decoder the scan page picked (shown in its stats line), decode rate at
8 fps, and whether continuous focus engaged.

## Next — Phase 4: Hardening

- Fountain mode, `--version-target`, torch and offline-first scan page,
  review items, README tuning guide, GitHub Actions.

## Open questions

- `net/http` logs a "TLS handshake error … unknown certificate authority"
  line for every CA-less client (each phone's first visit). Quiet it or keep
  it as a hint? Decide during Phase 4's review.
- The dashboard's elapsed time restarts if the page is reloaded mid-transfer
  (timing lives in the page, not the API). Persist it, or add
  `started_at` to the snapshot?
