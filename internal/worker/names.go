// SPDX-License-Identifier: AGPL-3.0-only

package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ValidName returns nil if name is a file name the worker may touch, or an
// error wrapping ErrInvalid.
//   - 1 to MaxNameBytes bytes.
//   - Only ASCII letters, digits, '.', '_' and '-'.
//   - The first byte is a letter or a digit. That rules out "." and "..",
//     names that look like options ("-x"), and the worker's own temporary
//     files, which start with '.'.
//
// No '/' can pass, so a name never leaves the server directory or reaches
// into a subdirectory.
func ValidName(name string) error {
	if len(name) == 0 || len(name) > MaxNameBytes {
		return fmt.Errorf("%w: invalid filename length", ErrInvalid)
	}

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
// either that it does not exist (Absent), or that its current content has
// exactly this SHA-256. The zero value matches nothing that can be read
// back, so a forgotten Expect fails safe.
type Expect struct {
	Absent bool
	SHA256 [sha256.Size]byte
}

// ExpectAbsent is the Expect for "the file must not exist".
func ExpectAbsent() Expect {
	return Expect{Absent: true}
}

// ExpectContent is the Expect for "the file must still contain content".
func ExpectContent(content []byte) Expect {
	return Expect{SHA256: sha256.Sum256(content)}
}

// String is the form waystone-worker takes on its command line: "absent",
// or the SHA-256 as 64 lowercase hex digits.
func (e Expect) String() string {
	if e.Absent {
		return "absent"
	}

	return hex.EncodeToString(e.SHA256[:])
}

// ParseExpect is the reverse of String. Anything else, including uppercase
// hex or a hash of the wrong length, is an error wrapping ErrInvalid.
func ParseExpect(s string) (Expect, error) {
	if s == "absent" {
		return ExpectAbsent(), nil
	}

	if len(s) != sha256.Size*2 {
		return Expect{}, fmt.Errorf("%w: invalid hash length", ErrInvalid)
	}

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
