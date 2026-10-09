package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewServerConfiguration(t *testing.T) {

	server := newServer("0.1.0-alpha")

	if server.Addr != "127.0.0.1:8080" {
		t.Errorf("expected loopback address, got %q", server.Addr)
	}

	if server.ReadHeaderTimeout != 5*time.Second {
		t.Errorf(
			"expected 5-second header timeout, got %v",
			server.ReadHeaderTimeout,
		)
	}
}
func TestNewServerVersion(t *testing.T) {

	server := newServer("1.2.3-test")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()

	server.Handler.ServeHTTP(recorder, req)

	var body struct {
		Version string `json:"version"`
	}

	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if body.Version != "1.2.3-test" {
		t.Errorf("expected version 1.2.3-test, got %q", body.Version)
	}
}
