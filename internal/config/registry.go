package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Kind is how a config value is parsed and formatted.
type Kind int

// Value kinds.
const (
	KString   Kind = iota // public_url, listen, data_dir
	KSecret               // admin_token — like KString but masked in dumps/errors
	KList                 // trusted_proxies (comma-separated IPs)
	KInt                  // sessions, max_beams
	KBytes                // max_gz_bytes, max_body ("64MiB")
	KDuration             // *_ttl, max_age ("10m"; "0"/"off" = zero)
	KRate                 // rate_* ("5/min")
)

// key is one registry entry: the single source of truth for a setting.
type key struct {
	Name    string
	Kind    Kind
	Default string // canonical string; "" for admin_token, "" for data_dir (home-relative, filled in Load)
	Live    bool   // PATCH-able from /admin at runtime; false = restart-only
}

// registry is the ONE enumeration of keys, in file-template order. Load, the
// tower flags, /api/info and PATCH /api/admin/config all read it; nothing else
// enumerates keys. Ordering: restart-only first, then live-editable.
var registry = []key{
	{"public_url", KString, "http://localhost:8443", false},
	{"listen", KString, "127.0.0.1:8443", false},
	{"admin_token", KSecret, "", false},
	{"data_dir", KString, "", false},
	{"trusted_proxies", KList, "127.0.0.1,::1", false},
	{"sessions", KInt, "32", true},
	{"max_beams", KInt, "10", true},
	{"max_gz_bytes", KBytes, "64MiB", true},
	{"idle_ttl", KDuration, "30m", true},
	{"max_age", KDuration, "24h", true},
	{"warning_ttl", KDuration, "1m", true},
	{"terminated_ttl", KDuration, "1h", true},
	{"review_ttl", KDuration, "24h", true},
	{"max_body", KBytes, "8MiB", true},
	{"rate_create", KRate, "5/min", true},
	{"rate_join", KRate, "10/min", true},
	{"rate_frames", KRate, "30/s", true},
	{"rate_ping", KRate, "2/min", true},
	{"rate_extension", KRate, "3/h", true},
	{"rate_admin", KRate, "10/min", true},
}

func byName(name string) (key, bool) {
	for _, k := range registry {
		if k.Name == name {
			return k, true
		}
	}
	return key{}, false
}

// Keys returns every setting name in registry order.
func Keys() []string {
	out := make([]string, len(registry))
	for i, k := range registry {
		out[i] = k.Name
	}
	return out
}

// IsLive reports whether a key can be changed at runtime (vs restart-only).
func IsLive(name string) bool {
	k, ok := byName(name)
	return ok && k.Live
}

// parseValue validates raw for key k and returns its canonical string form plus
// the typed value (int, int64, time.Duration, Rate, string, or []string).
// max_age is the one duration allowed to be zero (off); the other durations,
// ints, byte sizes and rates must be positive.
func parseValue(k key, raw string) (canon string, val any, err error) {
	s := strings.TrimSpace(raw)
	switch k.Kind {
	case KString, KSecret:
		switch k.Name {
		case "public_url":
			if err := validURL(s); err != nil {
				return "", nil, err
			}
		case "listen":
			_, port, err := net.SplitHostPort(s)
			if err != nil {
				return "", nil, fmt.Errorf("not a host:port: %v", err)
			}
			if p, err := strconv.Atoi(port); err != nil || p < 0 || p > 65535 {
				return "", nil, fmt.Errorf("bad port %q", port)
			}
		}
		return s, s, nil
	case KList:
		var parts []string
		for _, p := range strings.Split(s, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if _, err := netip.ParseAddr(p); err != nil {
				return "", nil, fmt.Errorf("%q is not an IP address", p)
			}
			parts = append(parts, p)
		}
		return strings.Join(parts, ","), parts, nil
	case KInt:
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return "", nil, fmt.Errorf("must be a positive integer, got %q", raw)
		}
		return strconv.Itoa(n), n, nil
	case KBytes:
		b, err := parseBytes(s)
		if err != nil {
			return "", nil, err
		}
		if b < 1 {
			return "", nil, fmt.Errorf("must be at least 1 byte")
		}
		return formatBytes(b), b, nil
	case KDuration:
		d, err := parseDuration(s)
		if err != nil {
			return "", nil, err
		}
		if k.Name == "max_age" {
			if d < 0 {
				return "", nil, fmt.Errorf("must not be negative")
			}
		} else if d <= 0 {
			return "", nil, fmt.Errorf("must be a positive duration, got %q", raw)
		}
		return formatDuration(d), d, nil
	case KRate:
		r, err := parseRate(s)
		if err != nil {
			return "", nil, err
		}
		return formatRate(r), r, nil
	}
	return "", nil, fmt.Errorf("unknown kind")
}

func validURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("must be an absolute http(s) URL with a host, got %q", s)
	}
	return nil
}

// parseBytes reads "64MiB", "512KiB", "1GiB", or a bare byte count.
func parseBytes(s string) (int64, error) {
	for _, u := range []struct {
		suf string
		mul int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}} {
		if num, ok := strings.CutSuffix(s, u.suf); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("bad byte size %q", s)
			}
			return n * u.mul, nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad byte size %q (use e.g. 64MiB or a byte count)", s)
	}
	return n, nil
}

func formatBytes(b int64) string {
	for _, u := range []struct {
		suf string
		mul int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if b >= u.mul && b%u.mul == 0 {
			return strconv.FormatInt(b/u.mul, 10) + u.suf
		}
	}
	return strconv.FormatInt(b, 10)
}

func parseDuration(s string) (time.Duration, error) {
	if s == "0" || s == "off" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// formatDuration renders a duration compactly ("24h", "10m", "90s") rather than
// Go's "24h0m0s", so the admin dump and /api/info match the documented style.
func formatDuration(d time.Duration) string {
	if d == 0 {
		return "0"
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func parseRate(s string) (Rate, error) {
	n, unit, ok := strings.Cut(s, "/")
	if !ok {
		return Rate{}, fmt.Errorf("bad rate %q (use N/s, N/min or N/h)", s)
	}
	cnt, err := strconv.Atoi(strings.TrimSpace(n))
	if err != nil || cnt < 1 {
		return Rate{}, fmt.Errorf("bad rate count in %q", s)
	}
	var per time.Duration
	switch strings.TrimSpace(unit) {
	case "s", "sec":
		per = time.Second
	case "m", "min":
		per = time.Minute
	case "h", "hour":
		per = time.Hour
	default:
		return Rate{}, fmt.Errorf("bad rate unit %q (use s, min or h)", unit)
	}
	return Rate{N: cnt, Per: per}, nil
}

func formatRate(r Rate) string {
	unit := map[time.Duration]string{time.Second: "s", time.Minute: "min", time.Hour: "h"}[r.Per]
	return fmt.Sprintf("%d/%s", r.N, unit)
}
