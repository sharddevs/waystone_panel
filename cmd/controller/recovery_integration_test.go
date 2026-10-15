// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration && linux

package main

// I7, F2 at the controller (stage 4c): run marks every running operation
// uncertain before it serves anything. Like the other integration tests,
// this wipes waystone_test.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sharddevs/waystone_panel/internal/store"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type lockedBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuilder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuilder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestRunRecoversBeforeServing(t *testing.T) {
	ctx := context.Background()
	ownerDSN := envOr("WAYSTONE_TEST_OWNER_DSN", "host=/var/run/postgresql dbname=waystone_test user=waystone_owner")
	appDSN := envOr("WAYSTONE_TEST_APP_DSN", "host=/var/run/postgresql dbname=waystone_test user=waystone_app")
	if !strings.Contains(ownerDSN, "waystone_test") {
		t.Fatalf("refusing to wipe a database that isn't waystone_test: %q", ownerDSN)
	}
	owner, err := pgx.Connect(ctx, ownerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(ctx)
	const (
		tenant  = "0199e1a0-0000-7000-8000-00000000000a"
		node    = "0199e1a0-0000-7000-8000-0000000000a1"
		server  = "0199e1a0-0000-7000-8000-000000000a01"
		running = "0199e1a0-0000-7000-8000-0000000c0001"
	)
	for _, q := range []string{
		`DROP SCHEMA IF EXISTS waystone CASCADE`,
		`DROP SCHEMA IF EXISTS waystone_meta CASCADE`,
		`CREATE SCHEMA waystone`,
		`CREATE SCHEMA waystone_meta`,
	} {
		if _, err := owner.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Migrate(ctx, ownerDSN); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO waystone.tenants (tenant_id, name) VALUES ('` + tenant + `', 'A')`,
		`INSERT INTO waystone.nodes (tenant_id, node_id, name) VALUES ('` + tenant + `', '` + node + `', 'n')`,
		`INSERT INTO waystone.servers (tenant_id, server_id, node_id, name, profile, runtime)
			VALUES ('` + tenant + `', '` + server + `', '` + node + `', 's', 'minecraft-java', 'systemd')`,
		`INSERT INTO waystone.operations (tenant_id, operation_id, resource_type, resource_id, node_id, action, state,
			actor_type, actor_id, actor_name, requested_at, started_at, claimed_epoch)
			VALUES ('` + tenant + `', '` + running + `', 'server', '` + server + `', '` + node + `', 'server.start', 'running',
			'local_user', '1000', 'a', now(), now(), 1)`,
	} {
		if _, err := owner.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	stderr := &lockedBuilder{}
	done := make(chan int, 1)
	dir := t.TempDir()
	go func() {
		done <- run([]string{"-db", appDSN, "-api-socket", filepath.Join(dir, "api.sock"), "-web-socket", filepath.Join(dir, "web.sock"),
			"-public-url", "https://panel.example.com", "-proxy-uid", "4242"}, stderr)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stderr.String(), "listening on") {
		if time.Now().After(deadline) {
			t.Fatalf("controller didn't start: %s", stderr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 0 {
		t.Errorf("run = %d: %s", code, stderr)
	}

	out := stderr.String()
	rec := strings.Index(out, "startup recovery: 1 running operation(s) marked uncertain")
	if rec < 0 || rec > strings.Index(out, "listening on") {
		t.Errorf("recovery missing or after serving:\n%s", out)
	}
	var state, code string
	if err := owner.QueryRow(ctx, `SELECT state, error_code FROM waystone.operations WHERE operation_id = $1`, running).
		Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "uncertain" || code != "controller_restarted" {
		t.Errorf("op = %s %s, want uncertain controller_restarted", state, code)
	}
}
