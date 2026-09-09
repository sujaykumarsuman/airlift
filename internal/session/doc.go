// Package session owns in-memory transfer sessions: creation, lookup, TTL
// sweep, frame ingest with dedup and CRC rejection, the chunk bitmap,
// completion detection, decoded-fps accounting, relay tracking, and the
// verdicts and downloads a finished transfer exposes. See ADR 0005.
package session
