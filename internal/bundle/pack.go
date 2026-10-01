package bundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ErrBoundary is returned when a text-format bundle would contain a line
// starting with a boundary marker; the caller should retry with base64.
var ErrBoundary = errors.New("contains a bundle boundary marker")

// PackReport summarises a pack run.
type PackReport struct {
	Packed  int
	Skipped []string // paths dropped from a text bundle because they are binary
	Source  string   // "git", "walk-fallback" or "explicit-list"
}

// Pack writes a repobundle to w, byte-for-byte as repobundle.py did
// (docs/BUNDLE.md). format is "text" or "base64". When explicit is non-nil,
// those slash-separated paths (relative to root, already resolved by
// ResolveExplicit) are bundled in that order; otherwise the git-aware file
// list under root is used. excludeAbs, when set, is an output path never
// bundled. Symlinks and non-regular files are skipped; a text bundle drops
// binary files and refuses one holding a boundary marker.
func Pack(w io.Writer, root, format string, explicit []string, excludeAbs string) (PackReport, error) {
	if format != "text" && format != "base64" {
		return PackReport{}, fmt.Errorf("format must be text or base64, got %q", format)
	}
	var rep PackReport
	var files []string
	if explicit != nil {
		files, rep.Source = explicit, "explicit-list"
	} else {
		usedGit, err := false, error(nil)
		if files, usedGit, err = ListFiles(root); err != nil {
			return rep, err
		}
		if usedGit {
			rep.Source = "git"
		} else {
			rep.Source = "walk-fallback"
		}
	}
	if _, err := fmt.Fprintf(w, "%s format=%s\n", Magic, format); err != nil {
		return rep, err
	}
	for _, rel := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		fi, err := os.Lstat(path)
		if err != nil || !fi.Mode().IsRegular() { // skip dirs, symlinks, missing
			continue
		}
		if excludeAbs != "" {
			if ap, err := filepath.Abs(path); err == nil && ap == excludeAbs {
				continue
			}
		}
		// Two reads of the file, neither holding it: the first learns what the
		// entry header needs (size, sha256, and for text whether it can be
		// carried at all), the second writes the payload and checks the file
		// did not change in between.
		scan, err := scanFile(path, format == "text")
		if err != nil {
			return rep, err
		}
		if format == "text" {
			if scan.binary {
				rep.Skipped = append(rep.Skipped, rel)
				continue
			}
			if scan.boundary {
				return rep, fmt.Errorf("%s %w; re-run with --format base64", rel, ErrBoundary)
			}
		}
		mode := strconv.FormatUint(uint64(fi.Mode().Perm()), 8)
		if _, err := fmt.Fprintf(w, "%s %d %s %s %s\n", boundary, scan.size, scan.sha256, mode, rel); err != nil {
			return rep, err
		}
		if err := writePayload(w, path, format, scan); err != nil {
			return rep, err
		}
		if _, err := w.Write([]byte{'\n'}); err != nil {
			return rep, err
		}
		rep.Packed++
	}
	if _, err := fmt.Fprintf(w, "%s\n", end); err != nil {
		return rep, err
	}
	return rep, nil
}

// ListFiles returns the files git would track or keep under root (respecting
// .gitignore, including tracked ignore files), sorted; usedGit reports whether
// git supplied the list. It falls back to a plain walk skipping .git when root
// is not a git repository or git is unavailable.
func ListFiles(root string) (files []string, usedGit bool, err error) {
	tracked, e1 := gitList(root, "ls-files", "-z")
	others, e2 := gitList(root, "ls-files", "-z", "--others", "--exclude-standard")
	if e1 == nil && e2 == nil {
		seen := map[string]bool{}
		var out []string
		for _, r := range append(tracked, others...) {
			if r == "" || seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, r)
		}
		sort.Strings(out)
		return out, true, nil
	}
	var out []string
	werr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if werr != nil {
		return nil, false, werr
	}
	sort.Strings(out)
	return out, false, nil
}

func gitList(root string, args ...string) ([]string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	var out []string
	for _, seg := range bytes.Split(stdout.Bytes(), []byte{0}) {
		if len(seg) > 0 {
			out = append(out, string(seg))
		}
	}
	return out, nil
}

// ResolveExplicit normalises an explicit path list to unique files relative to
// root, preserving order. Paths may be absolute or relative to root; a path
// that is not a file, or that escapes root, is an error.
func ResolveExplicit(root string, paths []string) ([]string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		ap := p
		if !filepath.IsAbs(ap) {
			ap = filepath.Join(absRoot, p)
		}
		if ap, err = filepath.Abs(ap); err != nil {
			return nil, err
		}
		if fi, err := os.Stat(ap); err != nil || !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("not a file: %s", p)
		}
		rel, err := filepath.Rel(absRoot, ap)
		if err != nil {
			return nil, err
		}
		rel = filepath.ToSlash(rel)
		if rel == ".." || strings.HasPrefix(rel, "../") {
			return nil, fmt.Errorf("%s is outside --root (%s); set --root to a common parent", p, root)
		}
		if !seen[rel] {
			seen[rel] = true
			out = append(out, rel)
		}
	}
	return out, nil
}

// packChunk is how much of a file Pack reads at a time; tests shrink it so
// lines, runes and base64 groups straddle reads.
var packChunk = 1 << 20

// fileScan is what the first read of a file learns.
type fileScan struct {
	size     int64
	sha256   string
	binary   bool // a NUL byte or invalid UTF-8 (isBinaryData)
	boundary bool // a line starting with a boundary marker (hasBoundaryLine)
}

// scanFile reads the file at path once, hashing it and, for the text format,
// deciding isBinaryData and hasBoundaryLine as they would on the whole file.
func scanFile(path string, text bool) (fileScan, error) {
	f, err := os.Open(path)
	if err != nil {
		return fileScan{}, err
	}
	defer f.Close()
	var sc fileScan
	h := sha256.New()
	var u utf8Stream
	var lines lineStream
	buf := make([]byte, packChunk)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			p := buf[:n]
			h.Write(p)
			sc.size += int64(n)
			if text && !sc.binary {
				sc.binary = bytes.IndexByte(p, 0) >= 0 || !u.write(p)
				sc.boundary = sc.boundary || lines.write(p)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fileScan{}, err
		}
	}
	if text && !sc.binary {
		sc.binary = !u.close()
		sc.boundary = sc.boundary || lines.close()
	}
	sc.sha256 = hex.EncodeToString(h.Sum(nil))
	return sc, nil
}

// utf8Stream is utf8.Valid over a stream: a rune cut by a read boundary is
// carried into the next read.
type utf8Stream struct{ carry []byte }

func (u *utf8Stream) write(p []byte) bool {
	b := append(u.carry, p...)
	i, back := len(b)-1, 0
	for i > 0 && back < utf8.UTFMax-1 && !utf8.RuneStart(b[i]) {
		i--
		back++
	}
	cut := len(b)
	if i >= 0 && utf8.RuneStart(b[i]) && !utf8.FullRune(b[i:]) {
		cut = i // an incomplete rune at the end: wait for the rest
	}
	ok := utf8.Valid(b[:cut]) // before the carry is rewritten: b may share its array
	u.carry = append(u.carry[:0], b[cut:]...)
	return ok
}

func (u *utf8Stream) close() bool { return utf8.Valid(u.carry) }

// lineStream is hasBoundaryLine over a stream: it keeps the first bytes of the
// line in progress, enough to recognise a marker at its start.
type lineStream struct {
	head    []byte
	checked bool // the line in progress has been checked
}

func (l *lineStream) write(p []byte) bool {
	found := false
	for len(p) > 0 {
		nl := bytes.IndexByte(p, '\n')
		seg := p
		if nl >= 0 {
			seg = p[:nl]
		}
		if !l.checked {
			l.head = append(l.head, seg[:min(len(seg), len(boundary)-len(l.head))]...)
			if len(l.head) == len(boundary) || nl >= 0 {
				found = found || isMarker(l.head)
				l.checked = true
			}
		}
		if nl < 0 {
			break
		}
		l.head, l.checked, p = l.head[:0], false, p[nl+1:]
	}
	return found
}

func (l *lineStream) close() bool { return !l.checked && isMarker(l.head) }

func isMarker(head []byte) bool {
	return bytes.HasPrefix(head, []byte(boundary)) || bytes.HasPrefix(head, []byte(end))
}

// writePayload writes the file at path as the format carries it — text as it
// is, base64 wrapped at 120 columns as wrapBase64 wraps it — and fails if it
// is not the file scanFile read.
func writePayload(w io.Writer, path, format string, sc fileScan) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	var n int64
	src := io.TeeReader(f, h)
	if format == "text" {
		if n, err = io.Copy(w, src); err != nil {
			return err
		}
	} else {
		bw := &wrapWriter{w: w, width: 120}
		enc := base64.NewEncoder(base64.StdEncoding, bw)
		if n, err = io.CopyBuffer(enc, src, make([]byte, packChunk)); err != nil {
			return err
		}
		if err := enc.Close(); err != nil {
			return err
		}
		if bw.err != nil {
			return bw.err
		}
	}
	if n != sc.size || hex.EncodeToString(h.Sum(nil)) != sc.sha256 {
		return fmt.Errorf("%s changed while it was being bundled", path)
	}
	return nil
}

// wrapWriter breaks what passes through it into lines of width characters,
// with no newline after the last.
type wrapWriter struct {
	w     io.Writer
	width int
	col   int
	err   error
}

func (ww *wrapWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 && ww.err == nil {
		if ww.col == ww.width {
			_, ww.err = ww.w.Write([]byte{'\n'})
			ww.col = 0
			continue
		}
		k := min(len(p), ww.width-ww.col)
		_, ww.err = ww.w.Write(p[:k])
		ww.col += k
		p = p[k:]
	}
	if ww.err != nil {
		return 0, ww.err
	}
	return total, nil
}

// isBinaryData matches repobundle.py is_binary: a NUL byte or invalid UTF-8.
func isBinaryData(data []byte) bool {
	return bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data)
}

func hasBoundaryLine(data []byte) bool {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if bytes.HasPrefix(line, []byte(boundary)) || bytes.HasPrefix(line, []byte(end)) {
			return true
		}
	}
	return false
}

// wrapBase64 encodes data and wraps it at width characters, joined by newlines
// with no trailing newline (empty input yields no bytes).
func wrapBase64(data []byte, width int) []byte {
	enc := base64.StdEncoding.EncodeToString(data)
	var b bytes.Buffer
	for i := 0; i < len(enc); i += width {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(enc[i:min(i+width, len(enc))])
	}
	return b.Bytes()
}
