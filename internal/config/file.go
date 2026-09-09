package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// parseFile reads a flat key=value file: full-line "#" comments only, blank
// lines ignored, "key = value" otherwise. Unknown keys are an error (typo
// protection). A missing file is (nil, nil), not an error.
func parseFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i, ln := range strings.Split(string(data), "\n") {
		s := strings.TrimSpace(ln)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		// Strip an inline "# comment" (no config value contains '#'), so
		// uncommenting a template line like "listen = ...  # restart-only" works.
		if h := strings.IndexByte(s, '#'); h >= 0 {
			s = strings.TrimSpace(s[:h])
			if s == "" {
				continue
			}
		}
		name, val, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected key = value", path, i+1)
		}
		name = strings.TrimSpace(name)
		if _, known := byName(name); !known {
			return nil, fmt.Errorf("%s:%d: unknown key %q", path, i+1, name)
		}
		out[name] = strings.TrimSpace(val)
	}
	return out, nil
}

// parseFileEnforcing parses path, then requires mode 0600 (no group/other bits)
// if it sets a non-empty admin_token, so a token never sits in a world- or
// group-readable file.
func parseFileEnforcing(path string) (map[string]string, error) {
	m, err := parseFile(path)
	if err != nil || m == nil {
		return m, err
	}
	if tok, ok := m["admin_token"]; ok && tok != "" {
		fi, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("%s holds admin_token but is mode %#o; run: chmod 600 %s", path, fi.Mode().Perm(), path)
		}
	}
	return m, nil
}

// ensureHomeAndTemplate creates <home> and, only if <home>/config is absent,
// writes a commented template (0644 — it holds no secret) that changes nothing
// but documents every key. It never overwrites an existing file.
func ensureHomeAndTemplate(home, cfgPath string) error {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(cfgPath); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.WriteFile(cfgPath, []byte(template()), 0o644)
}

func template() string {
	var b strings.Builder
	b.WriteString("# airlift tower configuration  (~/.airlift/config)\n")
	b.WriteString("# flat key = value, full-line \"#\" comments. Precedence, low to high:\n")
	b.WriteString("#   file < ~/.airlift/overrides < env AIRLIFT_<KEY> < --<key> flag\n")
	b.WriteString("# Restart-only keys are marked; the rest are live-editable from /admin.\n\n")
	for _, k := range registry {
		def := k.Default
		if k.Name == "data_dir" {
			def = "~/.airlift/data"
		}
		line := fmt.Sprintf("# %s = %s", k.Name, def)
		switch {
		case k.Name == "admin_token":
			line = "# admin_token =                 # restart-only; set to enable /admin (chmod 600 this file)"
		case !k.Live:
			line += strings.Repeat(" ", max(1, 34-len(line))) + "# restart-only"
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// WriteOverrides merges live-key changes into <home>/overrides and returns the
// reloaded config. Restart-only or unknown keys are rejected and every value is
// validated before anything is written; the file is written atomically at 0600.
// Precedence still holds: a key also pinned by a flag or AIRLIFT_<KEY> env
// value keeps that value in the returned Config even after the override is
// written — the admin dump shows the winning source, so a pinned key is
// visible as such rather than silently ignored.
func WriteOverrides(p Params, changes map[string]string) (*Config, error) {
	env := p.Env
	if env == nil {
		env = os.Getenv
	}
	home, err := resolveHome(p, env)
	if err != nil {
		return nil, err
	}
	ovrPath := filepath.Join(home, "overrides")

	canon := map[string]string{}
	for name, val := range changes {
		k, ok := byName(name)
		if !ok {
			return nil, fmt.Errorf("unknown key %q", name)
		}
		if !k.Live {
			return nil, fmt.Errorf("%q is restart-only; edit the config file and restart", name)
		}
		c, _, err := parseValue(k, val)
		if err != nil {
			return nil, fmt.Errorf("%s=%q: %w", name, val, err)
		}
		canon[name] = c
	}

	cur, err := parseFile(ovrPath)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		cur = map[string]string{}
	}
	for name, c := range canon {
		cur[name] = c
	}
	if err := writeAtomic(ovrPath, renderKV(cur), 0o600); err != nil {
		return nil, err
	}
	return Load(p)
}

// renderKV writes "key = value" lines in registry order under a header.
func renderKV(vals map[string]string) []byte {
	var b strings.Builder
	b.WriteString("# written by airlift admin — live config overrides\n")
	for _, k := range registry {
		if v, ok := vals[k.Name]; ok {
			fmt.Fprintf(&b, "%s = %s\n", k.Name, v)
		}
	}
	return []byte(b.String())
}

// writeAtomic writes data to path via a temp file in the same directory then
// renames it into place (atomic on POSIX).
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".overrides-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
