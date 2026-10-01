package main

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/bundle"
)

// beamSource is what a beam carries, before it becomes bytes: one file sent
// as it is, or a folder or several files to bundle.
type beamSource struct {
	name     string
	file     string   // one regular file, sent as it is
	root     string   // else: bundle from here…
	explicit []string // …these paths (nil: the git-aware walk of root)
}

// resolveSource turns the input paths into a beamSource. One directory is
// bundled (git-aware); one file is sent as-is; several files are bundled,
// rooted at their deepest common directory, and need a name — from --name, a
// `name:` line in --files-from, or a prompt on a terminal.
func resolveSource(inputs []string, flagName, listName string, ask *prompter) (beamSource, error) {
	name := flagName
	if name == "" {
		name = listName
	}
	if len(inputs) == 1 {
		info, err := os.Stat(inputs[0])
		if err != nil {
			return beamSource{}, err
		}
		if info.IsDir() {
			if name == "" {
				abs, err := filepath.Abs(inputs[0])
				if err != nil {
					return beamSource{}, err
				}
				name = filepath.Base(abs)
			}
			return beamSource{name: name, root: inputs[0]}, nil
		}
		if !info.Mode().IsRegular() {
			return beamSource{}, fmt.Errorf("%s is not a regular file", inputs[0])
		}
		if name == "" {
			name = filepath.Base(inputs[0])
		}
		return beamSource{name: name, file: inputs[0]}, nil
	}
	if name == "" {
		line, err := ask.ask("name", "a name for these files")
		switch {
		case errors.Is(err, errNotInteractive):
			return beamSource{}, errors.New("several files need a name: pass --name NAME")
		case err != nil:
			return beamSource{}, err
		case line == "":
			return beamSource{}, errors.New("a name is required: pass --name NAME")
		}
		name = line
	}
	abs := make([]string, len(inputs))
	for i, p := range inputs {
		a, err := filepath.Abs(p)
		if err != nil {
			return beamSource{}, err
		}
		abs[i] = a
	}
	root := commonDir(abs)
	explicit, err := bundle.ResolveExplicit(root, abs)
	if err != nil {
		return beamSource{}, err
	}
	return beamSource{name: name, root: root, explicit: explicit}, nil
}

// bytes is the payload in memory, for a QR page (a page carries what a screen
// can show, so it is never large). The second result is the bundle format
// written ("" for a single file, sent as-is).
func (src beamSource) bytes(format string, stderr io.Writer, st *status) ([]byte, string, error) {
	if src.file != "" {
		data, err := os.ReadFile(src.file)
		return data, "", err
	}
	var buf bytes.Buffer
	used, err := packBundle(&buf, func() error { buf.Reset(); return nil }, src.root, format, src.explicit, stderr, st)
	if err != nil {
		return nil, "", err
	}
	return buf.Bytes(), used, nil
}

// payload is what a direct send streams (ADR 0024): one file on disk, sent and
// kept exactly as it is, with its size and sha256 — never a repobundle. A
// single file is read where it is; a folder or several files are first zipped
// into a temporary file (their paths and modes kept), which remove deletes.
type payload struct {
	path   string
	name   string // what the tower keeps it as: the file's name, or <name>.zip
	size   int64
	sha256 string
	files  int // the files in a zip; 0 for a single file sent as it is
	temp   bool
}

func (p *payload) remove() {
	if p != nil && p.temp {
		os.Remove(p.path)
	}
}

// stage readies the payload for a direct send without holding it in memory:
// a single file is hashed where it is; a folder or several files are zipped
// to a temporary file, hashed as it is written.
func (src beamSource) stage(st *status) (*payload, error) {
	if src.file != "" {
		f, err := os.Open(src.file)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		n, err := io.Copy(h, &progressReader{r: f, total: info.Size(), st: st, verb: "hashing", start: time.Now()})
		st.clear()
		if err != nil {
			return nil, err
		}
		return &payload{path: src.file, name: src.name, size: n, sha256: hex.EncodeToString(h.Sum(nil))}, nil
	}
	files := src.explicit
	if files == nil {
		var err error
		if files, _, err = bundle.ListFiles(src.root); err != nil { // the files git would keep, as a page bundles
			return nil, err
		}
	}
	f, err := os.CreateTemp("", "airlift-*.zip")
	if err != nil {
		return nil, err
	}
	name := src.name
	if !strings.EqualFold(filepath.Ext(name), ".zip") {
		name += ".zip"
	}
	p := &payload{path: f.Name(), name: name, temp: true}
	h := sha256.New()
	n, err := zipFiles(io.MultiWriter(f, h), src.root, files, st)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	st.clear()
	if err != nil {
		p.remove()
		return nil, err
	}
	info, err := os.Stat(p.path)
	if err != nil {
		p.remove()
		return nil, err
	}
	p.size, p.sha256, p.files = info.Size(), hex.EncodeToString(h.Sum(nil)), n
	return p, nil
}

// zipFiles writes a zip of the regular files rel (slash paths under root) to
// w, each streamed from disk with its path, mode and time, and returns how
// many went in. Symlinks and anything not a regular file are skipped, as a
// bundle skips them; archive/zip moves to zip64 for a file over 4 GiB.
func zipFiles(w io.Writer, root string, rels []string, st *status) (int, error) {
	cw := &countingWriter{w: w}
	zw := zip.NewWriter(cw)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestSpeed) // the link is slower than this
	})
	n := 0
	for _, rel := range rels {
		path := filepath.Join(root, filepath.FromSlash(rel))
		fi, err := os.Lstat(path)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		hdr, err := zip.FileInfoHeader(fi)
		if err != nil {
			return n, err
		}
		hdr.Name, hdr.Method = rel, zip.Deflate
		zf, err := zw.CreateHeader(hdr)
		if err != nil {
			return n, err
		}
		in, err := os.Open(path)
		if err != nil {
			return n, err
		}
		cw.report = func(b int64) { st.live(fmt.Sprintf("  zipping  %d files · %s", n+1, humanBytes(b))) }
		_, err = io.Copy(zf, in)
		in.Close()
		if err != nil {
			return n, err
		}
		n++
	}
	return n, zw.Close()
}

// packBundle writes the bundle in the requested format to w and reports the
// one written. "auto" tries text — the smaller, human-readable form, about
// 30 % less gzip than base64 for source trees — and falls back to base64 when a
// file is binary (text would drop it) or holds a boundary marker (text refuses
// it), so nothing is ever silently left out of a beam; reset empties w for the
// second try.
func packBundle(w io.Writer, reset func() error, root, format string, explicit []string, stderr io.Writer, st *status) (string, error) {
	cw := &countingWriter{w: w, report: func(n int64) { st.live("  bundle   reading files · " + humanBytes(n)) }}
	if format != "auto" {
		rep, err := bundle.Pack(cw, root, format, explicit, "")
		if err == nil && len(rep.Skipped) > 0 {
			// An explicit text bundle drops binaries by design; say so rather than
			// letting a file go missing quietly.
			st.clear()
			fmt.Fprintf(stderr, "airlift beam: text format skipped %d binary file(s): %s\n", len(rep.Skipped), strings.Join(rep.Skipped, ", "))
		}
		return format, err
	}
	rep, err := bundle.Pack(cw, root, "text", explicit, "")
	if err == nil && len(rep.Skipped) == 0 {
		return "text", nil
	}
	if err != nil && !errors.Is(err, bundle.ErrBoundary) {
		return "", err
	}
	if err := reset(); err != nil {
		return "", err
	}
	cw.n = 0
	_, err = bundle.Pack(cw, root, "base64", explicit, "")
	return "base64", err
}

// progressReader draws a bar for a long read (a large file being hashed).
type progressReader struct {
	r     io.Reader
	n     int64
	total int64
	st    *status
	verb  string
	start time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	if p.total >= 64<<20 {
		p.st.live(fmt.Sprintf("  %-8s %s %3d %%  %s of %s  %s", p.verb, bar(p.n, p.total, 20), 100*min(p.n, p.total)/p.total,
			humanBytes(p.n), humanBytes(p.total), rate(p.n, time.Since(p.start))))
	}
	return n, err
}
