// SPDX-License-Identifier: AGPL-3.0-only

// Package fetch downloads a pinned server jar for the controller (stage
// 6c-3). The controller runs with no network at all (N15), so the one
// download Waystone makes happens in waystone-fetch, a separate service
// with network and nothing else: no database credentials and no socket
// but the one it was started on.
//
// The controller asks over a socket with one line, the jar's SHA-256.
// No URL, path or setting ever crosses it: fetch looks the hash up in the
// set it was built with, downloads only that artifact's pinned URL, and
// keeps the file only if its size, SHA-1 and SHA-256 are the pin's. It
// answers with one word. The controller checks the file again itself
// before it trusts it.
package fetch

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/sharddevs/waystone_panel/internal/profile/minecraft"
)

// The replies, one line each.
const (
	ReplyOK      = "ok"
	ReplyRefused = "refused" // not a hash in the pinned set; nothing was fetched
	ReplyFailed  = "failed"  // the download or a check failed; nothing was kept
)

// DefaultDir is waystone-fetch's StateDirectory, where a finished jar
// waits as <sha256> for the controller.
const DefaultDir = "/var/lib/waystone-fetch"

// DefaultSocket is the socket the controller asks on.
const DefaultSocket = "/run/waystone-fetch/fetch.sock"

// Timeout is the most one download may take.
const Timeout = 2 * time.Minute

// ErrRefused: the request wasn't a pinned hash.
var ErrRefused = errors.New("not a pinned artifact")

var errRedirect = errors.New("redirects are refused")

// NewClient is the HTTP client fetch uses: the system's roots, TLS 1.2 or
// newer, no proxy from the environment, no redirects, and the download
// timeout (6c-I2).
func NewClient() *http.Client {
	return &http.Client{
		Timeout: Timeout,
		Transport: &http.Transport{
			Proxy:               nil,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: 15 * time.Second,
			ForceAttemptHTTP2:   true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errRedirect
		},
	}
}

// Fetcher serves the controller's requests.
type Fetcher struct {
	Client    *http.Client         // NewClient()
	Dir       string               // DefaultDir
	Artifacts []minecraft.Artifact // minecraft.Pinned
	Log       *log.Logger          // log.Default()
}

func (f *Fetcher) defaults() {
	if f.Client == nil {
		f.Client = NewClient()
	}
	if f.Dir == "" {
		f.Dir = DefaultDir
	}
	if f.Artifacts == nil {
		f.Artifacts = minecraft.Pinned
	}
	if f.Log == nil {
		f.Log = log.Default()
	}
}

// ReadRequest reads the controller's one line: 64 lowercase hex digits
// and a newline, naming an artifact in set. Anything else is ErrRefused.
func ReadRequest(r io.Reader, set []minecraft.Artifact) (minecraft.Artifact, error) {
	line, err := bufio.NewReader(io.LimitReader(r, 2*sha256.Size+1)).ReadString('\n')
	if err != nil || len(line) != 2*sha256.Size+1 {
		return minecraft.Artifact{}, ErrRefused
	}
	sum := line[:2*sha256.Size]
	if b, err := hex.DecodeString(sum); err != nil || hex.EncodeToString(b) != sum {
		return minecraft.Artifact{}, ErrRefused
	}
	art, ok := minecraft.ForSHA256(set, sum)
	if !ok {
		return minecraft.Artifact{}, ErrRefused
	}
	return art, nil
}

// Serve answers one request read from r, on w. Its error says why it
// didn't answer ok; the reply has already been written.
func (f *Fetcher) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	f.defaults()
	art, err := ReadRequest(r, f.Artifacts)
	if err != nil {
		f.Log.Printf("fetch=refused reason=not_pinned")
		fmt.Fprintln(w, ReplyRefused)
		return err
	}
	if err := f.Fetch(ctx, art); err != nil {
		f.Log.Printf("fetch=failed sha256=%s error=%q", art.SHA256, err.Error())
		fmt.Fprintln(w, ReplyFailed)
		return err
	}
	f.Log.Printf("fetch=done sha256=%s version=%s", art.SHA256, art.Version)
	_, err = fmt.Fprintln(w, ReplyOK)
	return err
}

// Fetch downloads art from its pinned URL into Dir/<sha256>: first into
// <sha256>.part (0640), then renamed once the size and both hashes match.
func (f *Fetcher) Fetch(ctx context.Context, art minecraft.Artifact) (err error) {
	f.defaults()
	u, err := url.Parse(art.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("the pinned URL %q isn't https", art.URL)
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	final := filepath.Join(f.Dir, art.SHA256)
	part := final + ".part"
	if err := os.Remove(part); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			out.Close()
			os.Remove(part)
		}
	}()
	h1, h256 := sha1.New(), sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h1, h256), io.LimitReader(resp.Body, art.Size+1))
	if err != nil {
		return err
	}
	if n != art.Size {
		return fmt.Errorf("%d bytes, want %d", n, art.Size)
	}
	if got := hex.EncodeToString(h1.Sum(nil)); got != art.SHA1 {
		return fmt.Errorf("sha1 %s, want %s", got, art.SHA1)
	}
	if got := hex.EncodeToString(h256.Sum(nil)); got != art.SHA256 {
		return fmt.Errorf("sha256 %s, want %s", got, art.SHA256)
	}
	if err = out.Sync(); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	if err = os.Rename(part, final); err != nil {
		return err
	}
	return syncDir(f.Dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Ask is the controller's side: it asks the fetch service on socket for
// art, waits for its answer until ctx ends, and returns where the jar
// waits if the answer is ok.
func Ask(ctx context.Context, socket, dir string, art minecraft.Artifact) (string, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return "", fmt.Errorf("fetch service: %w", err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()
	if _, err := fmt.Fprintln(conn, art.SHA256); err != nil {
		return "", fmt.Errorf("fetch service: %w", err)
	}
	reply, err := bufio.NewReader(io.LimitReader(conn, 16)).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("fetch service: no answer: %w", err)
	}
	if reply != ReplyOK+"\n" {
		return "", fmt.Errorf("fetch service: %q", clip(reply))
	}
	return filepath.Join(dir, art.SHA256), nil
}

func clip(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}
