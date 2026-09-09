// Package beam is the encode side of airlift: the shared pipeline that turns a
// byte blob into a manifest and a list of frames (gzip → chunk → frame →
// base45, sequential or fountain), the QR rendering and self-contained HTML
// player that `airlift beam` emits, the frames dump that `airlift frames`
// writes, and the reference decode that `airlift decode` runs. `airlift beam`,
// `airlift frames` and `internal/replay` share the one encoder here. See
// docs/PROTOCOL.md.
package beam
