// SPDX-License-Identifier: AGPL-3.0-only

package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sharddevs/waystone_panel/internal/password"
	"github.com/sharddevs/waystone_panel/internal/store"
	"github.com/sharddevs/waystone_panel/internal/web"
)

// slowStore knows no usernames, and holds every login lookup until
// release is closed, the way a queue of slow hashes would.
type slowStore struct {
	lookups atomic.Int32
	entered chan struct{}
	release chan struct{}
	err     error
}

func (s *slowStore) LookupLogin(ctx context.Context, username string) (store.LoginRecord, error) {
	s.lookups.Add(1)
	s.entered <- struct{}{}
	<-s.release
	if s.err != nil {
		return store.LoginRecord{}, s.err
	}
	return store.LoginRecord{}, store.ErrNotFound
}

func (s *slowStore) LookupSession(context.Context, [32]byte) (store.SessionRecord, error) {
	return store.SessionRecord{}, store.ErrNotFound
}

func (s *slowStore) InTenantTx(context.Context, store.Actor, func(context.Context, *store.Tx) error) error {
	return errors.New("not used")
}

func signInHandler(t *testing.T, st Store) http.Handler {
	t.Helper()
	m, err := New(Config{Store: st, Params: password.Params{Memory: 64, Time: 1, Threads: 1},
		Now: func() time.Time { return t0 }, Log: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	o, err := web.ParsePublicURL("https://panel.example.com")
	if err != nil {
		t.Fatal(err)
	}
	h, err := web.New(web.Config{Public: o, ProxyUID: 4242, Log: log.New(io.Discard, "", 0), Sessions: m}, m.Routes()...)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func signInRequest(username, ip string) *http.Request {
	b, _ := json.Marshal(map[string]string{"username": username, "password": "wrong password here"})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/session", strings.NewReader(string(b)))
	r.Host = "panel.example.com"
	r.Header.Set("X-Forwarded-For", ip)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "panel.example.com")
	r.Header.Set("Origin", "https://panel.example.com")
	r.Header.Set("Content-Type", "application/json")
	return r
}

// race sends n sign-ins at once, each named by req(i), and holds the
// lookups until every request has either reached one or been answered.
// It returns how many lookups ran and the status of each request.
func race(t *testing.T, h http.Handler, st *slowStore, n int, req func(i int) *http.Request) (int, map[int]int) {
	t.Helper()
	st.lookups.Store(0)
	st.entered = make(chan struct{}, n)
	st.release = make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req(i))
			mu.Lock()
			codes[w.Code]++
			mu.Unlock()
			if w.Code == http.StatusTooManyRequests && w.Header().Get("Retry-After") == "" {
				t.Error("429 without Retry-After")
			}
			done <- struct{}{}
		}(i)
	}
	// Every request either waits in a lookup or has been answered: only
	// then may the lookups finish, so none of them could have been
	// counted late.
	waiting, answered := 0, 0
	for waiting+answered < n {
		select {
		case <-st.entered:
			waiting++
		case <-done:
			answered++
		case <-time.After(10 * time.Second):
			t.Fatalf("stuck: %d waiting, %d answered of %d", waiting, answered, n)
		}
	}
	close(st.release)
	wg.Wait()
	return int(st.lookups.Load()), codes
}

// A1: 40 parallel guesses at one username get 5 password checks; the
// other 35 are refused before any lookup or hash.
func TestSignInConcurrentPerUser(t *testing.T) {
	st := &slowStore{}
	lookups, codes := race(t, signInHandler(t, st), st, 40, func(int) *http.Request { return signInRequest("trevor", "203.0.113.7") })
	if lookups != limitPerUser {
		t.Errorf("%d lookups, want %d", lookups, limitPerUser)
	}
	if codes[http.StatusUnauthorized] != limitPerUser || codes[http.StatusTooManyRequests] != 40-limitPerUser {
		t.Errorf("statuses %v, want %d×401 and %d×429", codes, limitPerUser, 40-limitPerUser)
	}
}

// The same for one address trying many usernames: 20, then 429.
func TestSignInConcurrentPerIP(t *testing.T) {
	st := &slowStore{}
	lookups, codes := race(t, signInHandler(t, st), st, 60, func(i int) *http.Request {
		return signInRequest("user"+string(rune('a'+i%26))+string(rune('a'+i/26)), "203.0.113.7")
	})
	if lookups != limitPerIP {
		t.Errorf("%d lookups, want %d", lookups, limitPerIP)
	}
	if codes[http.StatusTooManyRequests] != 60-limitPerIP {
		t.Errorf("statuses %v, want %d×429", codes, 60-limitPerIP)
	}
}

// A sign-in that couldn't be decided (the store failed) gives its attempt
// back: 503s never lock anyone out.
func TestSignInBusyIsRefunded(t *testing.T) {
	st := &slowStore{err: errors.New("database down")}
	h := signInHandler(t, st)
	lookups, codes := race(t, h, st, 10, func(int) *http.Request { return signInRequest("trevor", "203.0.113.7") })
	if lookups != limitPerUser || codes[http.StatusServiceUnavailable] != limitPerUser {
		t.Fatalf("store down: lookups %d, statuses %v", lookups, codes)
	}
	// All five were given back, so with the store back the next five are
	// checked again instead of refused.
	st.err = nil
	lookups, codes = race(t, h, st, 10, func(int) *http.Request { return signInRequest("trevor", "203.0.113.7") })
	if lookups != limitPerUser || codes[http.StatusUnauthorized] != limitPerUser {
		t.Errorf("store back: lookups %d, statuses %v", lookups, codes)
	}
}

// knownStore knows one account, whose password is right; creating the
// session then fails, which is fine here: the password was checked.
type knownStore struct{ rec store.LoginRecord }

func (s knownStore) LookupLogin(context.Context, string) (store.LoginRecord, error) {
	return s.rec, nil
}

func (knownStore) LookupSession(context.Context, [32]byte) (store.SessionRecord, error) {
	return store.SessionRecord{}, store.ErrNotFound
}

func (knownStore) InTenantTx(context.Context, store.Actor, func(context.Context, *store.Tx) error) error {
	return errors.New("not used")
}

// A right password doesn't count against the address: many people behind
// one NAT can all sign in.
func TestSignInSuccessIsNotCounted(t *testing.T) {
	hash, err := password.Hash("right password here", password.Params{Memory: 64, Time: 1, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	h := signInHandler(t, knownStore{store.LoginRecord{UserID: "0199e1a0-0000-7000-8000-0000000000f1", PasswordHash: hash}})
	for i := 0; i < limitPerIP+5; i++ {
		b, _ := json.Marshal(map[string]string{"username": "trevor", "password": "right password here"})
		r := signInRequest("trevor", "203.0.113.7")
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("sign-in %d with the right password: 429", i+1)
		}
	}
}

// A sign-in that gives up waiting for a hash slot is a 503, and its
// attempt is given back.
func TestSignInHashWaitIsRefunded(t *testing.T) {
	m, err := New(Config{Store: knownStore{}, Params: password.Params{Memory: 64, Time: 1, Threads: 1},
		Now: func() time.Time { return t0 }, Log: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	o, err := web.ParsePublicURL("https://panel.example.com")
	if err != nil {
		t.Fatal(err)
	}
	h, err := web.New(web.Config{Public: o, ProxyUID: 4242, Log: log.New(io.Discard, "", 0), Sessions: m}, m.Routes()...)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxHashes; i++ {
		m.hashing <- struct{}{} // every slot taken
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, signInRequest("trevor", "203.0.113.7").WithContext(ctx))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d %s, want 503", w.Code, w.Body.String())
	}
	if n := len(m.limits.failures); n != 0 {
		t.Errorf("%d limiter keys after a 503, want 0", n)
	}
}

// A4: IPv6 counts by /64, IPv4-mapped IPv6 as IPv4.
func TestAddrKey(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7":               "ip:203.0.113.7",
		"::ffff:203.0.113.7":        "ip:203.0.113.7",
		"2001:db8:1:2:aaaa::1":      "ip:2001:db8:1:2::/64",
		"2001:db8:1:2:ffff:ffff::9": "ip:2001:db8:1:2::/64",
		"2001:db8:1:3::1":           "ip:2001:db8:1:3::/64",
		"fe80::1%eth0":              "ip:fe80::/64",
	} {
		if got := addrKey(netip.MustParseAddr(in)); got != want {
			t.Errorf("addrKey(%s) = %q, want %q", in, got, want)
		}
	}
}

// A4 end to end: twenty addresses in one /64 share one limit.
func TestSignInIPv6SharesPrefix(t *testing.T) {
	st := &slowStore{}
	lookups, codes := race(t, signInHandler(t, st), st, 30, func(i int) *http.Request {
		return signInRequest("user"+string(rune('a'+i)), "2001:db8:1:2::"+string(rune('a'+i%6))+string(rune('0'+i/6)))
	})
	if lookups != limitPerIP || codes[http.StatusTooManyRequests] != 30-limitPerIP {
		t.Errorf("lookups %d, statuses %v, want %d lookups", lookups, codes, limitPerIP)
	}
}

func busyManager(t *testing.T) (*Manager, http.Handler) {
	t.Helper()
	m, err := New(Config{Store: knownStore{}, Params: password.Params{Memory: 64, Time: 1, Threads: 1},
		Now: func() time.Time { return t0 }, Log: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	o, err := web.ParsePublicURL("https://panel.example.com")
	if err != nil {
		t.Fatal(err)
	}
	h, err := web.New(web.Config{Public: o, ProxyUID: 4242, Log: log.New(io.Discard, "", 0), Sessions: m}, m.Routes()...)
	if err != nil {
		t.Fatal(err)
	}
	return m, h
}

// A2: a sign-in waits for a hash slot at most m.wait, then 503s with
// its attempt given back.
func TestSignInHashWaitBounded(t *testing.T) {
	m, h := busyManager(t)
	m.wait = 50 * time.Millisecond
	for i := 0; i < maxHashes; i++ {
		m.hashing <- struct{}{}
	}
	start := time.Now()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, signInRequest("trevor", "203.0.113.7"))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), web.CodeBusy) {
		t.Fatalf("%d %s, want 503 controller_busy", w.Code, w.Body.String())
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("waited %v", d)
	}
	if n := len(m.limits.failures); n != 0 {
		t.Errorf("%d limiter keys after a 503, want 0", n)
	}
}

// A2: past maxHashWaiters in line, a sign-in is refused at once.
func TestSignInWaitersCapped(t *testing.T) {
	m, h := busyManager(t)
	m.wait = time.Hour // a refusal here can't be the timeout
	for i := 0; i < maxHashWaiters; i++ {
		m.waiting <- struct{}{}
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, signInRequest("trevor", "203.0.113.7"))
		done <- w
	}()
	select {
	case w := <-done:
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%d %s, want 503", w.Code, w.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sign-in waited instead of being refused")
	}
}

// With maxLookups session lookups running, one more is refused at once,
// and web turns that into 503, not 401.
func TestSessionLookupsCapped(t *testing.T) {
	m, h := busyManager(t)
	// Each lookup gives its slot back: many in a row never run out.
	for i := 0; i < 2*maxLookups; i++ {
		if _, err := m.ResolveSession(context.Background(), strings.Repeat("A", web.TokenLength)); !errors.Is(err, web.ErrNoSession) {
			t.Fatalf("lookup %d: %v, want ErrNoSession", i+1, err)
		}
	}
	for i := 0; i < maxLookups; i++ {
		m.lookups <- struct{}{}
	}
	if _, err := m.ResolveSession(context.Background(), strings.Repeat("A", web.TokenLength)); !errors.Is(err, errLookupBusy) {
		t.Fatalf("ResolveSession: %v, want errLookupBusy", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	r.Host = "panel.example.com"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "panel.example.com")
	r.AddCookie(&http.Cookie{Name: web.SessionCookieName, Value: strings.Repeat("A", web.TokenLength)})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("%d %s, want 503", w.Code, w.Body.String())
	}
	<-m.lookups
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("with a slot free: %d %s, want 401", w.Code, w.Body.String())
	}
}

// slowSessions holds every session lookup until release is closed.
type slowSessions struct {
	knownStore
	entered chan struct{}
	release chan struct{}
}

func (s slowSessions) LookupSession(context.Context, [32]byte) (store.SessionRecord, error) {
	s.entered <- struct{}{}
	<-s.release
	return store.SessionRecord{}, store.ErrNotFound
}

// N1: one address holding maxLookupsPerAddr lookups gets 503 for the
// next, while another address is still looked up.
func TestSessionLookupsPerAddress(t *testing.T) {
	st := slowSessions{entered: make(chan struct{}, 20), release: make(chan struct{})}
	h := signInHandler(t, st)
	get := func(ip string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
		r.Host = "panel.example.com"
		r.Header.Set("X-Forwarded-For", ip)
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("X-Forwarded-Host", "panel.example.com")
		r.AddCookie(&http.Cookie{Name: web.SessionCookieName, Value: strings.Repeat("A", web.TokenLength)})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	var wg sync.WaitGroup
	codes := make(chan int, 20)
	for i := 0; i < maxLookupsPerAddr; i++ {
		wg.Add(1)
		// Different addresses in one /64 are one address here.
		ip := fmt.Sprintf("2001:db8:1:2::%x", i+1)
		go func() { defer wg.Done(); codes <- get(ip).Code }()
		<-st.entered
	}
	if w := get("2001:db8:1:2::ff"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("one more from the same /64: %d, want 503", w.Code)
	}
	wg.Add(1)
	go func() { defer wg.Done(); codes <- get("203.0.113.9").Code }()
	select {
	case <-st.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("another address wasn't looked up")
	}
	close(st.release)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusUnauthorized {
			t.Errorf("a held lookup answered %d, want 401", c)
		}
	}
}

func TestInflight(t *testing.T) {
	f := inflight{n: map[string]int{}}
	if !f.acquire("a", 2) || !f.acquire("a", 2) || f.acquire("a", 2) || !f.acquire("b", 2) {
		t.Fatal("acquire doesn't count per key")
	}
	f.release("a")
	if !f.acquire("a", 2) {
		t.Error("release didn't give the place back")
	}
	f.release("a")
	f.release("a")
	f.release("b")
	if len(f.n) != 0 {
		t.Errorf("keys left: %v", f.n)
	}
}

// A real sign-in succeeding doesn't clear guesses counted against the
// same username at the same time: only its own attempt is taken back.
func TestSignInSuccessKeepsOthersAttempts(t *testing.T) {
	hash, err := password.Hash("right password here", password.Params{Memory: 64, Time: 1, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Config{Store: knownStore{store.LoginRecord{UserID: "0199e1a0-0000-7000-8000-0000000000f1", PasswordHash: hash}},
		Params: password.Params{Memory: 64, Time: 1, Threads: 1}, Now: func() time.Time { return t0 }, Log: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	o, err := web.ParsePublicURL("https://panel.example.com")
	if err != nil {
		t.Fatal(err)
	}
	h, err := web.New(web.Config{Public: o, ProxyUID: 4242, Log: log.New(io.Discard, "", 0), Sessions: m}, m.Routes()...)
	if err != nil {
		t.Fatal(err)
	}
	// Four guesses from elsewhere, counted (in flight or failed).
	for i := 0; i < limitPerUser-1; i++ {
		m.limits.attempt(t0, limit{"ip:198.51.100.9", "per_ip", limitPerIP}, limit{"user:trevor", "per_user", limitPerUser})
	}
	b, _ := json.Marshal(map[string]string{"username": "trevor", "password": "right password here"})
	r := signInRequest("trevor", "203.0.113.7")
	r.Body = io.NopCloser(strings.NewReader(string(b)))
	h.ServeHTTP(httptest.NewRecorder(), r)
	if n := len(m.limits.failures["user:trevor"]); n != limitPerUser-1 {
		t.Errorf("%d attempts on the username after a success, want the other %d", n, limitPerUser-1)
	}
	if _, ok := m.limits.failures["ip:203.0.113.7"]; ok {
		t.Error("the success counted against its address")
	}
}
