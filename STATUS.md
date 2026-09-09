# STATUS

## Phase 4 — Hardening: built and verified without hardware

- Fountain mode end to end: `beam --fountain` (LT, robust soliton, ADR
  0009), a peeling decoder in Python and Go that share one packet contract
  (checked seed by seed against `vectors-fountain.json`), sessions that
  accept chunks and packets from any number of relays.
- `--version-target`; torch, installable offline-first scan page,
  `/s/last` resume; `started_at`/`finished_at` in the snapshot; the TLS
  handshake noise silenced; GitHub Actions for CI and releases; README
  workflow, tuning table and zero-hop variant.
- Verified: 1 MB bundle through fountain replay with 20 % loss and
  reordering in one pass; two relays beat one in the tests; tokens never
  reach the log; expiry closes streams.

## Pending — the hardware runs

Phase 3: Mac + Android, install `/ca.crt` once, scan the join QR, scan
`beam.html` off the monitor, compare zip and `--dest` with the source; a
second run shows no warning. Phase 4: the same with a 1 MB bundle in
fountain mode at ≥ 8 fps decoded, then with two phones on one session.
Watch the scan page's stats line for the decoder used and the decode rate.

## Next

- Nothing scheduled beyond the hardware runs. Candidates once those are in:
  tune the default `--chunk`/`--fps` to what the phones actually sustain,
  and revisit the fountain packet count against measured loss.

## Open questions

- None. (The handshake log lines are now dropped; elapsed time comes from
  the tower's clock.)
