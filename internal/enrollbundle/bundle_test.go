// SPDX-License-Identifier: AGPL-3.0-only

package enrollbundle

import (
	"encoding/base64"
	"net/netip"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	b := Bundle{Addr: netip.MustParseAddrPort("10.0.20.80:7443")}
	b.Pin[0], b.Secret[31] = 0xab, 0x01
	s, err := Encode(b)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(s, "+/= \n") {
		t.Fatalf("not one base64url line: %q", s)
	}
	got, err := Decode("  " + s + "\n")
	if err != nil || got != b {
		t.Fatalf("Decode = %+v, %v", got, err)
	}
}

func TestDecodeRefuses(t *testing.T) {
	enc := func(j string) string { return base64.RawURLEncoding.EncodeToString([]byte(j)) }
	pin := strings.Repeat("ab", 32)
	sec := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	good := `{"v":1,"addr":"10.0.0.1:7443","pin":"` + pin + `","secret":"` + sec + `"}`
	if _, err := Decode(enc(good)); err != nil {
		t.Fatalf("good bundle: %v", err)
	}
	for name, s := range map[string]string{
		"empty":         "",
		"too long":      strings.Repeat("a", MaxEncodedBytes+1),
		"not base64":    "!!!",
		"not json":      enc("nope"),
		"version 2":     enc(strings.Replace(good, `"v":1`, `"v":2`, 1)),
		"host name":     enc(strings.Replace(good, "10.0.0.1:7443", "panel.example.com:7443", 1)),
		"no port":       enc(strings.Replace(good, "10.0.0.1:7443", "10.0.0.1", 1)),
		"port 0":        enc(strings.Replace(good, ":7443", ":0", 1)),
		"zone":          enc(strings.Replace(good, "10.0.0.1:7443", "[fe80::1%eth0]:7443", 1)),
		"unspecified":   enc(strings.Replace(good, "10.0.0.1:7443", "0.0.0.0:7443", 1)),
		"unspecified 6": enc(strings.Replace(good, "10.0.0.1:7443", "[::]:7443", 1)),
		"multicast":     enc(strings.Replace(good, "10.0.0.1:7443", "239.1.1.1:7443", 1)),
		"multicast 6":   enc(strings.Replace(good, "10.0.0.1:7443", "[ff0e::1]:7443", 1)),
		"link-local":    enc(strings.Replace(good, "10.0.0.1:7443", "169.254.1.1:7443", 1)),
		"link-local 6":  enc(strings.Replace(good, "10.0.0.1:7443", "[fe80::1]:7443", 1)),
		"short pin":     enc(strings.Replace(good, pin, pin[:62], 1)),
		"short secret":  enc(strings.Replace(good, sec, base64.RawURLEncoding.EncodeToString(make([]byte, 31)), 1)),
		"unknown field": enc(strings.Replace(good, `"v":1`, `"v":1,"x":1`, 1)),
		"two values":    enc(good + good),
	} {
		if _, err := Decode(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// 6a-I5: Encode refuses the same addresses Decode does.
func TestEncodeRefusesAddrs(t *testing.T) {
	for _, a := range []string{"0.0.0.0:7443", "[::]:7443", "239.1.1.1:7443", "169.254.1.1:7443", "[ff0e::1]:7443", "[fe80::1]:7443", "10.0.0.1:0"} {
		if _, err := Encode(Bundle{Addr: netip.MustParseAddrPort(a)}); err == nil {
			t.Errorf("Encode(%s) accepted", a)
		}
	}
	if _, err := Encode(Bundle{Addr: netip.MustParseAddrPort("[2001:db8::1]:7443")}); err != nil {
		t.Errorf("a global IPv6 address: %v", err)
	}
}
