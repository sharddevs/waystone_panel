// SPDX-License-Identifier: AGPL-3.0-only

package webui

import (
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sharddevs/waystone_panel/internal/web"
)

const index = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Waystone</title>
<script type="module" crossorigin src="/assets/index-abc123.js"></script>
<link rel="stylesheet" crossorigin href="/assets/index-abc123.css">
</head><body><div id="root"></div></body></html>
`

func build() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                          {Data: []byte(index)},
		"assets/index-abc123.js":              {Data: []byte("console.log(1)")},
		"assets/index-abc123.css":             {Data: []byte("body{}")},
		"assets/IBMPlexSans-Regular-x1.woff2": {Data: []byte("wOF2")},
		"assets/notes.map":                    {Data: []byte("{}")},
		"assets/sub/deep-1.js":                {Data: []byte("1")},
		"licenses/OFL.txt":                    {Data: []byte("SIL Open Font License")},
		"stray.exe":                           {Data: []byte("MZ")},
	}
}

// handler is web.sock as the controller builds it, with only the UI.
func handler(t *testing.T, files fs.FS) http.Handler {
	t.Helper()
	routes, err := Routes(files)
	if err != nil {
		t.Fatal(err)
	}
	o, err := web.ParsePublicURL("https://panel.example.com")
	if err != nil {
		t.Fatal(err)
	}
	h, err := web.New(web.Config{Public: o, ProxyUID: 4242, Log: log.New(io.Discard, "", 0)}, routes...)
	if err != nil {
		t.Fatal(err)
	}
	if err := web.RouteCoverage(h); err != nil {
		t.Fatal(err)
	}
	return h
}

func get(h http.Handler, method, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.Host = "panel.example.com"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "panel.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Hashed files: their type, cached for a year, with the CSP and the
// rest of web's headers still on.
func TestAssets(t *testing.T) {
	h := handler(t, build())
	for path, typ := range map[string]string{
		"/assets/index-abc123.js":              "text/javascript; charset=utf-8",
		"/assets/index-abc123.css":             "text/css; charset=utf-8",
		"/assets/IBMPlexSans-Regular-x1.woff2": "font/woff2",
		"/assets/sub/deep-1.js":                "text/javascript; charset=utf-8",
	} {
		w := get(h, http.MethodGet, path)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != typ {
			t.Errorf("%s: %d %q, want 200 %q", path, w.Code, w.Header().Get("Content-Type"), typ)
		}
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Errorf("%s: Cache-Control %q", path, got)
		}
		if w.Header().Get("Content-Security-Policy") != web.ContentSecurityPolicy || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: baseline headers missing: %v", path, w.Header())
		}
		if w.Header().Get("Last-Modified") != "" {
			t.Errorf("%s: Last-Modified %q", path, w.Header().Get("Last-Modified"))
		}
	}
	if w := get(h, http.MethodHead, "/assets/index-abc123.js"); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("HEAD: %d, %d bytes", w.Code, w.Body.Len())
	}
}

// A missing file, an unknown type or a directory under /assets/ is a
// JSON 404 that is not cached, never index.html.
func TestAssetsNotFound(t *testing.T) {
	h := handler(t, build())
	for _, path := range []string{"/assets/missing.js", "/assets/notes.map", "/assets/sub", "/assets/", "/assets/../stray.exe"} {
		w := get(h, http.MethodGet, path)
		if w.Code >= 300 && w.Code < 400 && !strings.Contains(w.Body.String(), "MZ") {
			continue // the mux cleans ".." with a redirect; no file is served
		}
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"not_found"`) {
			t.Errorf("%s: %d %s, want a JSON 404", path, w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control %q", path, w.Header().Get("Cache-Control"))
		}
	}
}

// A directory is never served as a file, even one with a served
// extension, on disk as in a devweb controller: the page is index.html.
func TestDirectoriesAreNotFiles(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"assets/sub.js", "odd.txt"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	h := handler(t, os.DirFS(dir))
	if w := get(h, http.MethodGet, "/assets/sub.js"); w.Code != http.StatusNotFound {
		t.Errorf("/assets/sub.js: %d %q, want 404", w.Code, w.Body.String())
	}
	if w := get(h, http.MethodGet, "/odd.txt"); w.Code != http.StatusOK || w.Body.String() != index {
		t.Errorf("/odd.txt: %d %q, want index.html", w.Code, w.Body.String())
	}
}

// Every other page is index.html, not cached, so a reload of a deep
// link works (W2); the build's other files are served as themselves.
func TestPages(t *testing.T) {
	h := handler(t, build())
	for _, path := range []string{"/", "/login", "/servers/0199e1a0-0000-7000-8000-000000000a01", "/operations/x?y=1", "/index.html", "/stray.exe", "/licenses"} {
		w := get(h, http.MethodGet, path)
		if w.Code != http.StatusOK || w.Body.String() != index || w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
			t.Errorf("%s: %d %q %q, want index.html", path, w.Code, w.Header().Get("Content-Type"), w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control %q, want no-store", path, w.Header().Get("Cache-Control"))
		}
	}
	w := get(h, http.MethodGet, "/licenses/OFL.txt")
	if w.Code != http.StatusOK || w.Body.String() != "SIL Open Font License" || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Errorf("licence: %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
}

// The API's unknown paths stay JSON 404s, and other methods aren't the
// UI's.
func TestAPIPathsAreNotPages(t *testing.T) {
	h := handler(t, build())
	for _, path := range []string{"/api", "/api/", "/api/v1/nope", "/api/v2/servers"} {
		w := get(h, http.MethodGet, path)
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"not_found"`) {
			t.Errorf("GET %s: %d %s, want a JSON 404", path, w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/servers", nil)
	r.Host = "panel.example.com"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "panel.example.com")
	r.Header.Set("Origin", "https://panel.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "<html") {
		t.Errorf("POST /servers: %d %s, want no page", w.Code, w.Body.String())
	}
}

// Without a UI (no -tags ui): every page is the "UI not built" page, a
// 503 the CSP allows as is, and the API paths are still JSON.
func TestNotBuilt(t *testing.T) {
	h := handler(t, nil)
	for _, path := range []string{"/", "/servers/x"} {
		w := get(h, http.MethodGet, path)
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "web UI isn't built") || w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
			t.Errorf("%s: %d %q", path, w.Code, w.Body.String())
		}
		if err := CheckIndex(w.Body.Bytes()); err != nil {
			t.Errorf("the not-built page: %v", err)
		}
	}
	if w := get(h, http.MethodGet, "/assets/index-abc123.js"); w.Code != http.StatusNotFound {
		t.Errorf("asset without a UI: %d", w.Code)
	}
	if w := get(h, http.MethodGet, "/api/v1/nope"); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), `"not_found"`) {
		t.Errorf("API path without a UI: %d %s", w.Code, w.Body.String())
	}
}

// A build that is there but unusable is refused, so the controller
// won't start with it.
func TestRoutesRefuseABadBuild(t *testing.T) {
	if _, err := Routes(fstest.MapFS{"assets/a.js": {Data: []byte("1")}}); err == nil {
		t.Error("no index.html: accepted")
	}
	if _, err := Routes(fstest.MapFS{"index.html": {Data: []byte("<script>alert(1)</script>")}}); err == nil {
		t.Error("inline script: accepted")
	}
}

func TestCheckIndex(t *testing.T) {
	if err := CheckIndex([]byte(index)); err != nil {
		t.Errorf("Vite's index.html: %v", err)
	}
	for _, bad := range []string{
		`<script>alert(1)</script>`,
		`<SCRIPT type="module">x</SCRIPT>`,
		`<script src="/a.js"></script><script>inline()</script>`,
		`<script src="/a.js"`,
		`<style>body{}</style>`,
		`<div style="color:red"></div>`,
		`<script src="https://cdn.example/x.js"></script>`,
		`<link rel="stylesheet" href="http://fonts.example/a.css">`,
		`<link rel="stylesheet" href="//fonts.example/a.css">`,
	} {
		if CheckIndex([]byte(bad)) == nil {
			t.Errorf("CheckIndex(%q) accepted", bad)
		}
	}
}

// In a release build -dev-ui-dir is refused.
func TestDevDirIsDevOnly(t *testing.T) {
	if _, err := DevDir(t.TempDir()); err == nil && !devBuild {
		t.Error("DevDir accepted in a release build")
	}
}
