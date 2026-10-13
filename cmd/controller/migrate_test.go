// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunMigrate(t *testing.T) {
	failing := func(context.Context, string) error { return errors.New("boom") }
	tests := []struct {
		name       string
		args       []string
		migrate    func(context.Context, string) error
		wantCode   int
		wantDSN    string
		wantStdout string
		wantStderr string
	}{
		{name: "ok", args: []string{"-db", "dbname=waystone"}, wantCode: 0,
			wantDSN: "dbname=waystone", wantStdout: "migrations applied"},
		{name: "missing -db", args: nil, wantCode: 2},
		{name: "empty -db", args: []string{"-db", ""}, wantCode: 2},
		{name: "unknown flag", args: []string{"-db", "x", "-force"}, wantCode: 2},
		{name: "extra argument", args: []string{"-db", "x", "up"}, wantCode: 2},
		{name: "migrate fails", args: []string{"-db", "x"}, migrate: failing, wantCode: 1, wantStderr: "boom"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotDSN string
			calls := 0
			migrate := tc.migrate
			if migrate == nil {
				migrate = func(_ context.Context, dsn string) error { calls++; gotDSN = dsn; return nil }
			}
			var stdout, stderr bytes.Buffer
			code := runMigrate(tc.args, &stdout, &stderr, migrate)
			if code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr %q)", code, tc.wantCode, stderr.String())
			}
			if tc.wantCode == 2 && calls != 0 {
				t.Error("migrate was called despite a usage error")
			}
			if tc.wantDSN != "" && gotDSN != tc.wantDSN {
				t.Errorf("dsn = %q, want %q", gotDSN, tc.wantDSN)
			}
			if !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tc.wantStdout)
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.wantStderr)
			}
			if tc.wantCode != 0 && stdout.Len() != 0 {
				t.Errorf("stdout = %q on failure, want empty", stdout.String())
			}
		})
	}
}
