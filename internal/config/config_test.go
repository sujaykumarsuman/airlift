package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// load with an injected env map and no ambient environment.
func load(t *testing.T, home string, env, flags map[string]string) (*Config, error) {
	t.Helper()
	return Load(Params{
		Home:  home,
		Env:   func(k string) string { return env[k] },
		Flags: flags,
	})
}

func TestDefaults(t *testing.T) {
	home := t.TempDir()
	c, err := load(t, home, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Sessions != 32 || c.MaxBeams != 10 {
		t.Fatalf("sessions=%d max_beams=%d", c.Sessions, c.MaxBeams)
	}
	if c.MaxAge != 24*time.Hour {
		t.Fatalf("max_age default = %v, want 24h", c.MaxAge)
	}
	if c.IdleTTL != 30*time.Minute || c.TerminatedTTL != time.Hour {
		t.Fatalf("ttls idle=%v terminated=%v", c.IdleTTL, c.TerminatedTTL)
	}
	if c.MaxGzBytes != 64<<20 || c.MaxBody != 8<<20 {
		t.Fatalf("bytes %d %d", c.MaxGzBytes, c.MaxBody)
	}
	if c.RateFrames != (Rate{30, time.Second}) || c.RateCreate != (Rate{5, time.Minute}) {
		t.Fatalf("rates %+v %+v", c.RateFrames, c.RateCreate)
	}
	if c.DataDir != filepath.Join(home, "data") {
		t.Fatalf("data_dir %q", c.DataDir)
	}
	if c.AdminEnabled() {
		t.Fatalf("admin should be disabled by default")
	}
	if _, src, _ := c.Get("sessions"); src != LayerDefault {
		t.Fatalf("source %v", src)
	}
}

func TestPrecedence(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "config"), "sessions = 40\n", 0o644)
	write(t, filepath.Join(home, "overrides"), "sessions = 41\n", 0o600)

	// file < overrides < env < flag
	cases := []struct {
		name  string
		env   map[string]string
		flags map[string]string
		want  int
		src   Layer
	}{
		{"file+overrides", nil, nil, 41, LayerOverrides},
		{"env wins", map[string]string{"AIRLIFT_SESSIONS": "42"}, nil, 42, LayerEnv},
		{"flag wins", map[string]string{"AIRLIFT_SESSIONS": "42"}, map[string]string{"sessions": "43"}, 43, LayerFlag},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := load(t, home, tc.env, tc.flags)
			if err != nil {
				t.Fatal(err)
			}
			if c.Sessions != tc.want {
				t.Fatalf("sessions %d, want %d", c.Sessions, tc.want)
			}
			if _, src, _ := c.Get("sessions"); src != tc.src {
				t.Fatalf("source %v, want %v", src, tc.src)
			}
		})
	}
	// With no config file at all, defaults hold (file layer just absent).
	c, _ := load(t, t.TempDir(), nil, nil)
	if c.Sessions != 32 {
		t.Fatalf("bare default %d", c.Sessions)
	}
}

func TestParseKinds(t *testing.T) {
	ok := map[string]string{
		"sessions = 5":                                "sessions",
		"max_gz_bytes = 512KiB":                       "max_gz_bytes",
		"max_body = 1048576":                          "max_body",
		"idle_ttl = 90s":                              "idle_ttl",
		"max_age = 0":                                 "max_age",
		"rate_frames = 30/s":                          "rate_frames",
		"rate_extension = 3/h":                        "rate_extension",
		"trusted_proxies = 10.0.0.1,::1":              "trusted_proxies",
		"trusted_proxies = 10.42.0.0/16":              "trusted_proxies",
		"trusted_proxies = 10.0.0.1,10.42.0.0/16,::1": "trusted_proxies",
		"public_url = https://x.example/airlift":      "public_url",
		"listen = 0.0.0.0:9000":                       "listen",
	}
	for line := range ok {
		home := t.TempDir()
		write(t, filepath.Join(home, "config"), line+"\n", 0o644)
		if _, err := load(t, home, nil, nil); err != nil {
			t.Errorf("%q: %v", line, err)
		}
	}
	bad := []string{
		"sessions = 0", "sessions = -1", "sessions = x",
		"max_gz_bytes = 0", "max_gz_bytes = big",
		"idle_ttl = 0", // non-max_age ttl must be positive
		"idle_ttl = nope",
		"rate_frames = 30", // no unit
		"rate_frames = 30/decade",
		"trusted_proxies = not-an-ip", "trusted_proxies = 10.0.0.0/99",
		"public_url = ftp://x", "public_url = /relative",
		"listen = noport", "listen = 1.2.3.4:notaport", "listen = 1.2.3.4:99999",
	}
	for _, line := range bad {
		home := t.TempDir()
		write(t, filepath.Join(home, "config"), line+"\n", 0o644)
		if _, err := load(t, home, nil, nil); err == nil {
			t.Errorf("%q: accepted, want error", line)
		}
	}
	// A public_url with a path prefix parses (the base path is derived from it
	// by internal/server.ParsePublicURL, not here).
	home := t.TempDir()
	write(t, filepath.Join(home, "config"), "public_url = https://h.example/airlift/\n", 0o644)
	if c, err := load(t, home, nil, nil); err != nil || c.PublicURL != "https://h.example/airlift/" {
		t.Fatalf("public_url %q err %v", c.PublicURL, err)
	}
}

func TestUnknownKeyFatal(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "config"), "sesions = 40\n", 0o644) // typo
	_, err := load(t, home, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("err = %v", err)
	}
}

func TestAdminToken0600(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "config")
	write(t, cfg, "admin_token = s3cr3t-value\n", 0o644) // too open
	_, err := load(t, home, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("expected a 0600 refusal, got %v", err)
	}
	if err := os.Chmod(cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := load(t, home, nil, nil)
	if err != nil {
		t.Fatalf("0600 file rejected: %v", err)
	}
	if !c.AdminEnabled() {
		t.Fatal("admin should be enabled")
	}
	// The token is masked in the effective dump.
	for _, kv := range c.Effective() {
		if kv.Name == "admin_token" && kv.Value != "****" {
			t.Fatalf("token not masked: %q", kv.Value)
		}
	}
}

func TestTemplateCreatedOnce(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "config")
	if _, err := Load(Params{Home: home, CreateFile: true, Env: func(string) string { return "" }}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# sessions = 32") || !strings.Contains(string(data), "restart-only") {
		t.Fatalf("template:\n%s", data)
	}
	// A real setting the operator added survives a second start.
	write(t, cfg, string(data)+"\nsessions = 7\n", 0o644)
	c, err := Load(Params{Home: home, CreateFile: true, Env: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	if c.Sessions != 7 {
		t.Fatalf("template overwrote the file: sessions=%d", c.Sessions)
	}
}

func TestWriteOverrides(t *testing.T) {
	home := t.TempDir()
	p := Params{Home: home, Env: func(string) string { return "" }}

	c, err := WriteOverrides(p, map[string]string{"sessions": "12", "idle_ttl": "5m"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Sessions != 12 || c.IdleTTL != 5*time.Minute {
		t.Fatalf("after write: sessions=%d idle=%v", c.Sessions, c.IdleTTL)
	}
	if _, src, _ := c.Get("sessions"); src != LayerOverrides {
		t.Fatalf("source %v", src)
	}
	// Persisted and 0600.
	fi, err := os.Stat(filepath.Join(home, "overrides"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("overrides mode %v err %v", fi.Mode().Perm(), err)
	}
	// Restart-only, unknown, and invalid are all refused.
	for _, ch := range []map[string]string{
		{"listen": "0.0.0.0:1"},
		{"nope": "1"},
		{"sessions": "0"},
	} {
		if _, err := WriteOverrides(p, ch); err == nil {
			t.Errorf("%v: accepted, want refusal", ch)
		}
	}
	// A refused write did not corrupt the earlier value.
	c2, _ := Load(p)
	if c2.Sessions != 12 {
		t.Fatalf("overrides clobbered: sessions=%d", c2.Sessions)
	}
}

func TestInlineComments(t *testing.T) {
	home := t.TempDir()
	// An inline comment on a value, and a whole-line comment, are both stripped.
	write(t, filepath.Join(home, "config"), "sessions = 5   # keep it small\n# max_beams = 99\n", 0o644)
	c, err := load(t, home, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Sessions != 5 || c.MaxBeams != 10 {
		t.Fatalf("sessions=%d max_beams=%d", c.Sessions, c.MaxBeams)
	}
	// Uncommenting a restart-only template line (which carries a trailing
	// "# restart-only") must yield a clean value, not one with the comment.
	write(t, filepath.Join(home, "config"), "listen = 0.0.0.0:9000   # restart-only\n", 0o644)
	c, err = load(t, home, nil, nil)
	if err != nil || c.Listen != "0.0.0.0:9000" {
		t.Fatalf("listen=%q err=%v", c.Listen, err)
	}
}

func TestDataDirTildeExpands(t *testing.T) {
	uh, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no user home")
	}
	home := t.TempDir()
	write(t, filepath.Join(home, "config"), "data_dir = ~/airlift-data-test\n", 0o644)
	c, err := load(t, home, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != filepath.Join(uh, "airlift-data-test") {
		t.Fatalf("data_dir %q, want %q", c.DataDir, filepath.Join(uh, "airlift-data-test"))
	}
}

func TestDurationCanonicalCompact(t *testing.T) {
	c, _ := load(t, t.TempDir(), nil, nil)
	want := map[string]string{"max_age": "24h", "idle_ttl": "30m", "warning_ttl": "1m", "rate_frames": "30/s"}
	got := map[string]string{}
	for _, kv := range c.Effective() {
		got[kv.Name] = kv.Value
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s canonical = %q, want %q", k, got[k], v)
		}
	}
}

func TestGetMasksSecret(t *testing.T) {
	home := t.TempDir()
	c, err := load(t, home, map[string]string{"AIRLIFT_ADMIN_TOKEN": "live-secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminToken != "live-secret" {
		t.Fatalf("field should hold the real token, got %q", c.AdminToken)
	}
	if v, _, _ := c.Get("admin_token"); v != "****" {
		t.Fatalf("Get leaked the token: %q", v)
	}
}

func TestEmptyEnvIsUnset(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "config"), "sessions = 40\n", 0o644)
	// AIRLIFT_SESSIONS="" must NOT shadow the file value.
	c, err := load(t, home, map[string]string{"AIRLIFT_SESSIONS": ""}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Sessions != 40 {
		t.Fatalf("empty env shadowed the file: %d", c.Sessions)
	}
}
