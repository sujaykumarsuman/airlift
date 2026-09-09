package bundle

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// zipEpoch keeps zip output deterministic; MS-DOS timestamps start in 1980.
var zipEpoch = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// WriteTree writes verified files under root, creating directories as
// needed and applying each file's mode. Every path passes SafePath again.
func WriteTree(root string, files []File) error {
	for _, f := range files {
		if !f.OK {
			return fmt.Errorf("refusing to write unverified entry %q: %s", f.Path, f.Err)
		}
		safe, err := SafePath(f.Path)
		if err != nil {
			return err
		}
		full := filepath.Join(root, filepath.FromSlash(safe))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, f.Data, 0o644); err != nil {
			return err
		}
		if err := os.Chmod(full, f.Mode&0o777); err != nil {
			return err
		}
	}
	return nil
}

// Zip archives verified files with their paths and modes preserved.
func Zip(files []File) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		if !f.OK {
			return nil, fmt.Errorf("refusing to archive unverified entry %q: %s", f.Path, f.Err)
		}
		safe, err := SafePath(f.Path)
		if err != nil {
			return nil, err
		}
		hdr := &zip.FileHeader{Name: safe, Method: zip.Deflate, Modified: zipEpoch}
		hdr.SetMode(f.Mode&0o777 | 0o400)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(f.Data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
