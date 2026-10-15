// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Origin is the canonical public origin: the one scheme, host and port
// the browser-facing side answers to. Build it with ParsePublicURL.
type Origin struct {
	scheme string // "https", or "http" in devweb builds
	host   string // lowercase DNS name, IPv4 address or bracketed IPv6
	port   int    // 0 means the scheme's default port
}

// maxPublicURL bounds the configured value, so a mistake can't turn into
// a huge allocation or log line.
const maxPublicURL = 2048

// devHosts are the only hosts an http public URL may name, and only in
// devweb builds.
var devHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "[::1]": true}

// ParsePublicURL checks WAYSTONE_PUBLIC_URL and returns its canonical
// origin (W4). It is strict on purpose: the value is typed once by an
// administrator, and anything unusual is more likely a mistake than a
// need.
//
//   - It must start with "https://" (lowercase). In devweb builds,
//     "http://" is also accepted, for localhost, 127.0.0.1 and [::1] only.
//   - The host is a lowercase DNS name (ASCII; an IDN is written in
//     punycode), an IPv4 address, or an IPv6 address in brackets, written
//     in its canonical form and without a zone. No trailing dot.
//   - An optional port, 1 to 65535, without leading zeros. The scheme's
//     default port is accepted and dropped.
//   - The path is empty or "/". No user info, query or fragment.
func ParsePublicURL(raw string) (Origin, error) {
	if raw == "" {
		return Origin{}, errors.New("the public URL is required")
	}
	if len(raw) > maxPublicURL {
		return Origin{}, fmt.Errorf("the public URL is longer than %d bytes", maxPublicURL)
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= ' ' || raw[i] >= 0x7f {
			return Origin{}, errors.New("the public URL may only contain printable ASCII, without spaces")
		}
	}
	var scheme string
	switch {
	case strings.HasPrefix(raw, "https://"):
		scheme = "https"
	case devHTTP && strings.HasPrefix(raw, "http://"):
		scheme = "http"
	default:
		if devHTTP {
			return Origin{}, errors.New("the public URL must start with https:// (or http:// for localhost in this devweb build)")
		}
		return Origin{}, errors.New("the public URL must start with https://")
	}
	if strings.ContainsAny(raw, "#?\\") {
		return Origin{}, errors.New("the public URL may not have a query or fragment")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Origin{}, fmt.Errorf("the public URL doesn't parse: %w", err)
	}
	if u.User != nil || strings.Contains(u.Host, "@") {
		return Origin{}, errors.New("the public URL may not contain user info")
	}
	if u.Opaque != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return Origin{}, errors.New("the public URL may not have a path: Waystone is served at the root of its host")
	}
	host, port, err := splitAuthority(u.Host)
	if err != nil {
		return Origin{}, fmt.Errorf("the public URL's host: %w", err)
	}
	if err := checkHost(host); err != nil {
		return Origin{}, fmt.Errorf("the public URL's host: %w", err)
	}
	o := Origin{scheme: scheme, host: host}
	if port != "" {
		n, err := parsePort(port)
		if err != nil {
			return Origin{}, fmt.Errorf("the public URL's port: %w", err)
		}
		if n != defaultPort(scheme) {
			o.port = n
		}
	}
	if scheme == "http" && !devHosts[host] {
		return Origin{}, errors.New("an http public URL may only name localhost, 127.0.0.1 or [::1]")
	}
	return o, nil
}

// Scheme is "https" (or "http" in a devweb build).
func (o Origin) Scheme() string { return o.scheme }

// Authority is what a matching Host header holds: the host, plus the
// port when it isn't the scheme's default.
func (o Origin) Authority() string {
	if o.port == 0 {
		return o.host
	}
	return o.host + ":" + strconv.Itoa(o.port)
}

// String is the origin as a browser serializes it in an Origin header,
// e.g. "https://panel.example.com".
func (o Origin) String() string { return o.scheme + "://" + o.Authority() }

// IsZero reports whether o was never set.
func (o Origin) IsZero() bool { return o.scheme == "" }

// matchAuthority reports whether a Host (or X-Forwarded-Host) value names
// this origin. Host names are case-insensitive, and the scheme's default
// port may be written or left out; nothing else is forgiven. In
// particular a trailing dot doesn't match, so there is exactly one
// spelling per origin apart from case and the default port.
func (o Origin) matchAuthority(v string) bool {
	if v == "" || len(v) > 300 {
		return false
	}
	host, port, err := splitAuthority(strings.ToLower(v))
	if err != nil || host != o.host {
		return false
	}
	if port == "" {
		return o.port == 0
	}
	n, err := parsePort(port)
	if err != nil {
		return false
	}
	if n == defaultPort(o.scheme) {
		n = 0
	}
	return n == o.port
}

// splitAuthority splits "host", "host:port", "[v6]" or "[v6]:port". The
// host keeps its brackets.
func splitAuthority(a string) (host, port string, err error) {
	if a == "" {
		return "", "", errors.New("missing")
	}
	if a[0] == '[' {
		end := strings.IndexByte(a, ']')
		if end < 0 {
			return "", "", errors.New("unclosed '['")
		}
		host, rest := a[:end+1], a[end+1:]
		if rest == "" {
			return host, "", nil
		}
		if rest[0] != ':' || len(rest) == 1 {
			return "", "", errors.New("unexpected text after ']'")
		}
		return host, rest[1:], nil
	}
	if strings.Count(a, ":") > 1 {
		return "", "", errors.New("an IPv6 address must be in brackets")
	}
	host, port, found := strings.Cut(a, ":")
	if found && port == "" {
		return "", "", errors.New("empty port")
	}
	return host, port, nil
}

// checkHost accepts a canonical IPv4 address, a canonical bracketed IPv6
// address without a zone, or a lowercase DNS name.
func checkHost(h string) error {
	if strings.HasPrefix(h, "[") {
		addr, err := netip.ParseAddr(h[1 : len(h)-1])
		if err != nil || !addr.Is6() || addr.Zone() != "" || addr.Is4In6() {
			return errors.New("not a valid IPv6 address (no zones, no IPv4-mapped form)")
		}
		if "["+addr.String()+"]" != h {
			return fmt.Errorf("write the IPv6 address in its canonical form, [%s]", addr)
		}
		return nil
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		if !addr.Is4() {
			return errors.New("an IPv6 address must be in brackets")
		}
		return nil
	}
	if len(h) > 253 {
		return errors.New("longer than 253 characters")
	}
	if strings.HasSuffix(h, ".") {
		return errors.New("no trailing dot")
	}
	labels := strings.Split(h, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 {
			return errors.New("each label must be 1 to 63 characters")
		}
		if l[0] == '-' || l[len(l)-1] == '-' {
			return errors.New("a label may not start or end with '-'")
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return errors.New("only lowercase letters, digits, '-' and '.' are allowed (write an IDN in punycode)")
			}
		}
	}
	last := labels[len(labels)-1]
	if strings.Trim(last, "0123456789") == "" {
		return errors.New("looks like a malformed IP address (all-numeric last label)")
	}
	return nil
}

// parsePort accepts 1 to 65535 in plain decimal, without leading zeros.
func parsePort(p string) (int, error) {
	if p == "" || len(p) > 5 || p[0] == '0' {
		return 0, errors.New("must be 1 to 65535")
	}
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return 0, errors.New("must be 1 to 65535")
		}
	}
	n, _ := strconv.Atoi(p)
	if n > 65535 {
		return 0, errors.New("must be 1 to 65535")
	}
	return n, nil
}

func defaultPort(scheme string) int {
	if scheme == "http" {
		return 80
	}
	return 443
}
