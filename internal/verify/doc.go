// Package verify runs the verification chain on a completed session:
// concatenate chunks, sha256 against the manifest's gz_sha256, gunzip,
// sha256 against orig_sha256.
//
// Populated in Phase 2; see docs/BUILD-PLAN.md.
package verify
