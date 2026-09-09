// Package bundle is the repobundle stage: Pack writes a bundle (a byte-for-byte
// port of the retired tools/repobundle.py pack, docs/BUNDLE.md, ADR 0010) and
// Parse reads one. It sniffs the "#repobundle v1" header, handles both text and
// base64 formats, checks per-file sha256, preserves modes, sanitises paths, and
// builds tree and zip outputs. See ADR 0006 and docs/BUNDLE.md.
package bundle
