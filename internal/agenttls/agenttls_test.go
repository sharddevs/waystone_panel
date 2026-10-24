// SPDX-License-Identifier: AGPL-3.0-only

package agenttls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustGen(t *testing.T) tls.Certificate {
	t.Helper()
	c, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// selfSigned makes a certificate for any key type, to try the shapes the
// server must refuse.
func selfSigned(t *testing.T, key any, pub any) tls.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// handshake runs one TLS handshake over loopback TCP and returns both
// sides' errors and the server's view.
func handshake(t *testing.T, server, client *tls.Config) (serr, cerr error, state tls.ConnectionState) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	type result struct {
		err   error
		state tls.ConnectionState
	}
	done := make(chan result, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			done <- result{err: err}
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		sc := tls.Server(c, server)
		err = sc.Handshake()
		if err == nil {
			// TLS 1.3 checks the client certificate after the server's
			// Finished; reading the client's first byte waits for that.
			_, err = sc.Read(make([]byte, 1))
		}
		if err == nil {
			// One byte back, so the client also reads whatever the
			// server sent after the handshake (session tickets).
			_, err = sc.Write([]byte{1})
		}
		done <- result{err: err, state: sc.ConnectionState()}
	}()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	cc := tls.Client(c, client)
	cerr = cc.Handshake()
	if cerr == nil {
		_, cerr = cc.Write([]byte{1})
	}
	if cerr == nil {
		_, cerr = cc.Read(make([]byte, 1))
	}
	if cerr != nil {
		c.Close()
	}
	r := <-done
	return r.err, cerr, r.state
}

func TestPinRoundTripAndParse(t *testing.T) {
	c := mustGen(t)
	p := PinOf(c.Leaf)
	got, err := ParsePin(p.String())
	if err != nil || got != p {
		t.Fatalf("ParsePin(%s) = %v, %v", p, got, err)
	}
	for _, bad := range []string{"", p.String()[:63], strings.ToUpper(p.String()), p.String()[:62] + "zz", p.String() + "0"} {
		if _, err := ParsePin(bad); err == nil {
			t.Errorf("ParsePin(%q) accepted", bad)
		}
	}
}

func TestLoadOrCreate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	c1, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if PinOf(c1.Leaf) != PinOf(c2.Leaf) {
		t.Fatal("pin changed across a reload")
	}
	for name, want := range map[string]os.FileMode{KeyFile: 0o600, CertFile: 0o644, ".": 0o700} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: mode %v, %v; want %v", name, fi.Mode().Perm(), err, want)
		}
	}
	// One file missing is never a new key.
	if err := os.Remove(filepath.Join(dir, CertFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("made a new key with the old one still there")
	}
	if _, err := os.Stat(filepath.Join(dir, CertFile)); err == nil {
		t.Fatal("wrote a certificate")
	}
}

func TestClientServerPinned(t *testing.T) {
	srv, cli := mustGen(t), mustGen(t)
	serr, cerr, st := handshake(t, ServerConfig(srv), ClientConfig(cli, PinOf(srv.Leaf)))
	if serr != nil || cerr != nil {
		t.Fatalf("handshake: server %v, client %v", serr, cerr)
	}
	if st.Version != tls.VersionTLS13 {
		t.Fatalf("version %x", st.Version)
	}
	p, err := PeerPin(st)
	if err != nil || p != PinOf(cli.Leaf) {
		t.Fatalf("server sees pin %v, %v", p, err)
	}
}

func TestClientRefusesWrongPin(t *testing.T) {
	srv, cli, other := mustGen(t), mustGen(t), mustGen(t)
	_, cerr, _ := handshake(t, ServerConfig(srv), ClientConfig(cli, PinOf(other.Leaf)))
	if cerr == nil || !strings.Contains(cerr.Error(), "not the pinned one") {
		t.Fatalf("client accepted a wrong pin: %v", cerr)
	}
}

func TestServerRefusesShapes(t *testing.T) {
	srv := mustGen(t)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	a, b := mustGen(t), mustGen(t)
	two := tls.Certificate{Certificate: [][]byte{a.Certificate[0], b.Certificate[0]}, PrivateKey: a.PrivateKey}

	cases := map[string][]tls.Certificate{
		"none": nil,
		"two":  {two},
		"p384": {selfSigned(t, p384, &p384.PublicKey)},
		"rsa":  {selfSigned(t, rsaKey, &rsaKey.PublicKey)},
	}
	for name, certs := range cases {
		cfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: certs, InsecureSkipVerify: true}
		serr, _, _ := handshake(t, ServerConfig(srv), cfg)
		if serr == nil {
			t.Errorf("%s: server accepted", name)
		}
	}
}

func TestServerRefusesTLS12(t *testing.T) {
	srv, cli := mustGen(t), mustGen(t)
	cfg := ClientConfig(cli, PinOf(srv.Leaf))
	cfg.MinVersion, cfg.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
	if serr, _, _ := handshake(t, ServerConfig(srv), cfg); serr == nil {
		t.Fatal("server accepted TLS 1.2")
	}
}

// 6M2: even a client that keeps a session cache never resumes.
func TestNoResumption(t *testing.T) {
	srv, cli := mustGen(t), mustGen(t)
	scfg := ServerConfig(srv)
	ccfg := ClientConfig(cli, PinOf(srv.Leaf))
	ccfg.ClientSessionCache = tls.NewLRUClientSessionCache(4)
	// The cache is keyed by server name, else by address, and each
	// handshake here has a new port: one fixed name lets a ticket be
	// offered at all.
	ccfg.ServerName = "controller"
	for i := range 3 {
		serr, cerr, st := handshake(t, scfg, ccfg)
		if serr != nil || cerr != nil {
			t.Fatalf("handshake %d: %v %v", i, serr, cerr)
		}
		if st.DidResume {
			t.Fatalf("handshake %d resumed", i)
		}
	}
}

// 6a-M1: a key directory or file that is a symlink, too open, or of the
// wrong type is refused, never followed or repaired.
func TestKeyDirRefuses(t *testing.T) {
	good := filepath.Join(t.TempDir(), "tls")
	if _, err := LoadOrCreate(good); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "tls")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(link); err == nil {
		t.Error("loaded through a symlinked directory")
	}
	if _, err := LoadOrCreate(link); err == nil {
		t.Error("used a symlinked directory")
	}

	// A symlinked key, even to a real key.
	dir := filepath.Join(t.TempDir(), "tls")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.Symlink(filepath.Join(good, KeyFile), filepath.Join(dir, KeyFile))
	os.Symlink(filepath.Join(good, CertFile), filepath.Join(dir, CertFile))
	if _, err := Load(dir); err == nil {
		t.Error("loaded a symlinked key")
	}

	// Too open, and never made private behind the caller's back.
	if err := os.Chmod(filepath.Join(good, KeyFile), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(good); err == nil {
		t.Error("loaded a group-readable key")
	}
	if fi, _ := os.Stat(filepath.Join(good, KeyFile)); fi.Mode().Perm() != 0o640 {
		t.Error("repaired the key's mode")
	}
	os.Chmod(filepath.Join(good, KeyFile), 0o600)
	if err := os.Chmod(filepath.Join(good, CertFile), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(good); err == nil {
		t.Error("loaded a world-writable certificate")
	}
	os.Chmod(filepath.Join(good, CertFile), 0o644)
	if err := os.Chmod(good, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(good); err == nil {
		t.Error("used a group-readable directory")
	}
	os.Chmod(good, 0o700)
	if _, err := Load(good); err != nil {
		t.Fatalf("good key after the checks: %v", err)
	}

	// Someone else's directory.
	d, err := OpenKeyDir(good, false, os.Geteuid()+1)
	if err == nil {
		d.Close()
		t.Error("opened a directory owned by another uid")
	}
}
