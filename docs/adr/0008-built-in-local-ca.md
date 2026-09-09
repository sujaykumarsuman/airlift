# ADR 0008 — TLS via a built-in local certificate authority

Status: accepted (Phase 0)

## Context

`getUserMedia` requires a secure context, so the scan page must be served
over HTTPS on the LAN. An ephemeral self-signed certificate means a browser
interstitial on every run, and the tower's IP changes with DHCP.

## Decision

The tower generates a CA once and persists it under
`os.UserConfigDir()/airlift/` (`~/Library/Application Support/airlift` on
macOS), key mode 0600. Each run it issues a short-lived leaf signed by that
CA, with SANs for every current LAN IPv4 plus `localhost` and `127.0.0.1`.
The CA is served at `GET /ca.crt`. One-time bootstrap per phone: proceed
through the interstitial once, open `/ca.crt`, install it as a CA
certificate; afterwards there are no warnings and address changes do not
matter because the leaf is reissued under the same CA. `--cert/--key`
overrides the whole mechanism for mkcert users. Standard library
`crypto/x509` only.

## Consequences

- The tower binds to the detected LAN interface rather than `0.0.0.0`;
  `--bind IP` overrides.
- Android shows a persistent "network may be monitored" notice while a user
  CA is installed; the README explains that this is all it means.
- Tests cover fresh CA creation, reuse, and leaf validity for in-SAN and
  out-of-SAN addresses.
