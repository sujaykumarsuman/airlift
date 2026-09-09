package bundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

const (
	// Magic starts every repobundle file.
	Magic    = "#repobundle v1"
	boundary = "@@@FILE@@@"
	end      = "@@@END@@@"
)

// ErrFormat wraps structural problems: not a bundle, unknown format, or a
// malformed entry header. Per-file hash failures are not errors; see File.OK.
var ErrFormat = errors.New("repobundle")

// File is one entry. Path is sanitised and slash-separated; when the
// declared path was rejected, Path keeps the original and Err says why.
type File struct {
	Path   string
	Mode   fs.FileMode // permission bits as packed (0o777 mask)
	Size   int64       // declared length
	SHA256 string      // declared digest
	Data   []byte
	OK     bool   // Data has the declared length and digest and Path is safe
	Err    string // why not OK
}

// Bundle is a parsed repobundle.
type Bundle struct {
	Format string // "text" or "base64"
	Files  []File
}

// IsBundle reports whether data starts with the repobundle magic.
func IsBundle(data []byte) bool {
	return bytes.HasPrefix(data, []byte(Magic))
}

// Parse decodes a bundle and verifies every entry. It mirrors the retired
// repobundle.py's unpack loop: lines that are neither a boundary nor the end
// marker are skipped, and an entry's content runs to the next boundary at a
// line start.
func Parse(data []byte) (*Bundle, error) {
	nl := bytes.IndexByte(data, '\n')
	header := data
	if nl >= 0 {
		header = data[:nl]
	}
	if !bytes.HasPrefix(header, []byte(Magic)) {
		return nil, fmt.Errorf("%w: missing header", ErrFormat)
	}
	b := &Bundle{Format: "text"}
	for _, tok := range strings.Fields(string(header)) {
		if v, ok := strings.CutPrefix(tok, "format="); ok {
			b.Format = v
		}
	}
	if b.Format != "text" && b.Format != "base64" {
		return nil, fmt.Errorf("%w: unknown format %q", ErrFormat, b.Format)
	}
	if nl < 0 {
		return b, nil
	}
	pos := nl + 1
	for pos < len(data) {
		if bytes.HasPrefix(data[pos:], []byte(end)) {
			break
		}
		if !bytes.HasPrefix(data[pos:], []byte(boundary)) {
			next := bytes.IndexByte(data[pos:], '\n')
			if next < 0 {
				break
			}
			pos += next + 1
			continue
		}
		hdrEnd := bytes.IndexByte(data[pos:], '\n')
		if hdrEnd < 0 {
			hdrEnd = len(data) - pos
		}
		f, err := parseHeader(string(data[pos : pos+hdrEnd]))
		if err != nil {
			return nil, err
		}
		cstart := min(pos+hdrEnd+1, len(data))
		bstart := nextBoundary(data, cstart)
		region := data[cstart:bstart]
		data, derr := decodeRegion(region, b.Format, f.Size)
		f.Data = data
		if f.Err == "" { // a rejected path outranks content problems
			f.Err = derr
		}
		if f.Err == "" {
			f.Err = check(f)
		}
		f.OK = f.Err == ""
		b.Files = append(b.Files, f)
		pos = bstart
	}
	return b, nil
}

// parseHeader reads "@@@FILE@@@ nbytes sha mode relpath"; relpath may contain spaces.
func parseHeader(line string) (File, error) {
	parts := strings.SplitN(strings.TrimRight(line, "\r"), " ", 5)
	if len(parts) != 5 {
		return File{}, fmt.Errorf("%w: malformed entry header %q", ErrFormat, line)
	}
	size, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || size < 0 {
		return File{}, fmt.Errorf("%w: bad size in %q", ErrFormat, line)
	}
	mode, err := strconv.ParseUint(parts[3], 8, 32)
	if err != nil {
		return File{}, fmt.Errorf("%w: bad mode in %q", ErrFormat, line)
	}
	f := File{Size: size, SHA256: parts[2], Mode: fs.FileMode(mode) & 0o777}
	if safe, err := SafePath(parts[4]); err != nil {
		f.Path = parts[4]
		f.Err = "path: " + err.Error()
	} else {
		f.Path = safe
	}
	return f, nil
}

func decodeRegion(region []byte, format string, size int64) ([]byte, string) {
	if format == "base64" {
		compact := make([]byte, 0, len(region))
		for _, c := range region {
			if c != ' ' && c != '\n' && c != '\r' && c != '\t' {
				compact = append(compact, c)
			}
		}
		out := make([]byte, base64.StdEncoding.DecodedLen(len(compact)))
		n, err := base64.StdEncoding.Decode(out, compact)
		if err != nil {
			return nil, "base64: " + err.Error()
		}
		return out[:n], ""
	}
	if int64(len(region)) > size {
		region = region[:size] // exact length; the trailing newline is cosmetic
	}
	return append([]byte(nil), region...), ""
}

func check(f File) string {
	if f.Err != "" {
		return f.Err
	}
	if int64(len(f.Data)) != f.Size {
		return fmt.Sprintf("size: got %d bytes, header says %d", len(f.Data), f.Size)
	}
	sum := sha256.Sum256(f.Data)
	if got := hex.EncodeToString(sum[:]); got != f.SHA256 {
		return fmt.Sprintf("sha256: got %s, header says %s", got, f.SHA256)
	}
	return ""
}

// nextBoundary is the index of the next line starting with the boundary or
// the end marker at or after start, or len(data).
func nextBoundary(data []byte, start int) int {
	i := start
	for i < len(data) {
		if i == 0 || data[i-1] == '\n' {
			if bytes.HasPrefix(data[i:], []byte(boundary)) || bytes.HasPrefix(data[i:], []byte(end)) {
				return i
			}
		}
		nl := bytes.IndexByte(data[i:], '\n')
		if nl < 0 {
			return len(data)
		}
		i += nl + 1
	}
	return len(data)
}

// Bad lists the paths of entries that failed verification.
func (b *Bundle) Bad() []string {
	var out []string
	for _, f := range b.Files {
		if !f.OK {
			out = append(out, f.Path)
		}
	}
	return out
}

// TotalBytes sums the declared sizes.
func (b *Bundle) TotalBytes() int64 {
	var n int64
	for _, f := range b.Files {
		n += f.Size
	}
	return n
}
