// SPDX-License-Identifier: AGPL-3.0-only

package serverlog

import (
	"strings"
	"testing"
)

func TestValidCursor(t *testing.T) {
	const good = "s=0123456789abcdef0123456789abcdef;i=1a2;b=fedcba9876543210fedcba9876543210;m=9f;t=5f1e2d3c4b5a6;x=77aa"
	for _, c := range []string{
		good,
		"s=0123456789abcdef0123456789abcdef;i=0;b=fedcba9876543210fedcba9876543210;m=0;t=0;x=0",
		"s=0123456789abcdef0123456789abcdef;i=ffffffffffffffff;b=fedcba9876543210fedcba9876543210;m=ffffffffffffffff;t=ffffffffffffffff;x=ffffffffffffffff",
	} {
		if !ValidCursor(c) {
			t.Errorf("%q refused", c)
		}
	}
	bad := []string{
		"", "s=0", "--file=/etc/shadow", "--since=today",
		good + "\n", good + ";y=1", "x" + good, strings.ToUpper(good),
		strings.Replace(good, "i=1a2", "i=", 1),
		strings.Replace(good, "i=1a2", "i=11111111111111111", 1),
		strings.Replace(good, "s=0123456789abcdef0123456789abcdef", "s=0123", 1),
		strings.Replace(good, ";b=", ";c=", 1),
		strings.Replace(good, "b=fedcba9876543210fedcba9876543210", "b=FEDCBA9876543210FEDCBA9876543210", 1),
		strings.Replace(good, ";", " ;", 1),
		strings.Replace(good, "x=77aa", "x=77aa\x00", 1),
		strings.Replace(good, "m=9f;t=", "t=9f;m=", 1),
	}
	for _, c := range bad {
		if ValidCursor(c) {
			t.Errorf("%q accepted", c)
		}
	}
}

func TestClean(t *testing.T) {
	cases := map[string]string{
		"plain":                       "plain",
		"tab\there":                   "tab\there",
		"\x1b[31mred\x1b[0m":          "\uFFFD[31mred\uFFFD[0m",
		"cr\rlf\n":                    "cr\uFFFDlf\uFFFD",
		"nul\x00":                     "nul\uFFFD",
		"del\x7f":                     "del\uFFFD",
		"c1\u0085\u009b":              "c1\uFFFD\uFFFD",
		"bad\xffutf8":                 "bad\uFFFDutf8",
		"rlo\u202egnp.exe":            "rlo\uFFFDgnp.exe",
		"lre\u202a pdf\u202c":         "lre\uFFFD pdf\uFFFD",
		"iso\u2066\u2067\u2068\u2069": "iso\uFFFD\uFFFD\uFFFD\uFFFD",
		"marks\u200e\u200f\u061c":     "marks\uFFFD\uFFFD\uFFFD",
		"emoji 🎉 and é":               "emoji 🎉 and é",
		"already \uFFFD":              "already \uFFFD",
		"zwj\u200d":                   "zwj\u200d",
	}
	for in, want := range cases {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
		if !CleanText(Clean(in)) || Clean(Clean(in)) != Clean(in) {
			t.Errorf("Clean(%q) isn't stable", in)
		}
	}
	for in := range cases {
		if in != Clean(in) && CleanText(in) {
			t.Errorf("CleanText(%q) is true, but Clean changes it", in)
		}
	}
}

func TestCleanCuts(t *testing.T) {
	if got := Clean(strings.Repeat("a", MaxLineBytes+10)); len(got) != MaxLineBytes {
		t.Errorf("%d bytes, want %d", len(got), MaxLineBytes)
	}
	// Never in the middle of a character.
	got := Clean("a" + strings.Repeat("é", MaxLineBytes))
	if len(got) != MaxLineBytes-1 || !CleanText(got) {
		t.Errorf("%d bytes, want %d on a boundary", len(got), MaxLineBytes-1)
	}
	if CleanText(strings.Repeat("a", MaxLineBytes+1)) {
		t.Error("CleanText takes a line over the limit")
	}
	if !CleanText(strings.Repeat("a", MaxLineBytes)) || !CleanText("") {
		t.Error("CleanText refuses a line at the limit, or an empty one")
	}
}
