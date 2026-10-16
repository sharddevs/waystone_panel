// SPDX-License-Identifier: AGPL-3.0-only

// Package password hashes and checks browser account passwords with
// argon2id (stage 5a-1, notes/stage5-scope.md §4). A hash is stored as a
// PHC string, which carries its own parameters, so they can change later
// without breaking existing accounts.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Length limits in bytes (NIST 800-63B: a minimum length, a generous
// maximum, no composition rules).
const (
	MinLength = 12
	MaxLength = 256
)

// Params are argon2id's cost settings.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32 // passes
	Threads uint8
}

// Default is RFC 9106's second recommended set: 64 MiB, 3 passes, 4 lanes.
var Default = Params{Memory: 64 * 1024, Time: 3, Threads: 4}

// The bounds a stored hash's parameters must fall in. A hash with bigger
// numbers is refused instead of run, so a tampered row can't make one
// login cost gigabytes.
const (
	maxMemory  = 256 * 1024
	maxTime    = 10
	maxThreads = 16
)

const (
	saltLen = 16
	keyLen  = 32
)

var (
	// ErrLength: the password is shorter than MinLength or longer than
	// MaxLength bytes.
	ErrLength = fmt.Errorf("password must be %d to %d bytes", MinLength, MaxLength)
	// ErrEncoding: the password isn't valid UTF-8.
	ErrEncoding = errors.New("password must be valid UTF-8")
	// ErrMalformed: a stored hash isn't a PHC string this package wrote,
	// or its parameters are out of bounds.
	ErrMalformed = errors.New("malformed password hash")
)

// Validate checks a new password: MinLength to MaxLength bytes of valid
// UTF-8. It says nothing about letters, digits or symbols on purpose.
func Validate(pw string) error {
	if len(pw) < MinLength || len(pw) > MaxLength {
		return ErrLength
	}
	if !utf8.ValidString(pw) {
		return ErrEncoding
	}
	return nil
}

// Hash validates pw, then hashes it with p and a fresh 16-byte random
// salt, and returns the PHC string
//
//	$argon2id$v=19$m=<memory>,t=<time>,p=<threads>$<salt>$<key>
//
// with salt and key in unpadded standard base64 (RFC 4648 §4 without '=').
// The key is 32 bytes.
func Hash(pw string, p Params) (string, error) {
	if err := Validate(pw); err != nil {
		return "", err
	}
	if err := checkParams(p); err != nil {
		return "", err
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password salt: %w", err)
	}
	return encode(p, salt, derive(pw, salt, p)), nil
}

// Verify reports whether pw matches the PHC string phc. A malformed phc
// is ErrMalformed, never a match. The comparison takes the same time
// wherever the keys differ.
func Verify(pw, phc string) (bool, error) {
	p, salt, key, err := decode(phc)
	if err != nil {
		return false, err
	}
	got := derive(pw, salt, p)
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

// derive is the one call into argon2.
func derive(pw string, salt []byte, p Params) []byte {
	return argon2.IDKey([]byte(pw), salt, p.Time, p.Memory, p.Threads, keyLen)
}

func checkParams(p Params) error {
	if p.Memory < 8*uint32(p.Threads) || p.Memory > maxMemory ||
		p.Time < 1 || p.Time > maxTime ||
		p.Threads < 1 || p.Threads > maxThreads {
		return ErrMalformed
	}
	return nil
}

var b64 = base64.RawStdEncoding

func encode(p Params, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// decode parses exactly what encode writes, and nothing looser: the
// algorithm must be argon2id, the version 19, the parameters in the order
// m, t, p as plain decimals within the bounds, the salt 16 bytes and the
// key 32.
func decode(phc string) (Params, []byte, []byte, error) {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" ||
		parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return Params{}, nil, nil, ErrMalformed
	}
	kv := strings.Split(parts[3], ",")
	if len(kv) != 3 {
		return Params{}, nil, nil, ErrMalformed
	}
	var vals [3]uint64
	for i, name := range []string{"m=", "t=", "p="} {
		v, ok := strings.CutPrefix(kv[i], name)
		if !ok || v == "" || (len(v) > 1 && v[0] == '0') {
			return Params{}, nil, nil, ErrMalformed
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return Params{}, nil, nil, ErrMalformed
		}
		vals[i] = n
	}
	if vals[2] > maxThreads {
		return Params{}, nil, nil, ErrMalformed
	}
	p := Params{Memory: uint32(vals[0]), Time: uint32(vals[1]), Threads: uint8(vals[2])}
	if err := checkParams(p); err != nil {
		return Params{}, nil, nil, err
	}
	salt, err := b64.Strict().DecodeString(parts[4])
	if err != nil || len(salt) != saltLen {
		return Params{}, nil, nil, ErrMalformed
	}
	key, err := b64.Strict().DecodeString(parts[5])
	if err != nil || len(key) != keyLen {
		return Params{}, nil, nil, ErrMalformed
	}
	return p, salt, key, nil
}
