// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The contracts C-W2, C-W3, C-W4, C-W6 and C-W7 as a scan of every
// non-test Go file outside this package. They are guardrails, not proof:
// the boundary itself is that New is the only way to build the handler
// (R3), and stage 5 and 7 keep their own acceptance gates.

// identityHeaders may only be named inside this package (C-W3).
var identityHeaders = []string{
	"x-forwarded-for", "x-forwarded-proto", "x-forwarded-host", "x-forwarded-port", "x-forwarded-server",
	"x-forwarded-prefix", "forwarded", "x-real-ip", "x-original-url", "x-original-host", "x-rewrite-url",
	"x-client-ip", "true-client-ip", "cf-connecting-ip", "location",
}

// allowedHeaderUses are the reviewed exceptions to C-W3/C-W4, by file
// (slash-separated, from the repository root) and header. Keep this short.
var allowedHeaderUses = map[string]string{
	// The local API's 202 points at the new operation with a relative
	// Location. It is not a redirect and not built from the request.
	"internal/api/handlers.go": "location",
}

// websocketImports may only be imported inside this package (C-W6).
var websocketImports = []string{
	"github.com/gorilla/websocket", "nhooyr.io/websocket", "github.com/coder/websocket",
	"golang.org/x/net/websocket", "github.com/gobwas/ws",
}

func TestBrowserContracts(t *testing.T) {
	root := filepath.Join("..", "..")
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "gen" || name == "node_modules" || name == "web" && filepath.Base(filepath.Dir(path)) != "internal" {
				return filepath.SkipDir
			}
			if abs, _ := filepath.Abs(path); abs == self {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		for _, v := range scanFile(t, path) {
			t.Errorf("%s", v)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 50 {
		t.Fatalf("scanned only %d files; the walk is broken", files)
	}
}

// scanFile returns the contract violations in one file.
func scanFile(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var out []string
	bad := func(n ast.Node, what string) {
		out = append(out, fset.Position(n.Pos()).String()+": "+what)
	}
	httpName := ""
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		for _, ws := range websocketImports {
			if p == ws || strings.HasPrefix(p, ws+"/") {
				bad(imp, "C-W6: WebSocket library outside internal/web")
			}
		}
		if p == "net/http" {
			httpName = "http"
			if imp.Name != nil {
				httpName = imp.Name.Name
			}
		}
	}
	isHTTP := func(e ast.Expr, name string) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && httpName != "" && id.Name == httpName
	}
	// requestVars: names declared with type *http.Request anywhere in the
	// file (parameters, variables). A coarse but safe over-approximation.
	requestVars := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		var names []*ast.Ident
		var typ ast.Expr
		switch v := n.(type) {
		case *ast.Field:
			names, typ = v.Names, v.Type
		case *ast.ValueSpec:
			names, typ = v.Names, v.Type
		default:
			return true
		}
		if star, ok := typ.(*ast.StarExpr); ok && isHTTP(star.X, "Request") {
			for _, id := range names {
				requestVars[id.Name] = true
			}
		}
		return true
	})
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CallExpr:
			if isHTTP(v.Fun, "SetCookie") {
				bad(v, "C-W2: http.SetCookie outside internal/web; use web.SessionCookie")
			}
			if isHTTP(v.Fun, "Redirect") {
				bad(v, "C-W4: http.Redirect outside internal/web; use web.Redirect")
			}
		case *ast.CompositeLit:
			if isHTTP(v.Type, "Cookie") {
				bad(v, "C-W2: http.Cookie built outside internal/web; use web.SessionCookie")
			}
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				s, err := strconv.Unquote(v.Value)
				if err == nil {
					for _, h := range identityHeaders {
						if strings.EqualFold(s, h) && !allowedUse(path, h) {
							bad(v, "C-W3/C-W4: header "+s+" named outside internal/web")
						}
					}
				}
			}
		case *ast.SelectorExpr:
			if id, ok := v.X.(*ast.Ident); ok && requestVars[id.Name] && (v.Sel.Name == "Host" || v.Sel.Name == "RemoteAddr") {
				bad(v, "C-W3: request "+v.Sel.Name+" read outside internal/web")
			}
		}
		return true
	})
	return out
}

func allowedUse(path, header string) bool {
	rel, err := filepath.Rel(filepath.Join("..", ".."), path)
	if err != nil {
		return false
	}
	return allowedHeaderUses[filepath.ToSlash(rel)] == header
}

// The scan must actually catch what it claims to. Each snippet holds one
// violation.
func TestBrowserContractsScanWorks(t *testing.T) {
	dir := t.TempDir()
	for i, src := range []string{
		`package x; import "net/http"; func f(w http.ResponseWriter) { http.SetCookie(w, nil) }`,
		`package x; import h "net/http"; var c = h.Cookie{Name: "a"}`,
		`package x; import "net/http"; var c = &http.Cookie{}`,
		`package x; import "net/http"; func f(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/", 302) }`,
		`package x; import "net/http"; func f(r *http.Request) string { return r.Header.Get("X-Forwarded-For") }`,
		`package x; func f(m map[string]string) string { return m["x-real-ip"] }`,
		`package x; import "net/http"; func f(w http.ResponseWriter) { w.Header().Set("Location", "/") }`,
		`package x; import "net/http"; func f(req *http.Request) string { return req.Host }`,
		`package x; import "net/http"; func f(r *http.Request) string { return r.RemoteAddr }`,
		`package x; import "net/http"; func f() { var r *http.Request; _ = r.Host }`,
		`package x; import _ "github.com/gorilla/websocket"`,
		`package x; import _ "golang.org/x/net/websocket"`,
	} {
		path := filepath.Join(dir, "f"+strconv.Itoa(i)+".go")
		if err := writeFile(path, src); err != nil {
			t.Fatal(err)
		}
		if got := scanFile(t, path); len(got) != 1 {
			t.Errorf("snippet %d: %d violations %q, want 1:\n%s", i, len(got), got, src)
		}
	}
	clean := filepath.Join(dir, "clean.go")
	if err := writeFile(clean, `package x; import "net/http"; func f(r *http.Request) string { return r.URL.Path + r.Method }`); err != nil {
		t.Fatal(err)
	}
	if got := scanFile(t, clean); len(got) != 0 {
		t.Errorf("clean file flagged: %q", got)
	}
}

// C-W7: this package never imports the local API, whose identity model
// (the peer uid is the actor) must not reach the browser path.
func TestNoLocalAPIImport(t *testing.T) {
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range matches {
		if strings.HasSuffix(m, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), m, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if strings.Contains(imp.Path.Value, "/internal/api") {
				t.Errorf("%s imports %s", m, imp.Path.Value)
			}
		}
	}
}
