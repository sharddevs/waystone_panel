// Package ids checks and creates the UUIDs Waystone uses as identifiers.
package ids

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// ErrInvalid is returned by Check for anything that is not a canonical UUID.
var ErrInvalid = errors.New("not a canonical lowercase UUID")

// Check returns nil if s is a UUID in canonical form, and ErrInvalid otherwise.
//
// Canonical form is exactly 36 bytes: lowercase hex digits (0-9, a-f), with
// '-' at positions 8, 13, 18 and 23 (counting from 0). Nothing else:
// no uppercase, braces, "urn:uuid:" prefix, spaces or newlines.
//
// This check is a security boundary. Server IDs end up inside unit names
// run by root, so being strict here is what keeps "../" or "sshd.service"
// out. Check bytes, not runes, and do not use a regexp: a short loop is
// easier to audit.
func Check(s string) error {
	if len(s) != 36 {
		return ErrInvalid
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return ErrInvalid
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return ErrInvalid
		}
	}
	return nil
}

// NewV7 returns a new random, time-ordered UUID (version 7, RFC 9562),
// in the canonical form Check accepts.
//
// Layout of the 16 bytes:
//   - bytes 0-5: Unix time in milliseconds, big-endian (48 bits)
//   - byte 6: high 4 bits are the version, 0111 (7); low 4 bits random
//   - byte 7: random
//   - byte 8: high 2 bits are the variant, 10; low 6 bits random
//   - bytes 9-15: random
//
// Use crypto/rand for the random bits, never math/rand. Then format as
// 8-4-4-4-12 lowercase hex.
func NewV7() (string, error) {
	var b [16]byte

	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	ms := uint64(time.Now().UnixMilli())

	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

	var hexBytes [32]byte
	hex.Encode(hexBytes[:], b[:])

	id := string(hexBytes[:])

	return id[:8] + "-" +
		id[8:12] + "-" +
		id[12:16] + "-" +
		id[16:20] + "-" +
		id[20:], nil
}
