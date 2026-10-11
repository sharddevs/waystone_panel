// SPDX-License-Identifier: AGPL-3.0-only

package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ValidName returns nil if name is a plain file name the worker may touch,
// otherwise an error wrapping ErrInvalid. Only ASCII letters, digits, '.',
// '_' and '-' are allowed, so a '/' can never get through and a name can't
// leave the server directory.
func ValidName(name string) error {
	if len(name) == 0 || len(name) > MaxNameBytes {
		return fmt.Errorf("%w: invalid filename length", ErrInvalid)
	}

	// The first byte has to be a letter or digit. That rules out "." and
	// "..", names that look like flags ("-x"), and our own temp files,
	// which start with '.'.
	for i := 0; i < len(name); i++ {
		c := name[i]

		letter := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z')

		digit := c >= '0' && c <= '9'

		if !letter && !digit {
			if i == 0 || (c != '.' && c != '_' && c != '-') {
				return fmt.Errorf("%w: invalid filename character", ErrInvalid)
			}
		}
	}

	return nil
}

// Expect is what a conditional write requires of the file it replaces:
// either it doesn't exist (Absent) or its content has this SHA-256.
// The zero value matches no real file, so forgetting to set it fails safe.
type Expect struct {
	Absent bool
	SHA256 [sha256.Size]byte
}

// ExpectAbsent returns an Expect for a file that must not exist yet.
func ExpectAbsent() Expect {
	return Expect{Absent: true}
}

// ExpectContent returns an Expect for a file that must still hold content.
func ExpectContent(content []byte) Expect {
	return Expect{SHA256: sha256.Sum256(content)}
}

// String returns the form waystone-worker takes on its command line:
// "absent", or the SHA-256 as 64 lowercase hex digits.
func (e Expect) String() string {
	if e.Absent {
		return "absent"
	}

	return hex.EncodeToString(e.SHA256[:])
}

// ParseExpect is the reverse of String. Anything else, including uppercase
// hex, is an error wrapping ErrInvalid.
func ParseExpect(s string) (Expect, error) {
	if s == "absent" {
		return ExpectAbsent(), nil
	}

	if len(s) != sha256.Size*2 {
		return Expect{}, fmt.Errorf("%w: invalid hash length", ErrInvalid)
	}

	// hex.DecodeString accepts uppercase too, so we check by hand and only
	// take the lowercase form that String produces.
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return Expect{}, fmt.Errorf("%w: invalid hash character", ErrInvalid)
		}
	}

	decoded, err := hex.DecodeString(s)
	if err != nil {
		return Expect{}, fmt.Errorf("%w: invalid hash: %v", ErrInvalid, err)
	}

	var expected Expect
	copy(expected.SHA256[:], decoded)

	return expected, nil
}
