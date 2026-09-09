package server

import (
	"fmt"
	"net/url"
	"strings"
)

// ParsePublicURL splits a configured public_url into the base — the full URL
// including any path prefix, trailing slash trimmed, used verbatim to build
// join links — and the path prefix ("" or "/airlift") used for <base href> and
// GET /api/info. The router itself stays rooted; the reverse proxy strips the
// prefix before a request reaches the tower.
func ParsePublicURL(raw string) (base, basePath string, err error) {
	if raw == "" {
		raw = "http://localhost:8443"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", fmt.Errorf("public_url %q needs an http(s) scheme and a host", raw)
	}
	return strings.TrimRight(raw, "/"), strings.TrimRight(u.Path, "/"), nil
}
