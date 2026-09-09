package verify

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/sujaykumarsuman/airlift/internal/proto"
)

// Hash is one verdict: the expected digest from the manifest and the actual
// digest computed here.
type Hash struct {
	OK       bool
	Expected string
	Actual   string
}

// Result is the outcome of the chain. Data is the verified plaintext, nil
// unless every step passed. OrigSHA is zero when the chain stopped earlier.
type Result struct {
	GzSHA   Hash
	OrigSHA Hash
	Data    []byte
	Err     error
}

// ErrMismatch wraps every hash or size failure.
var ErrMismatch = errors.New("verification failed")

// Chain runs concat → sha256 vs gz_sha256 → gunzip → sha256 vs orig_sha256.
func Chain(chunks [][]byte, m proto.Manifest) Result {
	var r Result
	blob := bytes.Join(chunks, nil)
	r.GzSHA = Hash{Expected: m.GzSHA256, Actual: SHA256Hex(blob)}
	r.GzSHA.OK = r.GzSHA.Actual == r.GzSHA.Expected && int64(len(blob)) == m.GzSize
	if !r.GzSHA.OK {
		r.Err = fmt.Errorf("%w: gzip blob (%d bytes, expected %d)", ErrMismatch, len(blob), m.GzSize)
		return r
	}
	data, err := Gunzip(blob, m.OrigSize)
	if err != nil {
		r.Err = fmt.Errorf("%w: gunzip: %v", ErrMismatch, err)
		return r
	}
	r.OrigSHA = Hash{Expected: m.OrigSHA256, Actual: SHA256Hex(data)}
	r.OrigSHA.OK = r.OrigSHA.Actual == r.OrigSHA.Expected && int64(len(data)) == m.OrigSize
	if !r.OrigSHA.OK {
		r.Err = fmt.Errorf("%w: original (%d bytes, expected %d)", ErrMismatch, len(data), m.OrigSize)
		return r
	}
	r.Data = data
	return r
}

// Gunzip inflates blob, refusing output larger than limit bytes.
func Gunzip(blob []byte, limit int64) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	data, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("inflates beyond %d bytes", limit)
	}
	return data, nil
}

// SHA256Hex is the lowercase hex digest, as the sender writes it.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
