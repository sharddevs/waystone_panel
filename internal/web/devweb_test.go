// SPDX-License-Identifier: AGPL-3.0-only

//go:build devweb

package web

import (
	"io"
	"log"
	"net/http/httptest"
	"testing"
)

// W7: a devweb build also accepts http, for loopback hosts only.
func TestParsePublicURLDevWeb(t *testing.T) {
	for _, in := range []string{"http://localhost", "http://localhost:5173", "http://127.0.0.1:8080", "http://[::1]:3000", "https://panel.example.com"} {
		if _, err := ParsePublicURL(in); err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
	for _, in := range []string{"http://panel.example.com", "http://192.168.1.10", "http://localhost.example.com", "http://127.0.0.2", "http://[::2]", "HTTP://localhost"} {
		if _, err := ParsePublicURL(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	o, _ := ParsePublicURL("http://localhost:80")
	if o.String() != "http://localhost" {
		t.Errorf("default http port kept: %q", o.String())
	}
}

// HSTS is only ever sent for https: a devweb handler on http://localhost
// never sends it, even on accepted requests.
func TestNoHSTSOnHTTP(t *testing.T) {
	o, err := ParsePublicURL("http://localhost:8080")
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Public: o, ProxyUID: testProxyUID, Log: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.Host = "localhost:8080"
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	r.Header.Set("X-Forwarded-Proto", "http")
	r.Header.Set("X-Forwarded-Host", "localhost:8080")
	w := serve(h, r)
	if w.Code != 200 || w.Header().Get("Strict-Transport-Security") != "" {
		t.Errorf("http healthz: %d, HSTS %q; want 200 without HSTS", w.Code, w.Header().Get("Strict-Transport-Security"))
	}
}
