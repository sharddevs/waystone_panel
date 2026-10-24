// SPDX-License-Identifier: AGPL-3.0-only

// Package webui serves the browser UI on web.sock (scope §6): the hashed
// files under /assets/, the other files of the build (the font licence),
// and index.html for every other GET outside /api/, so /servers/<id> and
// /operations/<id> survive a reload (W2). Everything goes through
// web.New, so the guard, the CSP and the other baseline headers apply.
package webui

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/sharddevs/waystone_panel/internal/web"
)

// immutable is the cache policy for hashed files: their name changes
// whenever their content does. Everything else keeps web's no-store.
const immutable = "public, max-age=31536000, immutable"

// types are the only files served, by extension. Anything else in the
// build is a 404, so a stray file can't be served with a guessed type.
var types = map[string]string{
	".js":    "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".woff2": "font/woff2",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".txt":   "text/plain; charset=utf-8",
	".html":  "text/html; charset=utf-8",
}

// Routes are the UI's routes for web.New. files is the build (Embedded
// or DevDir); nil means this binary has no UI, and every page is the "UI
// not built" page. A build without a usable index.html is an error, so a
// release controller refuses to start without the real UI.
func Routes(files fs.FS) ([]web.Route, error) {
	if files == nil {
		return []web.Route{
			{Method: http.MethodGet, Pattern: "/assets/{file...}", CSRF: web.CSRFNotApplicable, Handler: http.HandlerFunc(notFound)},
			{Method: http.MethodGet, Pattern: "/{path...}", CSRF: web.CSRFNotApplicable, Handler: http.HandlerFunc(notBuilt)},
		}, nil
	}
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		return nil, fmt.Errorf("web UI: no index.html in the build: %w", err)
	}
	if err := CheckIndex(index); err != nil {
		return nil, fmt.Errorf("web UI: index.html: %w", err)
	}
	return []web.Route{
		{Method: http.MethodGet, Pattern: "/assets/{file...}", CSRF: web.CSRFNotApplicable, Handler: assets(files)},
		{Method: http.MethodGet, Pattern: "/{path...}", CSRF: web.CSRFNotApplicable, Handler: pages(files)},
	}, nil
}

// CheckIndex refuses an index.html the CSP would break or that would
// load anything from outside: every <script> needs a same-origin src,
// and there may be no <style> element, no style attribute, and no
// absolute URL (WS scope §4a, design tokens: nothing fetched from
// outside).
func CheckIndex(b []byte) error {
	lower := bytes.ToLower(b)
	for rest := lower; ; {
		i := bytes.Index(rest, []byte("<script"))
		if i < 0 {
			break
		}
		end := bytes.IndexByte(rest[i:], '>')
		if end < 0 {
			return errors.New("an unterminated <script> tag")
		}
		if !bytes.Contains(rest[i:i+end], []byte(" src=")) {
			return errors.New("an inline <script>")
		}
		rest = rest[i+end:]
	}
	switch {
	case bytes.Contains(lower, []byte("<style")):
		return errors.New("a <style> element")
	case bytes.Contains(lower, []byte(" style=")):
		return errors.New("a style attribute")
	case bytes.Contains(lower, []byte("http://")), bytes.Contains(lower, []byte("https://")), bytes.Contains(lower, []byte("=\"//")), bytes.Contains(lower, []byte("='//")):
		return errors.New("an absolute URL")
	}
	return nil
}

// assets serves /assets/<file>: hashed, so cached for a year.
func assets(files fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("file")
		if !serveFile(w, r, files, "assets/"+name, immutable) {
			notFound(w, r)
		}
	})
}

// pages serves the build's other files at their path (no-store), and
// index.html for everything else. /api/... is never the UI: an unknown
// API path stays a JSON 404.
func pages(files fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/api" || strings.HasPrefix(p, "/api/") {
			notFound(w, r)
			return
		}
		name := strings.TrimPrefix(p, "/")
		if name != "" && name != "index.html" && !strings.HasPrefix(name, "assets/") && serveFile(w, r, files, name, "") {
			return
		}
		if !serveFile(w, r, files, "index.html", "") {
			// The build changed under a devweb controller; say so plainly.
			notBuilt(w, r)
		}
	})
}

// serveFile serves name from files if it is a regular file with a known
// type, and reports whether it did. cache, if set, replaces web's
// no-store.
func serveFile(w http.ResponseWriter, r *http.Request, files fs.FS, name, cache string) bool {
	if !fs.ValidPath(name) {
		return false
	}
	typ, ok := types[path.Ext(name)]
	if !ok {
		return false
	}
	f, err := files.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		b, err := io.ReadAll(f)
		if err != nil {
			return false
		}
		rs = bytes.NewReader(b)
	}
	w.Header().Set("Content-Type", typ)
	if cache != "" {
		w.Header().Set("Cache-Control", cache)
	}
	// A zero time: no Last-Modified (the embedded files have none).
	http.ServeContent(w, r, "", time.Time{}, rs)
	return true
}

func notFound(w http.ResponseWriter, _ *http.Request) { web.WriteError(w, web.CodeNotFound) }

// notBuiltPage has no script and no style, so the CSP allows it as is.
const notBuiltPage = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Waystone: web UI not built</title></head>
<body>
<h1>Waystone's web UI isn't built into this controller</h1>
<p>The API on this address works. To get the UI, build the controller with it:
<code>cd web &amp;&amp; npm ci &amp;&amp; npm run build</code>, then <code>go build -tags ui ./cmd/controller</code>.
See deploy/README.md.</p>
</body>
</html>
`

func notBuilt(w http.ResponseWriter, r *http.Request) {
	if p := r.URL.Path; p == "/api" || strings.HasPrefix(p, "/api/") {
		notFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, notBuiltPage)
}
