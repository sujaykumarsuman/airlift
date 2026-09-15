// Package beam is the encode side of airlift: the shared pipeline that turns a
// byte blob into a manifest and a list of frames (gzip → chunk → frame →
// base45, sequential or fountain, chosen by ModeAuto), the QR plan and the
// self-contained HTML player with its inline encoder that `airlift beam` emits
// (Build ties them together), and the reference Decode. `airlift beam` and `internal/replay`
// share the one encoder here; the Dump type is the internal fixture format the
// frozen vectors use. See docs/PROTOCOL.md.
package beam
