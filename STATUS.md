# STATUS

## Phase 2 — Tower core: done

- `internal/{proto,session,verify,bundle,tlsca,server,replay}` and
  `cmd/tower` (serve mode with the built-in CA, `--session`, terminal join
  QR; replay mode over loopback).
- `airlift-tower --dest DIR --replay sender/testdata/vectors.json --drop 0.2`
  reaches `READY` in three passes and writes the bundle and its tree.
- Serve mode verified live on the Mac: CA persisted with a 0600 key, leaf
  chain verifies, CA-less clients refused, LAN address auto-detected.

## Next — Phase 3: Web + end-to-end on hardware

- `web/` scan and tower entries; SSE and downloads via `fetch` (header-only
  auth, see `docs/API.md`); phone CA bootstrap walkthrough in the README.

## Open questions

- `net/http` logs a "TLS handshake error … unknown certificate authority"
  line for every CA-less client (each phone's first visit). Quiet it or keep
  it as a hint? Decide during Phase 4's review.
