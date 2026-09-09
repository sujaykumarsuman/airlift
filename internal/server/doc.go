// Package server exposes the tower's plain-HTTP API (docs/API.md): session
// routes, token auth, SSE state streams, GET /api/info, request limits, base-
// path-aware serving of the embedded web UI behind a TLS-terminating reverse
// proxy (ADR 0012), trusted-proxy address resolution, and the verification
// stage that runs when a session completes.
package server
