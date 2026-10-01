package bundle

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Unpack is the bundle stage for a payload too large to hold in memory — a
// streamed direct upload (ADR 0024). It reads the bundle once, in order,
// writing each entry that verifies under the tree root as WriteTree would, and
// decides exactly what Parse decides: the same entries, each OK or not for the
// same reasons. Only a header line has to fit in memory.

// maxHeaderLine bounds the bundle header and an entry header: a real one is a
// short line, so a longer one is a malformed bundle.
const maxHeaderLine = 64 << 10

// unpackBuffer is the read buffer; tests shrink it to exercise long lines.
var unpackBuffer = 64 << 10

// Entry is one entry of an unpacked bundle: what its header declared and
// whether its content verified. Its bytes are on disk under the tree root when
// OK and written, nowhere otherwise.
type Entry struct {
	Path   string
	Mode   fs.FileMode
	Size   int64
	SHA256 string
	OK     bool
	Err    string
}

// Unpacked is a streamed bundle's outcome: its format and every entry, in
// bundle order.
type Unpacked struct {
	Format  string
	Entries []Entry
}

// Bad lists the paths of entries that failed verification.
func (u *Unpacked) Bad() []string {
	var out []string
	for _, e := range u.Entries {
		if !e.OK {
			out = append(out, e.Path)
		}
	}
	return out
}

// TotalBytes sums the declared sizes.
func (u *Unpacked) TotalBytes() int64 {
	var n int64
	for _, e := range u.Entries {
		n += e.Size
	}
	return n
}

// Unpack reads a repobundle from r and writes every entry that verifies under
// root, staging each in tmp (a directory on root's filesystem) until its
// digest is known. Once an entry fails, the rest are still read and checked
// but no longer written: a bundle with a bad entry is refused whole, and the
// caller discards the tree. A malformed bundle or an I/O failure is an error.
func Unpack(r io.Reader, root, tmp string) (*Unpacked, error) {
	br := bufio.NewReaderSize(r, unpackBuffer)
	header, more, err := readHeaderLine(br)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(header, []byte(Magic)) {
		return nil, fmt.Errorf("%w: missing header", ErrFormat)
	}
	u := &Unpacked{Format: "text"}
	for _, tok := range strings.Fields(string(header)) {
		if v, ok := strings.CutPrefix(tok, "format="); ok {
			u.Format = v
		}
	}
	if u.Format != "text" && u.Format != "base64" {
		return nil, fmt.Errorf("%w: unknown format %q", ErrFormat, u.Format)
	}
	failed := false
	for more {
		peek, err := br.Peek(len(boundary))
		if len(peek) == 0 {
			if err != nil && err != io.EOF {
				return nil, err
			}
			break
		}
		if bytes.HasPrefix(peek, []byte(end)) {
			break
		}
		if !bytes.HasPrefix(peek, []byte(boundary)) {
			if more, err = skipLine(br); err != nil {
				return nil, err
			}
			continue
		}
		line, open, err := readHeaderLine(br)
		if err != nil {
			return nil, err
		}
		f, err := parseHeader(string(line))
		if err != nil {
			return nil, err
		}
		e := Entry{Path: f.Path, Mode: f.Mode, Size: f.Size, SHA256: f.SHA256, Err: f.Err}
		if more, err = unpackEntry(br, &e, u.Format, open, root, tmp, !failed); err != nil {
			return nil, err
		}
		failed = failed || !e.OK
		u.Entries = append(u.Entries, e)
	}
	return u, nil
}

// readHeaderLine reads one line, without its newline, and whether a newline
// ended it (false at the end of the input).
func readHeaderLine(br *bufio.Reader) ([]byte, bool, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxHeaderLine {
			return nil, false, fmt.Errorf("%w: a header line runs over %d bytes", ErrFormat, maxHeaderLine)
		}
		switch err {
		case nil:
			return line[:len(line)-1], true, nil
		case bufio.ErrBufferFull:
		case io.EOF:
			return line, false, nil
		default:
			return nil, false, err
		}
	}
}

// skipLine consumes one line whatever its length; false at the end of the input.
func skipLine(br *bufio.Reader) (bool, error) {
	for {
		_, err := br.ReadSlice('\n')
		switch err {
		case nil:
			return true, nil
		case bufio.ErrBufferFull:
		case io.EOF:
			return false, nil
		default:
			return false, err
		}
	}
}

// unpackEntry consumes an entry's content — every line up to the next line
// that starts with a boundary or the end marker — verifying it as Parse does
// and, when write is set and it verifies, moving it into place under root.
// open is false when the entry header ended the input. It reports whether
// input remains.
func unpackEntry(br *bufio.Reader, e *Entry, format string, open bool, root, tmp string, write bool) (bool, error) {
	var out *os.File
	if write && e.Err == "" {
		f, err := os.CreateTemp(tmp, ".entry-*")
		if err != nil {
			return false, err
		}
		out = f
		defer func() {
			if out != nil {
				out.Close()
				os.Remove(out.Name())
			}
		}()
	}
	var sink contentSink
	var w io.Writer
	if out != nil {
		w = out
	}
	if format == "base64" {
		sink = &base64Sink{size: e.Size, w: w, h: sha256.New()}
	} else {
		sink = &textSink{size: e.Size, w: w, h: sha256.New()}
	}
	more := open
	for atStart := true; more; {
		if atStart {
			peek, err := br.Peek(len(boundary))
			if len(peek) == 0 {
				if err != nil && err != io.EOF {
					return false, err
				}
				more = false
				break
			}
			if bytes.HasPrefix(peek, []byte(boundary)) || bytes.HasPrefix(peek, []byte(end)) {
				break
			}
		}
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			if werr := sink.write(chunk); werr != nil {
				return false, werr
			}
			atStart = chunk[len(chunk)-1] == '\n'
		}
		switch err {
		case nil, bufio.ErrBufferFull:
		case io.EOF:
			more = false
		default:
			return false, err
		}
	}
	if e.Err == "" { // a rejected path outranks content problems
		e.Err = sink.verdict(e.SHA256)
	}
	e.OK = e.Err == ""
	if out == nil || !e.OK {
		return more, nil
	}
	full := filepath.Join(root, filepath.FromSlash(e.Path)) // Path passed SafePath in parseHeader
	if err := out.Close(); err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return false, err
	}
	if err := os.Rename(out.Name(), full); err != nil {
		return false, err
	}
	out = nil
	return more, os.Chmod(full, e.Mode&0o777)
}

// contentSink takes an entry's content region as it streams past and says,
// once it has all of it, why the entry does not verify ("" when it does).
type contentSink interface {
	write(p []byte) error
	verdict(sha string) string
}

// textSink keeps the first size bytes of the region (the trailing newline is
// cosmetic) and needs exactly that many.
type textSink struct {
	size, n int64
	w       io.Writer // nil: verify only
	h       hash.Hash
}

func (t *textSink) write(p []byte) error {
	start := t.n
	t.n += int64(len(p))
	if start >= t.size {
		return nil
	}
	keep := p[:min(int64(len(p)), t.size-start)]
	t.h.Write(keep)
	if t.w != nil {
		_, err := t.w.Write(keep)
		return err
	}
	return nil
}

func (t *textSink) verdict(sha string) string {
	return sizeAndDigest(min(t.n, t.size), t.size, t.h, sha)
}

// base64Sink decodes the region as Parse does — whitespace dropped, then
// standard base64 with padding — a quantum at a time, never holding more than
// one read's worth. Padding ends the data: anything after it is an error, as
// it is for a single Decode of the whole region.
type base64Sink struct {
	size, n int64
	w       io.Writer
	h       hash.Hash
	q       []byte // characters not yet decoded (a partial quantum between writes)
	buf     []byte
	padded  bool
	err     string
}

func (b *base64Sink) write(p []byte) error {
	if b.err != "" {
		return nil
	}
	for _, c := range p {
		if c == ' ' || c == '\n' || c == '\r' || c == '\t' {
			continue
		}
		if b.padded {
			b.err = "base64: data after padding"
			return nil
		}
		b.q = append(b.q, c)
	}
	n := len(b.q) / 4 * 4
	if n == 0 {
		return nil
	}
	if err := b.decode(b.q[:n]); err != nil {
		return err
	}
	b.padded = b.q[n-1] == '='
	b.q = append(b.q[:0], b.q[n:]...)
	return nil
}

func (b *base64Sink) decode(src []byte) error {
	if cap(b.buf) < base64.StdEncoding.DecodedLen(len(src)) {
		b.buf = make([]byte, base64.StdEncoding.DecodedLen(len(src)))
	}
	m, err := base64.StdEncoding.Decode(b.buf[:cap(b.buf)], src)
	if err != nil {
		b.err = "base64: " + err.Error()
		return nil
	}
	got := b.buf[:m]
	start := b.n
	b.n += int64(m)
	if start >= b.size {
		return nil
	}
	keep := got[:min(int64(len(got)), b.size-start)]
	b.h.Write(keep)
	if b.w != nil {
		_, err := b.w.Write(keep)
		return err
	}
	return nil
}

func (b *base64Sink) verdict(sha string) string {
	if b.err == "" && len(b.q) > 0 { // a partial final quantum
		if err := b.decode(b.q); err != nil {
			return err.Error()
		}
		if b.err == "" {
			b.err = "base64: truncated input"
		}
	}
	if b.err != "" {
		return b.err
	}
	return sizeAndDigest(b.n, b.size, b.h, sha)
}

// sizeAndDigest is check's verdict for content of length got whose first
// min(got, want) bytes went through h.
func sizeAndDigest(got, want int64, h hash.Hash, sha string) string {
	if got != want {
		return fmt.Sprintf("size: got %d bytes, header says %d", got, want)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != sha {
		return fmt.Sprintf("sha256: got %s, header says %s", sum, sha)
	}
	return ""
}

// ZipTree archives an unpacked bundle's entries from their files under root,
// paths and modes preserved as Zip does, at flate's fastest level: it streams
// a payload too large for memory (archive/zip moves to zip64 on its own).
func ZipTree(w io.Writer, root string, entries []Entry) error {
	zw := zip.NewWriter(w)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestSpeed)
	})
	for _, e := range entries {
		if !e.OK {
			return fmt.Errorf("refusing to archive unverified entry %q: %s", e.Path, e.Err)
		}
		safe, err := SafePath(e.Path)
		if err != nil {
			return err
		}
		hdr := &zip.FileHeader{Name: safe, Method: zip.Deflate, Modified: zipEpoch}
		hdr.SetMode(e.Mode&0o777 | 0o400)
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(safe)))
		if err != nil {
			return err
		}
		_, err = io.Copy(fw, f)
		f.Close()
		if err != nil {
			return err
		}
	}
	return zw.Close()
}
