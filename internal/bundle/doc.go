// Package bundle is a Go port of tools/repobundle.py unpack. It sniffs the
// "#repobundle v1" header, handles both text and base64 formats, checks
// per-file sha256, preserves modes, sanitises paths, and builds zip downloads.
// See ADR 0006.
package bundle
