// Package proto implements the airlift wire format: the base45 codec, the
// 18-byte frame header, CRC-32 over payloads, the manifest JSON, and the
// fountain packet construction. It is the reference implementation of the
// codec and is tested against the frozen fixtures in testdata/vectors/. See
// docs/PROTOCOL.md.
package proto
