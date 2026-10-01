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
		data, err := os.ReadFile(path)
		if err != nil {
			return rep, err
		}
		var payload []byte
		if format == "text" {
			if isBinaryData(data) {
				rep.Skipped = append(rep.Skipped, rel)
				continue
			}
			if hasBoundaryLine(data) {
				return rep, fmt.Errorf("%s %w; re-run with --format base64", rel, ErrBoundary)
			}
			payload = data
		} else {
			payload = wrapBase64(data, 120)
		}
		sum := sha256.Sum256(data)
		mode := strconv.FormatUint(uint64(fi.Mode().Perm()), 8)
		if _, err := fmt.Fprintf(w, "%s %d %s %s %s\n", boundary, len(data), hex.EncodeToString(sum[:]), mode, rel); err != nil {
			return rep, err
		}
		if _, err := w.Write(payload); err != nil {
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
