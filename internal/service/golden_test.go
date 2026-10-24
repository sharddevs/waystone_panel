// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"bytes"
	"context"
	"flag"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/*.json")

// The error body both sockets send, with every field set. The web
// client's contract test decodes this file (Q9).
func TestGoldenError(t *testing.T) {
	s := New(Config{Log: discardLogger()})
	r := httptest.NewRequest("POST", "/", nil)
	r = r.WithContext(context.WithValue(context.Background(), requestIDKey{}, "0199e1a0-0000-7000-8000-00000000e001"))
	w := httptest.NewRecorder()
	s.writeError(w, r, &Error{code: CodePreviousOperationUncertain, operationID: "0199e1a0-0000-7000-8000-00000000d001"})
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
		t.Fatalf("%v (run go test ./internal/service -run Golden -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s changed; if that's intended, run with -update and update the web client:\n%s", path, got)
	}
}
