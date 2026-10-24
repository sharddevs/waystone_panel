// SPDX-License-Identifier: AGPL-3.0-only

// Package serverlog is what the helper, the agent and the controller
// agree on about a game server's log (stage 7a, notes/stage7-scope.md D1):
// the limits, the journald cursor's syntax, and how a line is cleaned
// before anyone shows it. Each of the three applies these rules itself;
// none trusts the one before it to have done so.
//
// Log lines are only ever passed through. Nothing here, or anywhere in
// Waystone, stores them.
package serverlog

import (
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxLines is the most lines one read may ask for, and DefaultLines
	// what the API reads without ?limit=.
	MaxLines     = 200
	DefaultLines = 200
	// MaxLineBytes caps one line's text, after cleaning.
	MaxLineBytes = 4 << 10
	// MaxTextBytes caps the text of all the lines of one answer, so it
	// fits one 64 KiB message on both the helper's socket and the agent
	// link, with room for the rest of the message.
	MaxTextBytes = 48 << 10
	// MaxCursorBytes caps a cursor.
	MaxCursorBytes = 256
)

// Line is one log line.
type Line struct {
	Time time.Time
	// Priority is syslog's 0 (emerg) to 7 (debug). A game's own output
	// is 6 (info) unless it says otherwise.
	Priority uint32
	Text     string
}

// Page is one read: lines oldest first, the cursor of the newest one
// ("" when there are none), and whether more lines follow the cursor
// already.
type Page struct {
	Lines  []Line
	Cursor string
	More   bool
}

// Why a read failed. The controller turns each into its own API error.
var (
	// ErrUnreachable: the server's node has no live session.
	ErrUnreachable = errors.New("serverlog: node not connected")
	// ErrBusy: the session already has MaxReads reads out.
	ErrBusy = errors.New("serverlog: too many reads in flight")
	// ErrUnavailable: the node answered that it couldn't read the log,
	// or didn't answer in time.
	ErrUnavailable = errors.New("serverlog: log unavailable")
)

// MaxReads is how many reads one agent session may have out at once.
const MaxReads = 4

// cursorPattern is journald's cursor, field by field, as sd-journal has
// always written it (audit 7M2).
var cursorPattern = regexp.MustCompile(`^s=[0-9a-f]{32};i=[0-9a-f]{1,16};b=[0-9a-f]{32};m=[0-9a-f]{1,16};t=[0-9a-f]{1,16};x=[0-9a-f]{1,16}$`)

// ValidCursor reports whether c is a journald cursor of at most
// MaxCursorBytes. Only its shape is checked; journald itself refuses one
// it doesn't know. The helper passes it in --after-cursor=<c> form, as
// one argv element, never through a shell.
func ValidCursor(c string) bool {
	return len(c) <= MaxCursorBytes && cursorPattern.MatchString(c)
}

// Clean makes s safe to show as plain text: invalid UTF-8, control
// characters other than tab (escape sequences included), and the
// characters that reorder text (bidi controls) each become U+FFFD. The
// result is cut to MaxLineBytes on a character boundary. Clean(Clean(s))
// == Clean(s).
func Clean(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		if r == utf8.RuneError || unsafeRune(r) {
			r = utf8.RuneError
		}
		if b.Len()+utf8.RuneLen(r) > MaxLineBytes {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// CleanText reports whether s is already what Clean would make of it.
func CleanText(s string) bool {
	if len(s) > MaxLineBytes || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unsafeRune(r) {
			return false
		}
	}
	return true
}

func unsafeRune(r rune) bool {
	switch {
	case r == '\t':
		return false
	case r < 0x20, r >= 0x7f && r <= 0x9f:
		return true
	// Bidi embeddings, overrides and isolates, and the marks.
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0x200e, r == 0x200f, r == 0x061c:
		return true
	}
	return false
}
