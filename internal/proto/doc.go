// Package proto implements the airlift wire format: the base45 codec, the
// 18-byte frame header, CRC-32 over payloads, and the manifest JSON. It is
// shared by the ingest path and by the fountain decoder (Phase 4).
//
// Populated in Phase 2; see docs/BUILD-PLAN.md.
package proto
