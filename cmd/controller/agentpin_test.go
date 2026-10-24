// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sharddevs/waystone_panel/internal/agenttls"
)

func TestAgentPin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent-tls")
	var out1, out2, errs bytes.Buffer
	if code := runAgentPin([]string{"-agent-tls-dir", dir}, 995, &out1, &errs); code != 0 {
		t.Fatalf("first run: %d %s", code, errs.String())
	}
	if code := runAgentPin([]string{"-agent-tls-dir", dir}, 995, &out2, &errs); code != 0 {
		t.Fatalf("second run: %d %s", code, errs.String())
	}
	pin := strings.TrimSpace(out1.String())
	if _, err := agenttls.ParsePin(pin); err != nil || out2.String() != out1.String() {
		t.Fatalf("pins %q and %q (%v)", out1.String(), out2.String(), err)
	}
	if code := runAgentPin([]string{"extra"}, 995, &out1, &errs); code != 2 {
		t.Fatalf("extra argument: %d", code)
	}
	// As root it makes nothing (6a-I1).
	rootDir := filepath.Join(t.TempDir(), "agent-tls")
	if code := runAgentPin([]string{"-agent-tls-dir", rootDir}, 0, &out1, &errs); code != 2 {
		t.Fatalf("as root: %d", code)
	}
	if _, err := os.Stat(rootDir); !os.IsNotExist(err) {
		t.Fatalf("made %s as root", rootDir)
	}
}
