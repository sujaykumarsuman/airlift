// Package airlift is the module root. It exists only to embed the built web
// UI (web/dist) so that cmd/airlift can serve it from a single static binary.
// go:embed cannot reach a parent directory, which is why this file lives here
// rather than under internal/server.
package airlift

import "embed"

// Dist holds the Vite build output. It is empty (only .gitkeep) until
// `make web` has run; internal/server treats a missing entry as "not built".
//
//go:embed all:web/dist
var Dist embed.FS
