// Package session owns in-memory transfer sessions: creation, lookup, TTL
// sweep, frame ingest with dedup and CRC rejection, the chunk bitmap,
// completion detection, decoded-fps accounting, and relay tracking.
//
// Populated in Phase 2; see docs/BUILD-PLAN.md.
package session
