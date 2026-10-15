// SPDX-License-Identifier: AGPL-3.0-only

package web

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
)

const testProxyUID = 4242

// syncBuffer is a log sink that is safe from several goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func mustOrigin(t *testing.T, raw string) Origin {
	t.Helper()
	o, err := ParsePublicURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// newTestHandler builds a handler for https://panel.example.com.
func newTestHandler(t *testing.T, routes ...Route) (*Handler, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	h, err := New(Config{
		Public:   mustOrigin(t, "https://panel.example.com"),
		ProxyUID: testProxyUID,
		Log:      log.New(logs, "", 0),
	}, routes...)
	if err != nil {
		t.Fatal(err)
	}
	return h, logs
}

// caddyRequest is a request exactly as the supported Caddyfile forwards
// it (WS scope §4a): the browser's Host, and the three X-Forwarded-*
// headers set by header_up. Unsafe methods also get the browser's Origin.
func caddyRequest(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Host = "panel.example.com"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "panel.example.com")
	if unsafeMethod(method) {
		r.Header.Set("Origin", "https://panel.example.com")
	}
	return r
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o600) }
