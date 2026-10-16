// SPDX-License-Identifier: AGPL-3.0-only

package session

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// The limit refuses once a key has max attempts in the window, says how
// long until the oldest one that matters drops out, and forgets attempts
// as they expire.
func TestLimiterWindow(t *testing.T) {
	l := newLimiter(15 * time.Minute)
	u := limit{"u", "per_user", 5}
	for i := 0; i < 5; i++ {
		counts, refused, _ := l.attempt(t0.Add(time.Duration(i)*time.Minute), u)
		if refused != "" || counts[0] != i+1 {
			t.Fatalf("attempt %d: counts %v refused %q", i+1, counts, refused)
		}
	}
	counts, refused, wait := l.attempt(t0.Add(5*time.Minute), u)
	if refused != "per_user" || wait != 10*time.Minute || counts != nil {
		t.Fatalf("6th attempt: counts %v refused %q wait %v, want per_user 10m", counts, refused, wait)
	}
	// A refused attempt isn't counted: at t0+15m the first has expired,
	// so there is room for exactly one more.
	if _, refused, _ := l.attempt(t0.Add(15*time.Minute), u); refused != "" {
		t.Error("still refused once the oldest attempt expired")
	}
	if _, refused, _ := l.attempt(t0.Add(15*time.Minute), u); refused == "" {
		t.Error("a refused attempt was counted, or an expired one wasn't dropped")
	}
	// One refusal counts nothing anywhere: "other" stays empty.
	if _, refused, _ := l.attempt(t0.Add(15*time.Minute), limit{"other", "per_ip", 20}, u); refused != "per_user" {
		t.Fatalf("refused %q, want per_user", refused)
	}
	if _, ok := l.failures["other"]; ok {
		t.Error("a refused attempt counted against another key")
	}
	l.reset("u")
	if _, refused, _ := l.attempt(t0.Add(15*time.Minute), limit{"u", "per_user", 1}); refused != "" {
		t.Error("reset didn't forget")
	}
	// A key whose attempts all expired is dropped from the map.
	l.attempt(t0.Add(31*time.Minute), limit{"x", "", 5})
	if _, ok := l.failures["u"]; !ok {
		t.Fatal("missing key")
	}
	l.attempt(t0.Add(31*time.Minute), limit{"u", "", 5})
	l.refund(t0.Add(31*time.Minute), "u")
	if _, ok := l.failures["u"]; ok {
		t.Error("an expired, refunded key stayed in the map")
	}
}

// refund takes back exactly one attempt per key, the one made at that
// time, and leaves the others.
func TestLimiterRefund(t *testing.T) {
	l := newLimiter(time.Hour)
	a, b := limit{"a", "", 10}, limit{"b", "", 10}
	l.attempt(t0, a, b)
	l.attempt(t0.Add(time.Minute), a, b)
	l.attempt(t0.Add(time.Minute), a)
	l.refund(t0.Add(time.Minute), "a", "b")
	if got := len(l.failures["a"]); got != 2 {
		t.Errorf("a: %d attempts, want 2", got)
	}
	if got := len(l.failures["b"]); got != 1 || !l.failures["b"][0].Equal(t0) {
		t.Errorf("b: %v, want just t0", l.failures["b"])
	}
	l.refund(t0.Add(time.Hour/2), "a") // nothing at that time
	if got := len(l.failures["a"]); got != 2 {
		t.Errorf("a after a refund that matches nothing: %d", got)
	}
	l.refund(t0, "b")
	if _, ok := l.failures["b"]; ok {
		t.Error("an empty key stayed in the map")
	}
}

// A1: however many attempts race, at most max get through.
func TestLimiterConcurrent(t *testing.T) {
	l := newLimiter(time.Hour)
	var wg sync.WaitGroup
	var mu sync.Mutex
	passed := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, refused, _ := l.attempt(t0, limit{"ip", "per_ip", 20}, limit{"u", "per_user", 5}); refused == "" {
				mu.Lock()
				passed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if passed != 5 {
		t.Errorf("%d attempts passed, want 5", passed)
	}
}

// Many keys can't grow the map without bound: once it reaches sweepAt, an
// attempt sweeps out every expired key.
func TestLimiterSweeps(t *testing.T) {
	l := newLimiter(time.Minute)
	for i := 0; i < sweepAt; i++ {
		l.attempt(t0, limit{fmt.Sprint("k", i), "", 5})
	}
	l.attempt(t0.Add(2*time.Minute), limit{"new", "", 5})
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
