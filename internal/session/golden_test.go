// SPDX-License-Identifier: AGPL-3.0-only

package session

import (
	"bytes"
	"flag"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sharddevs/waystone_panel/internal/web"
)

var update = flag.Bool("update", false, "rewrite testdata/*.json")

// GET /api/v1/session's body, as current writes it. The web client's
// contract test decodes this file (Q9).
func TestGoldenSession(t *testing.T) {
	var body sessionJSON
	body.User.ID = "0199e1a0-0000-7000-8000-00000000c001"
	body.User.DisplayName = "Trevor"
	body.CSRFToken = "c3JmdG9rZW5jc3JmdG9rZW5jc3JmdG9rZW5jc3JmdG9r"[:web.TokenLength]
	body.ExpiresAt = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	body.Preferences = preferencesJSON{Theme: "slate", Density: "compact", Mode: "system"}
	w := httptest.NewRecorder()
	web.WriteJSON(w, 200, body)
	got := w.Body.Bytes()
	path := filepath.Join("testdata", "session.json")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/session -run Golden -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s changed; if that's intended, run with -update and update the web client:\n%s", path, got)
	}
}
