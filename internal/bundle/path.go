package bundle

import (
	"errors"
	"fmt"
	"strings"
)

// ErrPath wraps every rejected path.
var ErrPath = errors.New("unsafe path")

// SafePath normalises a bundle path for use as a zip entry name or a path
// under --dest. Both "/" and "\" count as separators, "." and empty
// components are dropped, and the result is relative, non-empty and free of
// ".." components. Absolute paths and drive letters are rejected.
func SafePath(rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("%w: empty", ErrPath)
	}
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: NUL byte", ErrPath)
	}
	norm := strings.ReplaceAll(rel, "\\", "/")
	if strings.HasPrefix(norm, "/") {
		return "", fmt.Errorf("%w: absolute %q", ErrPath, rel)
	}
	var out []string
	for i, part := range strings.Split(norm, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("%w: %q has a .. component", ErrPath, rel)
		}
		if i == 0 && len(part) == 2 && part[1] == ':' && isLetter(part[0]) {
			return "", fmt.Errorf("%w: drive letter in %q", ErrPath, rel)
		}
		out = append(out, part)
	}
	if len(out) == 0 {
		return "", fmt.Errorf("%w: %q names no file", ErrPath, rel)
	}
	return strings.Join(out, "/"), nil
}

func isLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
