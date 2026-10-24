// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"io"
	"strings"
	"testing"
)

const testNode = "0199e1a0-0000-7000-8000-0000000000a1"

// Bad flags, and running as root, exit 2 before anything connects.
func TestRunRefuses(t *testing.T) {
	good := []string{"-controller", "/tmp/a.sock", "-controller-uid", "1000", "-node", testNode}
	cases := map[string]struct {
		args []string
		uid  int
	}{
		"root":              {good, 0},
		"unknown flag":      {append([]string{"-nope"}, good...), 995},
		"extra argument":    {append(append([]string{}, good...), "extra"), 995},
		"no controller":     {[]string{"-controller-uid", "1000", "-node", testNode}, 995},
		"no controller uid": {[]string{"-controller", "/tmp/a.sock", "-node", testNode}, 995},
		"bad node":          {[]string{"-controller", "/tmp/a.sock", "-controller-uid", "1000", "-node", "node-1"}, 995},
		"uppercase node":    {[]string{"-controller", "/tmp/a.sock", "-controller-uid", "1000", "-node", strings.ToUpper(testNode)}, 995},
		"empty helper":      {append(append([]string{}, good...), "-helper", ""), 995},
	}
	for name, c := range cases {
		if code := run(c.args, c.uid, io.Discard); code != 2 {
			t.Errorf("%s: run = %d, want 2", name, code)
		}
	}
	if _, err := parse(good, 995, io.Discard); err != nil {
		t.Errorf("good flags: %v", err)
	}
}

// Remote mode (6a): -controller-addr and -controller-pin, never mixed with
// the local flags.
func TestParseRemote(t *testing.T) {
	pin := strings.Repeat("ab", 32)
	good := []string{"-controller-addr", "10.0.20.80:7443", "-controller-pin", pin, "-node", testNode}
	cfg, err := parse(good, 995, io.Discard)
	if err != nil || !cfg.remote() || cfg.controllerAddr.String() != "10.0.20.80:7443" || cfg.controllerPin.String() != pin || cfg.tlsDir != defaultTLSDir {
		t.Fatalf("parse = %+v, %v", cfg, err)
	}
	for name, args := range map[string][]string{
		"mixed":     append(append([]string{}, good...), "-controller", "/tmp/a.sock"),
		"mixed uid": append(append([]string{}, good...), "-controller-uid", "1000"),
		"no pin":    {"-controller-addr", "10.0.20.80:7443", "-node", testNode},
		"no addr":   {"-controller-pin", pin, "-node", testNode},
		"host name": {"-controller-addr", "panel.example.com:7443", "-controller-pin", pin, "-node", testNode},
		"no port":   {"-controller-addr", "10.0.20.80", "-controller-pin", pin, "-node", testNode},
		"bad pin":   {"-controller-addr", "10.0.20.80:7443", "-controller-pin", strings.ToUpper(pin), "-node", testNode},
		"no tls":    append(append([]string{}, good...), "-tls-dir", ""),
		"nothing":   {"-node", testNode},
	} {
		if _, err := parse(args, 995, io.Discard); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Remote mode without a key is a configuration error, not a new key.
	args := append(append([]string{}, good...), "-tls-dir", t.TempDir())
	if code := run(args, 995, io.Discard); code != 2 {
		t.Errorf("no key: run = %d, want 2", code)
	}
}
