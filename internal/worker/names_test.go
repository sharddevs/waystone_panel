// SPDX-License-Identifier: AGPL-3.0-only

package worker

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	for _, name := range []string{
		"server.properties", "eula.txt", "server.jar", "a", "0", "white-list_v2.json",
		strings.Repeat("a", MaxNameBytes),
	} {
		if err := ValidName(name); err != nil {
			t.Errorf("ValidName(%q): %v", name, err)
		}
	}
	for _, name := range []string{
		"", ".", "..", ".hidden", ".server.properties.tmp-123", "-rf", "_x",
		"a/b", "../x", "/etc/passwd", "world/level.dat", `a\b`, "a b", "a\x00b", "a\nb",
		"eula.txt\n", "café", strings.Repeat("a", MaxNameBytes+1),
	} {
		if err := ValidName(name); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidName(%q) = %v, want ErrInvalid", name, err)
		}
	}
}

func TestExpectString(t *testing.T) {
	if got := ExpectAbsent().String(); got != "absent" {
		t.Errorf("ExpectAbsent().String() = %q, want absent", got)
	}
	// sha256("hello")
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got := ExpectContent([]byte("hello")).String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestParseExpect(t *testing.T) {
	for _, e := range []Expect{ExpectAbsent(), ExpectContent(nil), ExpectContent([]byte("hello"))} {
		got, err := ParseExpect(e.String())
		if err != nil || got != e {
			t.Errorf("ParseExpect(%q) = %+v, %v; want %+v", e.String(), got, err, e)
		}
	}
	hex := ExpectContent([]byte("hello")).String()
	for _, s := range []string{
		"", "Absent", "absent ", "none", hex[:63], hex + "0", strings.ToUpper(hex),
		"zz" + hex[2:], "-" + hex[1:],
	} {
		if _, err := ParseExpect(s); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseExpect(%q) = %v, want ErrInvalid", s, err)
		}
	}
}

// The zero Expect must never match a file that exists or a missing one.
func TestZeroExpectMatchesNothing(t *testing.T) {
	var zero Expect
	if zero.Absent || zero == ExpectContent(nil) || zero.SHA256 == sha256.Sum256([]byte{}) {
		t.Errorf("zero Expect %+v matches something", zero)
	}
}
