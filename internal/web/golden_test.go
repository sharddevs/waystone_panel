// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"bytes"
	"flag"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/*.json")

// web's own error body (no request ID). The web client's contract test
// decodes this file (Q9).
func TestGoldenError(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, CodeInvalidCredentials)
	got := w.Body.Bytes()
	path := filepath.Join("testdata", "error.json")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/web -run Golden -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s changed; if that's intended, run with -update and update the web client:\n%s", path, got)
	}
}
