// SPDX-License-Identifier: AGPL-3.0-only

package console

import (
	"strings"
	"testing"
)

func TestValidLine(t *testing.T) {
	for _, line := range []string{
		"say hi", "whitelist add Steve", "stop", "a", "say é 🎉", "say a/b",
		strings.Repeat("a", MaxLineBytes),
		"say " + strings.Repeat("é", (MaxLineBytes-4)/2),
	} {
		if !ValidLine(line) {
			t.Errorf("%q refused", line)
		}
	}
	for _, line := range []string{
		"", "/stop", "say hi\n", "say\nstop", "say\rhi", "say\thi", "say\x00", "say\x1b[2J", "say\x7f",
		"say\u0085", "say\u009b", "say\u202eboo", "say\u2066", "say\u200f", "say\u061c",
		"say \xff", strings.Repeat("a", MaxLineBytes+1),
		"say " + strings.Repeat("é", (MaxLineBytes-4)/2+1),
	} {
		if ValidLine(line) {
			t.Errorf("%q accepted", line)
		}
	}
}
