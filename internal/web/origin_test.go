// SPDX-License-Identifier: AGPL-3.0-only

//go:build !devweb

package web

import (
	"strings"
	"testing"
)

// W4: the public URL rules, in a release build.
func TestParsePublicURL(t *testing.T) {
	good := []struct{ in, origin, authority string }{
		{"https://panel.example.com", "https://panel.example.com", "panel.example.com"},
		{"https://panel.example.com/", "https://panel.example.com", "panel.example.com"},
		{"https://panel.example.com:443", "https://panel.example.com", "panel.example.com"},
		{"https://panel.example.com:8443", "https://panel.example.com:8443", "panel.example.com:8443"},
		{"https://panel", "https://panel", "panel"},
		{"https://xn--bcher-kva.example", "https://xn--bcher-kva.example", "xn--bcher-kva.example"},
		{"https://a-b.c-d.example", "https://a-b.c-d.example", "a-b.c-d.example"},
		{"https://192.168.1.10", "https://192.168.1.10", "192.168.1.10"},
		{"https://192.168.1.10:9443", "https://192.168.1.10:9443", "192.168.1.10:9443"},
		{"https://[2001:db8::1]", "https://[2001:db8::1]", "[2001:db8::1]"},
		{"https://[2001:db8::1]:8443", "https://[2001:db8::1]:8443", "[2001:db8::1]:8443"},
		{"https://" + strings.Repeat("a", 63) + ".example", "https://" + strings.Repeat("a", 63) + ".example", strings.Repeat("a", 63) + ".example"},
	}
	for _, c := range good {
		o, err := ParsePublicURL(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if o.String() != c.origin || o.Authority() != c.authority || o.Scheme() != "https" {
			t.Errorf("%q: origin %q authority %q scheme %q; want %q %q https", c.in, o.String(), o.Authority(), o.Scheme(), c.origin, c.authority)
		}
	}
	bad := []string{
		"",
		"panel.example.com",
		"//panel.example.com",
		"http://panel.example.com",
		"http://localhost",
		"http://127.0.0.1:8080",
		"HTTPS://panel.example.com",
		"Https://panel.example.com",
		"ftp://panel.example.com",
		"https://",
		"https:///",
		"https://Panel.Example.com",
		"https://panel.example.com.",
		"https://panel..example.com",
		"https://.example.com",
		"https://-panel.example.com",
		"https://panel-.example.com",
		"https://pa_nel.example.com",
		"https://pänel.example.com",
		"https://panel.example.com/waystone",
		"https://panel.example.com//",
		"https://panel.example.com/%2F",
		"https://panel.example.com?x=1",
		"https://panel.example.com/?",
		"https://panel.example.com#top",
		"https://user@panel.example.com",
		"https://user:pw@panel.example.com",
		"https://panel.example.com:",
		"https://panel.example.com:0",
		"https://panel.example.com:65536",
		"https://panel.example.com:0443",
		"https://panel.example.com:+443",
		"https://panel.example.com:44a",
		"https://panel.example.com:443:443",
		"https://2001:db8::1",
		"https://[2001:db8::1",
		"https://[2001:DB8::1]",
		"https://[2001:db8:0::1]",
		"https://[fe80::1%25eth0]",
		"https://[::ffff:192.168.1.1]",
		"https://[192.168.1.1]",
		"https://192.168.1",
		"https://192.168.01.10",
		"https://1.2.3.4.5",
		"https://panel.123",
		"https://" + strings.Repeat("a", 64) + ".example",
		"https://" + strings.Repeat("a.", 127) + "example",
		" https://panel.example.com",
		"https://panel.example.com ",
		"https://panel.example.com\n",
		"https://panel.exa mple.com",
		"https://panel.example.com\\@evil.test",
		"https://evil.test\\.panel.example.com",
		"https://panel.example.com/" + strings.Repeat("a", 2100),
	}
	for _, in := range bad {
		if o, err := ParsePublicURL(in); err == nil {
			t.Errorf("%q accepted as %q", in, o.String())
		}
	}
}

// A Host (or X-Forwarded-Host) value matches the origin case-
// insensitively and with or without the default port, and in no other
// spelling.
func TestMatchAuthority(t *testing.T) {
	o, err := ParsePublicURL("https://panel.example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"panel.example.com", "PANEL.example.COM", "panel.example.com:443"} {
		if !o.matchAuthority(v) {
			t.Errorf("%q didn't match", v)
		}
	}
	for _, v := range []string{"", "panel.example.com.", "panel.example.com:8443", "panel.example.com:", "panel.example.com:0443",
		"evil.test", "panel.example.com.evil.test", "evil.test:443", "panel.example.com@evil.test", " panel.example.com",
		"panel.example.com:443:443", "[panel.example.com]", "panel.example.com:4430", strings.Repeat("a", 301)} {
		if o.matchAuthority(v) {
			t.Errorf("%q matched", v)
		}
	}

	p, err := ParsePublicURL("https://panel.example.com:8443")
	if err != nil {
		t.Fatal(err)
	}
	if !p.matchAuthority("panel.example.com:8443") || p.matchAuthority("panel.example.com") || p.matchAuthority("panel.example.com:443") {
		t.Error("a non-default port must be written and match exactly")
	}
	v6, err := ParsePublicURL("https://[2001:db8::1]")
	if err != nil {
		t.Fatal(err)
	}
	if !v6.matchAuthority("[2001:db8::1]") || !v6.matchAuthority("[2001:DB8::1]:443") || v6.matchAuthority("[2001:db8::2]") || v6.matchAuthority("2001:db8::1") {
		t.Error("IPv6 authority matching is wrong")
	}
}
