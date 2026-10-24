// SPDX-License-Identifier: AGPL-3.0-only

// Package agenttls is the TLS of the agent link over TCP (stage 6a, D1):
// each side has its own ECDSA P-256 key and a self-signed certificate,
// and trusts the other by a pin, the SHA-256 of the certificate's
// SubjectPublicKeyInfo. There is no CA. Names, chains and expiry are
// never looked at; the pin is the whole check.
//
// Private keys never leave the machine that made them.
package agenttls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Pin is the SHA-256 of a certificate's SubjectPublicKeyInfo.
type Pin [32]byte

// String is the pin as 64 lowercase hex characters.
func (p Pin) String() string { return hex.EncodeToString(p[:]) }

// ParsePin reads exactly 64 lowercase hex characters.
func ParsePin(s string) (Pin, error) {
	var p Pin
	if len(s) != 64 {
		return p, errors.New("a pin is 64 hex characters")
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return p, errors.New("a pin is lowercase hex")
		}
	}
	_, err := hex.Decode(p[:], []byte(s))
	return p, err
}

// PinOf is cert's pin.
func PinOf(cert *x509.Certificate) Pin {
	return sha256.Sum256(cert.RawSubjectPublicKeyInfo)
}

// The files in a key directory.
const (
	KeyFile  = "key.pem"
	CertFile = "cert.pem"
)

// Generate makes a new P-256 key and its self-signed certificate. The
// certificate is valid for 100 years only because x509 needs some
// validity; nothing ever checks it.
func Generate() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "waystone"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(100, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// Encode returns the key and certificate as PEM.
func Encode(c tls.Certificate) (keyPEM, certPEM []byte, err error) {
	der, err := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]}), nil
}

// Load reads dir's key and certificate. dir and both files must belong
// to the user running this, as KeyDir checks.
func Load(dir string) (tls.Certificate, error) {
	d, err := OpenKeyDir(dir, false, os.Geteuid())
	if err != nil {
		return tls.Certificate{}, err
	}
	defer d.Close()
	return d.Load()
}

// LoadOrCreate loads dir's key, or makes one if neither file exists (see
// KeyDir.LoadOrCreate). dir is made 0700 if it doesn't exist; it and the
// files must belong to the user running this.
func LoadOrCreate(dir string) (tls.Certificate, error) {
	d, err := OpenKeyDir(dir, true, os.Geteuid())
	if err != nil {
		return tls.Certificate{}, err
	}
	defer d.Close()
	return d.LoadOrCreate()
}

// WriteFileAtomic writes data to a new file next to path (created with
// O_EXCL), gives it mode and, unless uid or gid is -1, that owner through
// the open file, syncs it, and renames it over path, so a reader sees the
// old file or the new one, never half of one. It is for directories only
// root can write, such as /etc; key directories use KeyDir.
func WriteFileAtomic(path string, data []byte, mode os.FileMode, uid, gid int) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // a no-op once renamed
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if uid != -1 || gid != -1 {
		if err := f.Chown(uid, gid); err != nil {
			f.Close()
			return err
		}
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func isP256(c *x509.Certificate) bool {
	k, ok := c.PublicKey.(*ecdsa.PublicKey)
	return ok && k.Curve == elliptic.P256()
}

// peerCert is the one certificate the peer must present (6M1), with a
// P-256 key.
func peerCert(cs tls.ConnectionState) (*x509.Certificate, error) {
	if len(cs.PeerCertificates) != 1 { // pin: exactly one
		return nil, errors.New("the peer must present exactly one certificate")
	}
	if !isP256(cs.PeerCertificates[0]) { // pin: P-256 only
		return nil, errors.New("the peer's key must be P-256")
	}
	return cs.PeerCertificates[0], nil
}

// PeerPin is the pin of the connection's peer, after the checks that
// ServerConfig and ClientConfig make.
func PeerPin(cs tls.ConnectionState) (Pin, error) {
	c, err := peerCert(cs)
	if err != nil {
		return Pin{}, err
	}
	return PinOf(c), nil
}

// ServerConfig is the controller's listener: TLS 1.3, a client
// certificate required, and no resumption (6M2: Go skips
// VerifyPeerCertificate on a resumed connection; VerifyConnection runs on
// every one). The handshake checks only the certificate's shape. An
// unknown key passes on purpose, so it can enroll; which calls it may
// make is the authorizer's decision.
func ServerConfig(own tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		Certificates:           []tls.Certificate{own},
		ClientAuth:             tls.RequireAnyClientCert,
		SessionTicketsDisabled: true, // pin: no resumption
		VerifyConnection: func(cs tls.ConnectionState) error {
			_, err := peerCert(cs)
			return err
		},
	}
}

// ClientConfig is the agent's side: TLS 1.3, its own certificate, and the
// controller's pin as the only trust. InsecureSkipVerify turns off Go's
// chain and name checks, which mean nothing here; VerifyConnection
// replaces them with the pin. There is no session cache, so no
// resumption either.
func ClientConfig(own tls.Certificate, controller Pin) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{own},
		InsecureSkipVerify: true, // the pin below is the check
		VerifyConnection: func(cs tls.ConnectionState) error {
			p, err := PeerPin(cs)
			if err != nil {
				return err
			}
			if subtle.ConstantTimeCompare(p[:], controller[:]) != 1 { // pin: must match
				return errors.New("the controller's key is not the pinned one")
			}
			return nil
		},
	}
}
