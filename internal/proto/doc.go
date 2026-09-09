// Package proto implements the airlift wire format: the base45 codec, the
// 18-byte frame header, CRC-32 over payloads, and the manifest JSON. It is
// the Go counterpart of the codec in sender/airlift.py and is tested against
// sender/testdata/vectors.json. See docs/PROTOCOL.md.
package proto
