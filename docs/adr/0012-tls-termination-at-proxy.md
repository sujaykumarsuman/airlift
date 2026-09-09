# ADR 0012 — TLS terminates at a reverse proxy; the tower is plain HTTP under a path prefix

Status: accepted (Phase 6, prompt 002)

Supersedes ADR 0008 (built-in local CA). Amends ADR 0007 (HTTP, not WebSocket) —
the transport is still HTTP request/response + SSE; only where TLS lives changes.

## Context

Prompt 001 had the tower serve HTTPS itself from a local certificate authority
it minted (ADR 0008): a leaf per run over the detected LAN interface, `GET
/ca.crt` for the phone to install, and `--bind`/`--cert`/`--key` knobs. That
fit one laptop on a LAN. Prompt 002 makes the tower a hosted, public,
multi-user service reached at `https://projects.example.dev/airlift`, behind a
reverse proxy that already terminates real TLS. Two things follow: the tower no
longer does TLS, and it lives under a **path prefix** the proxy strips.

`getUserMedia` still needs a secure context, but the proxy's real certificate
provides it in production and `localhost` provides it in development — so the
built-in CA, the certificate-install dance and the LAN detection are all dead
weight.

## Decision

The tower speaks **plain HTTP** on `listen` (default `127.0.0.1:8443`) and is
reached through an operator-run reverse proxy that terminates TLS. `internal/tlsca`,
`GET /ca.crt`, `--bind`, `--cert`, `--key`, `--ca-dir` and the LAN detection are
removed; the phone-first-run CA guidance goes with them.

- **`public_url`** carries scheme, host, port and an optional path prefix
  (`https://projects.example.dev/airlift`; default `http://localhost:8443`).
  `ParsePublicURL` splits it into the full base used verbatim for join links and
  the prefix (`/airlift` or `""`) used for `<base href>` and `/api/info`.
- **The router stays rooted** (`/api/…`, `/s/{sid}`, `/{$}`). The proxy strips
  the prefix (`handle_path /airlift/*` in Caddy; `http.StripPrefix` in the
  prefix test); the tower never strips it itself.
- **`<base href>`** is injected at serve time into the two HTML heads (a
  `<!--airlift-base-->` sentinel), so relative asset/API URLs resolve against
  the app root even under a prefix. The web build uses Vite `base: './'` and
  every client URL resolves against `document.baseURI`; the service worker and
  web manifest derive the prefix from their own served location at runtime.
- **`GET /api/info`** (unauthenticated, never logged) returns `{version,
  public_url, base_path, admin_enabled, caps}` so the pages can build correct
  links and shape their forms before any session exists.
- **Client address**: the identity is the direct peer, unless the peer is in
  `trusted_proxies` (default loopback), in which case `X-Forwarded-For` is read
  **right to left**, skipping further trusted hops, taking the first untrusted
  address — the client the trusted proxy appended. The leftmost XFF entry is
  client-spoofable and is never believed on its own; trusting it would let any
  client forge an identity and defeat eviction and the per-address rate limits.
- **`--dest` is gone**; verified output is written under `data_dir` always (the
  per-beam layout is ADR 0016). `data_dir` is emptied on start behind a guard
  that refuses the filesystem root, the user's home or an ancestor of it, and
  the airlift home, and refuses to erase any directory airlift did not create
  (an `.airlift-data` sentinel).

## Consequences

- The air-gapped side is untouched: the beam never talked to the tower. The
  operator side gains a proxy dependency (documented under `deploy/`) and loses
  the CA install step — a phone just trusts the proxy's real certificate.
- Development is plain HTTP on `localhost` (a secure context), so no local TLS
  is needed; `vite dev` keeps an optional self-signed plugin only for testing a
  real phone against the dev server over the LAN.
- The prefix is a runtime value, so it must be resolved at serve/runtime
  (`<base href>`, `document.baseURI`, `self.location`), never baked at build
  time. The prefix test (`http.StripPrefix` in front of the rooted handler)
  pins that join links, `<base>`, `/api/info` and rooted routing all line up.
- The whole isolation model (client identity, eviction, per-address rate limits
  in ADR 0013) rests on the right-to-left X-Forwarded-For rule and a correctly
  configured `trusted_proxies`.
