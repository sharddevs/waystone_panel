// SPDX-License-Identifier: AGPL-3.0-only

package fetch

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sharddevs/waystone_panel/internal/profile/minecraft"
)

var jar = bytes.Repeat([]byte("fetch test jar\n"), 5000)

// server serves body at /jar over TLS and counts requests.
func server(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func artifactAt(url string) minecraft.Artifact {
	s1 := sha1.Sum(jar)
	s256 := sha256.Sum256(jar)
	return minecraft.Artifact{Version: "26.3", URL: url + "/jar", SHA1: hex.EncodeToString(s1[:]),
		SHA256: hex.EncodeToString(s256[:]), Size: int64(len(jar)), Runtime: "java25"}
}

func fetcher(t *testing.T, srv *httptest.Server, art minecraft.Artifact) *Fetcher {
	t.Helper()
	c := srv.Client()
	c.CheckRedirect = NewClient().CheckRedirect
	c.Timeout = Timeout
	return &Fetcher{Client: c, Dir: t.TempDir(), Artifacts: []minecraft.Artifact{art}, Log: log.New(io.Discard, "", 0)}
}

func serve(f *Fetcher, request string) (string, error) {
	var out bytes.Buffer
	err := f.Serve(context.Background(), strings.NewReader(request), &out)
	return out.String(), err
}

func TestFetch(t *testing.T) {
	srv, n := server(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(jar) })
	art := artifactAt(srv.URL)
	f := fetcher(t, srv, art)
	reply, err := serve(f, art.SHA256+"\n")
	if err != nil || reply != "ok\n" {
		t.Fatalf("%q %v", reply, err)
	}
	got, err := os.ReadFile(filepath.Join(f.Dir, art.SHA256))
	if err != nil || !bytes.Equal(got, jar) {
		t.Fatalf("staged %d bytes, %v", len(got), err)
	}
	fi, _ := os.Stat(filepath.Join(f.Dir, art.SHA256))
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", fi.Mode())
	}
	if left, _ := filepath.Glob(filepath.Join(f.Dir, "*.part")); len(left) != 0 {
		t.Errorf("left %v", left)
	}
	if n.Load() != 1 {
		t.Errorf("%d requests", n.Load())
	}
}

// A request that isn't a pinned hash makes no request at all.
func TestFetchRefusesUnknownRequests(t *testing.T) {
	srv, n := server(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(jar) })
	art := artifactAt(srv.URL)
	f := fetcher(t, srv, art)
	other := sha256.Sum256([]byte("x"))
	for name, req := range map[string]string{
		"empty":        "",
		"no newline":   art.SHA256,
		"uppercase":    strings.ToUpper(art.SHA256) + "\n",
		"another hash": hex.EncodeToString(other[:]) + "\n",
		"a URL":        "https://example.com/x.jar\n",
		"a path":       "../../etc/passwd\n",
		"sha1":         art.SHA1 + "\n",
		"extra":        art.SHA256 + "x\n",
	} {
		reply, err := serve(f, req)
		if reply != "refused\n" || !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %q %v", name, reply, err)
		}
	}
	if n.Load() != 0 {
		t.Errorf("%d requests went out", n.Load())
	}
}

func TestFetchRefusesBadDownloads(t *testing.T) {
	wrong := bytes.Clone(jar)
	wrong[3] ^= 1
	for name, h := range map[string]http.HandlerFunc{
		"redirect":  func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) },
		"not found": func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) },
		"short":     func(w http.ResponseWriter, _ *http.Request) { w.Write(jar[:len(jar)-1]) },
		"oversize":  func(w http.ResponseWriter, _ *http.Request) { w.Write(append(bytes.Clone(jar), 'x')) },
		"wrong":     func(w http.ResponseWriter, _ *http.Request) { w.Write(wrong) },
	} {
		srv, _ := server(t, h)
		art := artifactAt(srv.URL)
		f := fetcher(t, srv, art)
		reply, err := serve(f, art.SHA256+"\n")
		if reply != "failed\n" || err == nil {
			t.Errorf("%s: %q %v", name, reply, err)
		}
		if left, _ := os.ReadDir(f.Dir); len(left) != 0 {
			t.Errorf("%s: kept %v", name, left)
		}
	}
	// Only https, even from a plain server with the right jar.
	var n atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.Write(jar)
	}))
	t.Cleanup(plain.Close)
	art := artifactAt(plain.URL)
	f := fetcher(t, plain, art)
	if err := f.Fetch(context.Background(), art); err == nil || n.Load() != 0 {
		t.Errorf("http: %v, %d requests", err, n.Load())
	}
}

// Each check refuses on its own, with the right bytes otherwise.
func TestFetchChecksEachPin(t *testing.T) {
	for name, tc := range map[string]struct {
		h   http.HandlerFunc
		pin func(*minecraft.Artifact)
	}{
		"status 500": {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500); w.Write(jar) }, nil},
		"size":       {func(w http.ResponseWriter, _ *http.Request) { w.Write(jar) }, func(a *minecraft.Artifact) { a.Size-- }},
		"sha256": {func(w http.ResponseWriter, _ *http.Request) { w.Write(jar) },
			func(a *minecraft.Artifact) { a.SHA256 = strings.Repeat("0", 64) }},
	} {
		srv, _ := server(t, tc.h)
		art := artifactAt(srv.URL)
		if tc.pin != nil {
			tc.pin(&art)
		}
		f := fetcher(t, srv, art)
		if err := f.Fetch(context.Background(), art); err == nil {
			t.Errorf("%s: fetched", name)
		}
		if left, _ := os.ReadDir(f.Dir); len(left) != 0 {
			t.Errorf("%s: kept %v", name, left)
		}
	}
}

// A body that never ends is cut at the pinned size plus one byte, long
// before the download timeout.
func TestFetchStopsAtThePinnedSize(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(jar)
		chunk := bytes.Repeat([]byte("x"), 32<<10)
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	art := artifactAt(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := fetcher(t, srv, art).Fetch(ctx, art)
	if err == nil || ctx.Err() != nil {
		t.Errorf("endless body: %v (context %v)", err, ctx.Err())
	}
}

// The wrong SHA-1 alone is enough to refuse.
func TestFetchChecksSHA1(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, _ *http.Request) { w.Write(jar) })
	art := artifactAt(srv.URL)
	art.SHA1 = strings.Repeat("0", 40)
	f := fetcher(t, srv, art)
	if reply, _ := serve(f, art.SHA256+"\n"); reply != "failed\n" {
		t.Errorf("%q", reply)
	}
}

func TestAsk(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "fetch.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	replies := make(chan string, 2)
	replies <- "ok\n"
	replies <- "failed\n"
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 65)
			io.ReadFull(c, buf)
			c.Write([]byte(<-replies))
			c.Close()
		}
	}()
	art := artifactAt("https://x")
	path, err := Ask(context.Background(), sock, "/stage", art)
	if err != nil || path != "/stage/"+art.SHA256 {
		t.Errorf("%q %v", path, err)
	}
	if _, err := Ask(context.Background(), sock, "/stage", art); err == nil {
		t.Error("a failed answer was taken")
	}
	if _, err := Ask(context.Background(), filepath.Join(dir, "none.sock"), "/stage", art); err == nil {
		t.Error("no service, no error")
	}
}

// 6c-I2: no proxy from the environment, TLS 1.2 or newer, no redirects.
func TestNewClient(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://192.0.2.1:3128")
	c := NewClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport %T", c.Transport)
	}
	if tr.Proxy != nil {
		t.Error("the client takes a proxy from the environment")
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Error("the client allows TLS older than 1.2")
	}
	if tr.TLSClientConfig != nil && (tr.TLSClientConfig.InsecureSkipVerify || tr.TLSClientConfig.RootCAs != nil) {
		t.Error("the client doesn't use the system's roots")
	}
	if c.CheckRedirect == nil || c.CheckRedirect(nil, nil) == nil {
		t.Error("the client follows redirects")
	}
	if c.Timeout != Timeout {
		t.Errorf("timeout %v", c.Timeout)
	}
}
