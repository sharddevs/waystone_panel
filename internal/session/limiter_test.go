// SPDX-License-Identifier: AGPL-3.0-only

package session

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// The limit blocks at max failures in the window, says how long until the
// oldest one that matters drops out, and forgets failures as they expire.
func TestLimiterWindow(t *testing.T) {
	l := newLimiter(15 * time.Minute)
	for i := 0; i < 5; i++ {
		if b, _ := l.blocked("u", 5, t0.Add(time.Duration(i)*time.Minute)); b {
			t.Fatalf("blocked after %d failures", i)
		}
		l.fail(t0.Add(time.Duration(i)*time.Minute), "u")
	}
	b, wait := l.blocked("u", 5, t0.Add(5*time.Minute))
	if !b || wait != 10*time.Minute {
		t.Fatalf("after 5 failures: blocked=%v wait=%v, want true 10m", b, wait)
	}
	// At t0+15m the first failure has expired: 4 left, not blocked.
	if b, _ := l.blocked("u", 5, t0.Add(15*time.Minute)); b {
		t.Error("still blocked once the oldest failure expired")
	}
	// The per-key counts come back from fail.
	if got := l.fail(t0.Add(15*time.Minute), "u", "other"); got[0] != 5 || got[1] != 1 {
		t.Errorf("counts %v, want [5 1]", got)
	}
	l.reset("u")
	if b, _ := l.blocked("u", 1, t0.Add(15*time.Minute)); b {
		t.Error("reset didn't forget")
	}
	// A key whose failures all expired is dropped from the map.
	if _, ok := l.failures["other"]; !ok {
		t.Fatal("missing key")
	}
	l.blocked("other", 5, t0.Add(31*time.Minute))
	if _, ok := l.failures["other"]; ok {
		t.Error("an expired key stayed in the map")
	}
}

// Many keys can't grow the map without bound: once it reaches sweepAt, a
// failure sweeps out every expired key.
func TestLimiterSweeps(t *testing.T) {
	l := newLimiter(time.Minute)
	for i := 0; i < sweepAt; i++ {
		l.fail(t0, fmt.Sprint("k", i))
	}
	l.fail(t0.Add(2*time.Minute), "new")
	if len(l.failures) != 1 {
		t.Errorf("%d keys after the sweep, want 1", len(l.failures))
	}
}

func TestRetryAfter(t *testing.T) {
	for d, want := range map[time.Duration]int{0: 1, -time.Second: 1, time.Millisecond: 1, 1500 * time.Millisecond: 2, 2 * time.Minute: 120} {
		if got := retryAfter(d); got != want {
			t.Errorf("retryAfter(%v) = %d, want %d", d, got, want)
		}
	}
}

// decodeJSON takes one JSON object with known fields and nothing else.
func TestDecodeJSON(t *testing.T) {
	type body struct {
		A string `json:"a"`
	}
	for _, c := range []struct {
		ctype, body, want string
	}{
		{"application/json", `{"a":"x"}`, ""},
		{"application/json; charset=utf-8", `{"a":"x"}`, ""},
		{"text/plain", `{"a":"x"}`, "unsupported_media_type"},
		{"application/x-www-form-urlencoded", `a=x`, "unsupported_media_type"},
		{"", `{"a":"x"}`, "unsupported_media_type"},
		{"application/json", `{"a":"x","b":1}`, "bad_request"},
		{"application/json", `{"a":"x"}{"a":"y"}`, "bad_request"},
		{"application/json", `{"a":1}`, "bad_request"},
		{"application/json", ``, "bad_request"},
		{"application/json", `{"a":"` + strings.Repeat("x", maxBody) + `"}`, "bad_request"},
	} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(c.body))
		if c.ctype != "" {
			r.Header.Set("Content-Type", c.ctype)
		}
		var v body
		if got := decodeJSON(httptest.NewRecorder(), r, &v); got != c.want {
			t.Errorf("%q %q: %q, want %q", c.ctype, c.body, got, c.want)
		}
	}
}

// Tokens are 43 characters of base64url and never repeat.
func TestNewToken(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := newToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != 43 || strings.ContainsAny(tok, "+/=") || seen[tok] {
			t.Fatalf("bad or repeated token %q", tok)
		}
		seen[tok] = true
	}
}
