// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// /healthz needs no identity and no database, so a Config without a
// Store is enough here.
func TestHealthz(t *testing.T) {
	h := New(Config{Version: "1.2.3-test"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body.Status != "ok" || body.Version != "1.2.3-test" {
		t.Errorf("body = %+v", body)
	}
}

// A wrong method or an unknown path gets the same JSON error shape as
// everything else: not_found, with the request ID.
func TestUnknownRoutesAreJSONNotFound(t *testing.T) {
	h := New(Config{Version: "dev", Log: discardLogger()})
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/healthz"},
		{http.MethodGet, "/nope"},
		{http.MethodDelete, "/api/v1/servers"},
		{http.MethodGet, "/api/v1/servers/x/start"},
		{http.MethodPut, "/api/v1/operations/x/resolve"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		e := decodeError(t, rec)
		if rec.Code != http.StatusNotFound || e.Code != CodeNotFound {
			t.Errorf("%s %s: %d %q, want 404 not_found", tc.method, tc.path, rec.Code, e.Code)
		}
	}
}

// Every response carries a fresh request ID, and a client can't choose it.
func TestRequestIDs(t *testing.T) {
	h := New(Config{Version: "dev", Log: discardLogger()})
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set("X-Request-ID", "chosen-by-client")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		id := rec.Header().Get("X-Request-ID")
		if len(id) != 36 || id == "chosen-by-client" || seen[id] {
			t.Errorf("request ID %q: want a new UUID each time", id)
		}
		seen[id] = true
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if e := decodeError(t, rec); e.RequestID != rec.Header().Get("X-Request-ID") {
		t.Errorf("error body request_id %q != header %q", e.RequestID, rec.Header().Get("X-Request-ID"))
	}
}

// errorJSON is the "error" object of an error response.
type errorJSON struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	RequestID   string `json:"request_id"`
	OperationID string `json:"operation_id"`
}

// decodeError parses rec as an error response and checks the parts every
// error must have.
func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorJSON {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("error Content-Type = %q, want application/json (body %q)", ct, rec.Body.String())
	}
	var body struct {
		Error errorJSON `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body isn't JSON: %v (%q)", err, rec.Body.String())
	}
	e := body.Error
	if e.Code == "" || e.Message == "" || e.RequestID == "" {
		t.Errorf("error body missing code, message or request_id: %q", rec.Body.String())
	}
	if info, ok := errorInfo[e.Code]; ok && (info.message != e.Message || info.status != rec.Code) {
		t.Errorf("code %s: got %d %q, want the fixed %d %q", e.Code, rec.Code, e.Message, info.status, info.message)
	}
	return e
}
