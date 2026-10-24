// SPDX-License-Identifier: AGPL-3.0-only

//go:build devweb

package webui

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// The dev loop (Q13): a rebuild on disk shows on the next request,
// without restarting the controller.
func TestDevDirIsLive(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", `<script type="module" src="/assets/a-1.js"></script>`)
	write("assets/a-1.js", "1")
	files, err := DevDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := Routes(files)
	if err != nil {
		t.Fatal(err)
	}
	h := routes[1].Handler
	w := get(h, http.MethodGet, "/")
	if w.Body.String() != `<script type="module" src="/assets/a-1.js"></script>` {
		t.Fatalf("first: %q", w.Body.String())
	}
	write("index.html", `<script type="module" src="/assets/a-2.js"></script>`)
	if w := get(h, http.MethodGet, "/"); w.Body.String() != `<script type="module" src="/assets/a-2.js"></script>` {
		t.Errorf("after the rebuild: %q", w.Body.String())
	}
}
