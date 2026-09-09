// Package verify runs the verification chain on a completed session:
// concatenate chunks, sha256 against the manifest's gz_sha256, gunzip,
// sha256 against orig_sha256. See docs/PROTOCOL.md.
package verify
