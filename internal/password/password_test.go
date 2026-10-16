// SPDX-License-Identifier: AGPL-3.0-only

package password

import (
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

// cheap keeps the tests fast. The cost of Default is measured on the VM,
// not here.
var cheap = Params{Memory: 64, Time: 1, Threads: 1}

const good = "correct horse battery"

func TestValidate(t *testing.T) {
	for _, pw := range []string{
		strings.Repeat("a", MinLength),
		strings.Repeat("a", MaxLength),
		"twelve chars",         // spaces count
		"ünïcödé pässwörd",     // multi-byte characters count as their bytes
		strings.Repeat("é", 6), // 12 bytes
	} {
		if err := Validate(pw); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", pw, err)
		}
	}
	for _, pw := range []string{"", "short", strings.Repeat("a", MinLength-1), strings.Repeat("a", MaxLength+1)} {
		if err := Validate(pw); !errors.Is(err, ErrLength) {
			t.Errorf("Validate(%d bytes) = %v, want ErrLength", len(pw), err)
		}
	}
	if err := Validate("valid prefix \xff\xfe"); !errors.Is(err, ErrEncoding) {
		t.Errorf("Validate(invalid UTF-8) = %v, want ErrEncoding", err)
	}
	// No composition rules: all lowercase letters is fine.
	if err := Validate("aaaaaaaaaaaa"); err != nil {
		t.Errorf("composition rule applied: %v", err)
	}
}

var phcShape = regexp.MustCompile(`^\$argon2id\$v=19\$m=64,t=1,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`)

func TestHashShapeAndRoundTrip(t *testing.T) {
	h, err := Hash(good, cheap)
	if err != nil {
		t.Fatal(err)
	}
	if !phcShape.MatchString(h) {
		t.Fatalf("Hash = %q, not the PHC shape", h)
	}
	ok, err := Verify(good, h)
	if err != nil || !ok {
		t.Fatalf("Verify(right password) = %v, %v", ok, err)
	}
	for _, wrong := range []string{"correct horse batterY", good + " ", "", strings.TrimSuffix(good, "y")} {
		ok, err := Verify(wrong, h)
		if err != nil || ok {
			t.Errorf("Verify(%q) = %v, %v; want false, nil", wrong, ok, err)
		}
	}
}

// Two hashes of the same password differ: the salt is fresh every time.
func TestHashSaltIsRandom(t *testing.T) {
	a, err := Hash(good, cheap)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Hash(good, cheap)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two hashes of the same password are identical")
	}
	if strings.Split(a, "$")[4] == strings.Split(b, "$")[4] {
		t.Fatal("two hashes share a salt")
	}
}

// The key is really argon2id over the password with the stored salt and
// the stored parameters, in the right order. Swapping time and memory, or
// using argon2i, gives a different key.
func TestHashIsArgon2id(t *testing.T) {
	p := Params{Memory: 128, Time: 2, Threads: 2}
	h, err := Hash(good, p)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(h, "$")
	if parts[3] != "m=128,t=2,p=2" {
		t.Fatalf("parameters %q, want m=128,t=2,p=2", parts[3])
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		t.Fatal(err)
	}
	want := base64.RawStdEncoding.EncodeToString(argon2.IDKey([]byte(good), salt, 2, 128, 2, 32))
	if parts[5] != want {
		t.Fatalf("key %q, want argon2id(time=2, memory=128, threads=2) = %q", parts[5], want)
	}
}

// Verify uses the parameters in the hash, not Default.
func TestVerifyUsesStoredParams(t *testing.T) {
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte(good), salt, 2, 96, 3, 32)
	b := base64.RawStdEncoding
	phc := "$argon2id$v=19$m=96,t=2,p=3$" + b.EncodeToString(salt) + "$" + b.EncodeToString(key)
	ok, err := Verify(good, phc)
	if err != nil || !ok {
		t.Fatalf("Verify = %v, %v; want true", ok, err)
	}
}

func TestHashRefusesBadInput(t *testing.T) {
	if _, err := Hash("short", cheap); !errors.Is(err, ErrLength) {
		t.Errorf("Hash(short) = %v, want ErrLength", err)
	}
	for _, p := range []Params{
		{Memory: 64, Time: 0, Threads: 1},
		{Memory: 64, Time: 1, Threads: 0},
		{Memory: 64, Time: 11, Threads: 1},
		{Memory: 256*1024 + 1, Time: 1, Threads: 1},
		{Memory: 64, Time: 1, Threads: 17},
		{Memory: 7, Time: 1, Threads: 1}, // argon2 needs at least 8 KiB per lane
	} {
		if _, err := Hash(good, p); !errors.Is(err, ErrMalformed) {
			t.Errorf("Hash(params %+v) = %v, want ErrMalformed", p, err)
		}
	}
}

func TestVerifyRefusesMalformed(t *testing.T) {
	h, err := Hash(good, cheap)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(h, "$")
	salt, key := parts[4], parts[5]
	with := func(algo, ver, params, s, k string) string {
		return "$" + algo + "$" + ver + "$" + params + "$" + s + "$" + k
	}
	b := base64.RawStdEncoding
	bad := map[string]string{
		"empty":            "",
		"plain text":       good,
		"argon2i":          with("argon2i", "v=19", "m=64,t=1,p=1", salt, key),
		"argon2d":          with("argon2d", "v=19", "m=64,t=1,p=1", salt, key),
		"version 16":       with("argon2id", "v=16", "m=64,t=1,p=1", salt, key),
		"no version":       "$argon2id$m=64,t=1,p=1$" + salt + "$" + key,
		"params reordered": with("argon2id", "v=19", "t=1,m=64,p=1", salt, key),
		"param missing":    with("argon2id", "v=19", "m=64,t=1", salt, key),
		"extra param":      with("argon2id", "v=19", "m=64,t=1,p=1,x=1", salt, key),
		"leading zero":     with("argon2id", "v=19", "m=064,t=1,p=1", salt, key),
		"signed":           with("argon2id", "v=19", "m=+64,t=1,p=1", salt, key),
		"memory too big":   with("argon2id", "v=19", "m=262145,t=1,p=1", salt, key),
		"time too big":     with("argon2id", "v=19", "m=64,t=11,p=1", salt, key),
		"time zero":        with("argon2id", "v=19", "m=64,t=0,p=1", salt, key),
		"threads too big":  with("argon2id", "v=19", "m=1024,t=1,p=17", salt, key),
		"threads overflow": with("argon2id", "v=19", "m=64,t=1,p=257", salt, key),
		"short salt":       with("argon2id", "v=19", "m=64,t=1,p=1", b.EncodeToString([]byte("short")), key),
		"short key":        with("argon2id", "v=19", "m=64,t=1,p=1", salt, b.EncodeToString(make([]byte, 16))),
		"padded salt":      with("argon2id", "v=19", "m=64,t=1,p=1", salt+"==", key),
		"bad base64":       with("argon2id", "v=19", "m=64,t=1,p=1", "!!!!!!!!!!!!!!!!!!!!!!", key),
		"trailing part":    h + "$extra",
		"no leading $":     strings.TrimPrefix(h, "$"),
	}
	for name, phc := range bad {
		ok, err := Verify(good, phc)
		if ok || !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: Verify = %v, %v; want false, ErrMalformed", name, ok, err)
		}
	}
}

// Default is the approved parameter set (scope §4).
func TestDefault(t *testing.T) {
	if Default != (Params{Memory: 65536, Time: 3, Threads: 4}) {
		t.Fatalf("Default = %+v, want m=65536 t=3 p=4", Default)
	}
}
