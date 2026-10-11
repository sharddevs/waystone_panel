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

// Check returns nil if s is a UUID in canonical form (36 bytes, lowercase
// hex, dashes at 8, 13, 18 and 23), and ErrInvalid otherwise.
//
// This is a security check. Server IDs end up in unit names that root runs,
// so being strict here is what keeps things like "../" or "sshd.service"
// out. A plain byte loop instead of a regexp is easier to audit.
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

// NewV7 returns a new time-ordered UUID (version 7, RFC 9562) in the
// form Check accepts.
func NewV7() (string, error) {
	var b [16]byte

	// crypto/rand, not math/rand, so the random part can't be predicted.
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	ms := uint64(time.Now().UnixMilli())

	// The first 6 bytes are the Unix time in milliseconds, big-endian.
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
