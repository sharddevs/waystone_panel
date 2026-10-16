// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"strings"
	"testing"
)

// Resolution notes (5b audit H2): newlines are fine, other control
// characters and bidi formatting characters aren't.
func TestValidNote(t *testing.T) {
	for _, note := range []string{
		"",
		"checked by hand: server was stopped",
		"line one\nline two",
		"mañana, 日本語 ✓",
		strings.Repeat("n", 500),
	} {
		if !validNote(note) {
			t.Errorf("validNote(%q) = false, want true", note)
		}
	}
	for _, note := range []string{
		strings.Repeat("n", 501),
		"\xff",
		"tab\there",
		"carriage\rreturn",
		"nul\x00",
		"esc\x1b[2J",
		"del\x7f",
		"c1\u0085",
		"؜arabic mark",
		"‎left mark",
		"‏right mark",
		"‪embed", "‫embed", "‬pop", "‭override", "‮override",
		"⁦isolate", "⁧isolate", "⁨isolate", "⁩pop",
	} {
		if validNote(note) {
			t.Errorf("validNote(%q) = true, want false", note)
		}
	}
}
