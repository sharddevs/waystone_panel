// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, 0, strings.NewReader(""), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "root") {
		t.Errorf("as root: %d %q", code, errOut.String())
	}
	if code := run([]string{"x"}, 1000, strings.NewReader(""), io.Discard, io.Discard); code != 2 {
		t.Errorf("an argument: %d", code)
	}
	out.Reset()
	// Not a pinned hash: refused, and nothing is fetched.
	if code := run([]string{"-dir", t.TempDir()}, 1000, strings.NewReader("nope\n"), &out, io.Discard); code != 2 || out.String() != "refused\n" {
		t.Errorf("unknown: %d %q", code, out.String())
	}
}
