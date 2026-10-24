// SPDX-License-Identifier: AGPL-3.0-only

// Package enrollbundle is the one-time enrollment bundle (stage 6a, D2):
// what an admin copies from the panel to a new machine. It holds the
// controller's agent listener address, the controller's key pin and the
// token's secret, as base64url JSON. It is shown once and never stored;
// the controller keeps only the secret's SHA-256.
package enrollbundle

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/sharddevs/waystone_panel/internal/agenttls"
)

// MaxEncodedBytes bounds what Decode reads.
const MaxEncodedBytes = 1024

// Bundle is a decoded bundle.
type Bundle struct {
	// Addr is the controller's agent listener: an IP literal and a port
	// (6H3), so the agent's unit can allow exactly that address.
	Addr netip.AddrPort
	// Pin is the controller's key pin.
	Pin agenttls.Pin
	// Secret is the token's secret.
	Secret [32]byte
}

type wire struct {
	V      int    `json:"v"`
	Addr   string `json:"addr"`
	Pin    string `json:"pin"`
	Secret string `json:"secret"`
}

// Encode returns the bundle as one line of base64url.
func Encode(b Bundle) (string, error) {
	if !b.Addr.IsValid() || b.Addr.Port() == 0 {
		return "", errors.New("enrollment bundle: no address")
	}
	j, err := json.Marshal(wire{V: 1, Addr: b.Addr.String(), Pin: b.Pin.String(), Secret: base64.RawURLEncoding.EncodeToString(b.Secret[:])})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(j), nil
}

// Decode reads a bundle, with spaces around it trimmed. Anything off is
// an error that says what, never what the secret was.
func Decode(s string) (Bundle, error) {
	s = strings.TrimSpace(s)
	if len(s) == 0 || len(s) > MaxEncodedBytes {
		return Bundle{}, errors.New("enrollment bundle: empty or too long")
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Bundle{}, errors.New("enrollment bundle: not base64url")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var w wire
	if err := dec.Decode(&w); err != nil || dec.More() {
		return Bundle{}, errors.New("enrollment bundle: not a bundle")
	}
	if w.V != 1 {
		return Bundle{}, fmt.Errorf("enrollment bundle: version %d, this agent reads version 1", w.V)
	}
	var b Bundle
	if b.Addr, err = netip.ParseAddrPort(w.Addr); err != nil || b.Addr.Port() == 0 || b.Addr.Addr().Zone() != "" {
		return Bundle{}, errors.New("enrollment bundle: the address must be an IP and a port")
	}
	if b.Pin, err = agenttls.ParsePin(w.Pin); err != nil {
		return Bundle{}, errors.New("enrollment bundle: bad controller pin")
	}
	sec, err := base64.RawURLEncoding.DecodeString(w.Secret)
	if err != nil || len(sec) != 32 {
		return Bundle{}, errors.New("enrollment bundle: bad secret")
	}
	copy(b.Secret[:], sec)
	return b, nil
}
