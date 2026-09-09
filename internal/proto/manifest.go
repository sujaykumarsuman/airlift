package proto

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrManifest wraps every manifest validation failure.
var ErrManifest = errors.New("manifest")

// Manifest is the payload of a MANIFEST frame. Field order matches the
// sender's compact JSON, so JSON() reproduces its bytes for ASCII names.
type Manifest struct {
	Name       string `json:"name"`
	GzSize     int64  `json:"gz_size"`
	GzSHA256   string `json:"gz_sha256"`
	OrigSize   int64  `json:"orig_size"`
	OrigSHA256 string `json:"orig_sha256"`
	Chunk      int    `json:"chunk"`
}

// ParseManifest decodes and validates a MANIFEST payload.
func ParseManifest(payload []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(payload, &m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrManifest, err)
	}
	switch {
	case m.Chunk < 1 || m.Chunk > 0xFFFF:
		return Manifest{}, fmt.Errorf("%w: chunk %d out of range", ErrManifest, m.Chunk)
	case m.GzSize < 0 || m.OrigSize < 0:
		return Manifest{}, fmt.Errorf("%w: negative size", ErrManifest)
	case !isSHA256Hex(m.GzSHA256) || !isSHA256Hex(m.OrigSHA256):
		return Manifest{}, fmt.Errorf("%w: malformed sha256", ErrManifest)
	case m.Total() > MaxChunks:
		return Manifest{}, fmt.Errorf("%w: %d chunks exceeds %d", ErrManifest, m.Total(), MaxChunks)
	}
	return m, nil
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// JSON is the compact encoding, byte-identical to the sender's for ASCII names.
func (m Manifest) JSON() ([]byte, error) {
	return json.Marshal(m)
}

// Total is N, the number of DATA chunks: ceil(gz_size / chunk), at least 1.
func (m Manifest) Total() int {
	if m.Chunk <= 0 {
		return 0
	}
	t := (m.GzSize + int64(m.Chunk) - 1) / int64(m.Chunk)
	if t < 1 {
		t = 1
	}
	return int(t)
}

// ChunkLen is the payload length DATA frame seq must carry: the chunk size
// for every chunk but the last, which carries the remainder.
func (m Manifest) ChunkLen(seq int) int {
	last := m.Total() - 1
	if seq < last {
		return m.Chunk
	}
	return int(m.GzSize - int64(last)*int64(m.Chunk))
}
