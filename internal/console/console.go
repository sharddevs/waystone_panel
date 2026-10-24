// SPDX-License-Identifier: AGPL-3.0-only

// Package console holds the rules for a console command line (stage 7b,
// notes/stage7-scope.md D2). The API, the store, the agent link and the
// agent all check a line with ValidLine, and the database has the same
// CHECK, so no one layer is trusted to have done it.
package console

import (
	"unicode"
	"unicode/utf8"
)

// MaxLineBytes is the longest line. With its newline it stays well under
// PIPE_BUF, so the agent's one write to the FIFO is atomic.
const MaxLineBytes = 256

// ValidLine reports whether line may be sent to a game's console: 1 to
// MaxLineBytes bytes of valid UTF-8, with no control character (newline
// and tab included), no bidirectional formatting character, and no
// leading "/". Every admin reads the line in the operation history, so it
// mustn't be able to hide or reorder what it shows, and a newline would
// make it two commands.
func ValidLine(line string) bool {
	if len(line) < 1 || len(line) > MaxLineBytes || !utf8.ValidString(line) || line[0] == '/' {
		return false
	}
	for _, r := range line {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return false
		}
	}
	return true
}
