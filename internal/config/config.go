// Package config resolves the tower's settings from, in decreasing priority, a
// tower flag, the environment (AIRLIFT_<KEY>), the admin-written overrides file,
// the ~/.airlift/config file, and the built-in default (prompt 002 decision 2,
// ADR 0012). The registry (registry.go) is the single source of truth for every
// key, its kind, default and whether it is live-editable. A resolved Config is
// an immutable snapshot; a live admin change is WriteOverrides + a fresh Load.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Layer is where a resolved value came from, lowest to highest priority.
type Layer int

// Resolution layers.
const (
	LayerDefault Layer = iota
	LayerFile
	LayerOverrides
	LayerEnv
	LayerFlag
)

func (l Layer) String() string {
	switch l {
	case LayerFile:
		return "file"
	case LayerOverrides:
		return "overrides"
	case LayerEnv:
		return "env"
	case LayerFlag:
		return "flag"
	default:
		return "default"
	}
}

// Rate is a per-address "N per Per" budget (the rate_* keys).
type Rate struct {
	N   int
	Per time.Duration
}

// Config is the resolved, typed, validated tower configuration.
type Config struct {
	PublicURL      string
	Listen         string
	AdminToken     string // "" = admin disabled
	DataDir        string // absolute
	TrustedProxies []string
	Sessions       int
	MaxBeams       int
	MaxGzBytes     int64
	IdleTTL        time.Duration
	InactiveTTL    time.Duration
	MaxAge         time.Duration // 0 = off
	WarningTTL     time.Duration
	TerminatedTTL  time.Duration
	ReviewTTL      time.Duration
	MaxBody        int64
	RateCreate     Rate
	RateJoin       Rate
	RateFrames     Rate
	RatePing       Rate
	RateExtension  Rate
	RateAdmin      Rate

	Home          string
	ConfigPath    string
	OverridesPath string

	sources map[string]Layer
	raw     map[string]string
}

// AdminEnabled reports whether an admin_token is configured.
func (c *Config) AdminEnabled() bool { return c.AdminToken != "" }

// BasePath is the path prefix from public_url ("" or e.g. "/airlift"), used for
// <base href>, service-worker scope and every generated link (decision 12).
func (c *Config) BasePath() string {
	u, err := url.Parse(c.PublicURL)
	if err != nil {
		return ""
	}
	return strings.TrimRight(u.Path, "/")
}

// KeyView is one row of the admin config dump.
type KeyView struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Source string `json:"source"`
	Live   bool   `json:"live"`
}

// Get returns the winning canonical string for a key and where it came from.
// A secret (admin_token) is masked; read the live token via the AdminToken
// field, never through Get.
func (c *Config) Get(name string) (value string, src Layer, ok bool) {
	v, ok := c.raw[name]
	if k, known := byName(name); known && k.Kind == KSecret && v != "" {
		v = "****"
	}
	return v, c.sources[name], ok
}

// Effective lists every key with its value and source, secrets masked — the
// body of GET /api/admin/config.
func (c *Config) Effective() []KeyView {
	out := make([]KeyView, 0, len(registry))
	for _, k := range registry {
		v := c.raw[k.Name]
		if k.Kind == KSecret && v != "" {
			v = "****"
		}
		out = append(out, KeyView{Name: k.Name, Value: v, Source: c.sources[k.Name].String(), Live: k.Live})
	}
	return out
}

// Params drive Load. Env, Home and ConfigFile are injectable for tests.
type Params struct {
	Env        func(string) string // default os.Getenv
	Home       string              // "" => $AIRLIFT_HOME or <userhome>/.airlift
	ConfigFile string              // --config FILE; "" => <home>/config
	Flags      map[string]string   // only flags the user actually set (fs.Visit)
	CreateFile bool                // tower: write a commented template if <home>/config is absent
}

// Load resolves the configuration by precedence and returns an immutable,
// validated snapshot. Missing files are empty, not an error; a file that sets a
// non-empty admin_token must be mode 0600 or Load refuses.
func Load(p Params) (*Config, error) {
	env := p.Env
	if env == nil {
		env = os.Getenv
	}
	home, err := resolveHome(p, env)
	if err != nil {
		return nil, err
	}
	cfgPath := p.ConfigFile
	if cfgPath == "" {
		cfgPath = filepath.Join(home, "config")
	}
	ovrPath := filepath.Join(home, "overrides")

	if p.CreateFile {
		if err := ensureHomeAndTemplate(home, cfgPath); err != nil {
			return nil, err
		}
	}
	fileVals, err := parseFileEnforcing(cfgPath)
	if err != nil {
		return nil, err
	}
	ovrVals, err := parseFileEnforcing(ovrPath)
	if err != nil {
		return nil, err
	}

	c := &Config{
		Home: home, ConfigPath: cfgPath, OverridesPath: ovrPath,
		sources: map[string]Layer{}, raw: map[string]string{},
	}
	for _, k := range registry {
		rawVal, layer := resolve(k, p.Flags, env, ovrVals, fileVals)
		if k.Name == "data_dir" && strings.TrimSpace(rawVal) == "" {
			rawVal = filepath.Join(home, "data")
		}
		canon, val, err := parseValue(k, rawVal)
		if err != nil {
			return nil, fmt.Errorf("config %s (%s): %w", k.Name, layer, err)
		}
		if k.Name == "data_dir" {
			p := expandUser(canon)
			if abs, aerr := filepath.Abs(p); aerr == nil {
				canon, val = abs, abs
			}
		}
		assign(c, k.Name, val)
		c.sources[k.Name] = layer
		c.raw[k.Name] = canon
	}
	return c, nil
}

// expandUser resolves a leading "~" or "~/" against the user's home directory
// (the conventional meaning), so data_dir = ~/x is not taken literally.
func expandUser(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	uh, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return uh
	}
	return filepath.Join(uh, p[2:])
}

// resolveHome applies $AIRLIFT_HOME then <userhome>/.airlift.
func resolveHome(p Params, env func(string) string) (string, error) {
	if p.Home != "" {
		return p.Home, nil
	}
	if h := env("AIRLIFT_HOME"); h != "" {
		return h, nil
	}
	uh, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w (set AIRLIFT_HOME)", err)
	}
	return filepath.Join(uh, ".airlift"), nil
}

// resolve applies flag > env > overrides > file > default for one key. A flag
// or env value present but empty still counts as "set" only for flags (the
// tower passes only visited flags); an empty AIRLIFT_<KEY> is treated as unset.
func resolve(k key, flags map[string]string, env func(string) string, ovr, file map[string]string) (string, Layer) {
	if v, ok := flags[k.Name]; ok {
		return v, LayerFlag
	}
	if v := env("AIRLIFT_" + strings.ToUpper(k.Name)); v != "" {
		return v, LayerEnv
	}
	if v, ok := ovr[k.Name]; ok {
		return v, LayerOverrides
	}
	if v, ok := file[k.Name]; ok {
		return v, LayerFile
	}
	return k.Default, LayerDefault
}

// assign writes a parsed value into the Config field for name.
func assign(c *Config, name string, val any) {
	switch name {
	case "public_url":
		c.PublicURL = val.(string)
	case "listen":
		c.Listen = val.(string)
	case "admin_token":
		c.AdminToken = val.(string)
	case "data_dir":
		c.DataDir = val.(string)
	case "trusted_proxies":
		if v, ok := val.([]string); ok {
			c.TrustedProxies = v
		}
	case "sessions":
		c.Sessions = val.(int)
	case "max_beams":
		c.MaxBeams = val.(int)
	case "max_gz_bytes":
		c.MaxGzBytes = val.(int64)
	case "idle_ttl":
		c.IdleTTL = val.(time.Duration)
	case "inactive_ttl":
		c.InactiveTTL = val.(time.Duration)
	case "max_age":
		c.MaxAge = val.(time.Duration)
	case "warning_ttl":
		c.WarningTTL = val.(time.Duration)
	case "terminated_ttl":
		c.TerminatedTTL = val.(time.Duration)
	case "review_ttl":
		c.ReviewTTL = val.(time.Duration)
	case "max_body":
		c.MaxBody = val.(int64)
	case "rate_create":
		c.RateCreate = val.(Rate)
	case "rate_join":
		c.RateJoin = val.(Rate)
	case "rate_frames":
		c.RateFrames = val.(Rate)
	case "rate_ping":
		c.RatePing = val.(Rate)
	case "rate_extension":
		c.RateExtension = val.(Rate)
	case "rate_admin":
		c.RateAdmin = val.(Rate)
	}
}
