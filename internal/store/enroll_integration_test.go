// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// 6a-1: tokens, redemption, pins and revocation (migration 00008).

func (e *env) newEnrollment(t *testing.T, actor Actor, name string) (Enrollment, [32]byte) {
	t.Helper()
	var en Enrollment
	var secret [32]byte
	err := e.store.InTenantTx(context.Background(), actor, func(ctx context.Context, tx *Tx) error {
		var err error
		en, secret, err = tx.CreateEnrollment(ctx, name)
		return err
	})
	if err != nil {
		t.Fatalf("CreateEnrollment: %v", err)
	}
	return en, secret
}

func pinOf(b byte) [32]byte {
	var p [32]byte
	for i := range p {
		p[i] = b
	}
	return p
}

var testAudit = EnrollAudit{RequestID: "req-1", ClientIP: "192.0.2.7"}

func (e *env) enroll(t *testing.T, secret, pin [32]byte) Enrolled {
	t.Helper()
	r, err := e.store.EnrollNode(context.Background(), sha256.Sum256(secret[:]), pin, testAudit)
	if err != nil {
		t.Fatalf("EnrollNode: %v", err)
	}
	return r
}

func (e *env) auditRows(t *testing.T, action string) []struct{ actorType, outcome, reason string } {
	t.Helper()
	rows, err := e.owner.Query(context.Background(), `
		SELECT actor_type, outcome, reason FROM waystone.audit_events
		WHERE action = $1 ORDER BY at, event_id`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []struct{ actorType, outcome, reason string }
	for rows.Next() {
		var r struct{ actorType, outcome, reason string }
		if err := rows.Scan(&r.actorType, &r.outcome, &r.reason); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestEnrollmentCreateAndList(t *testing.T) {
	e := setup(t)
	en, secret := e.newEnrollment(t, adminA, "node c")
	if secret == ([32]byte{}) {
		t.Fatal("zero secret")
	}
	if got := en.ExpiresAt.Sub(en.CreatedAt); got != EnrollmentTTL {
		t.Fatalf("expiry %v after creation, want %v", got, EnrollmentTTL)
	}
	// Only the hash is stored.
	var stored []byte
	if err := e.owner.QueryRow(context.Background(), `SELECT secret_sha256 FROM waystone.node_enrollments`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if h := sha256.Sum256(secret[:]); string(stored) != string(h[:]) {
		t.Fatal("stored value is not the secret's SHA-256")
	}

	var list []Enrollment
	err := e.store.InTenantTx(context.Background(), adminA, func(ctx context.Context, tx *Tx) error {
		var err error
		list, err = tx.ListEnrollments(ctx)
		return err
	})
	if err != nil || len(list) != 1 || list[0].ID != en.ID || list[0].UsedAt != nil || list[0].CreatedBy != adminA {
		t.Fatalf("ListEnrollments = %+v, %v", list, err)
	}
	// Another tenant's admin sees none of it.
	err = e.store.InTenantTx(context.Background(), adminB, func(ctx context.Context, tx *Tx) error {
		var err error
		list, err = tx.ListEnrollments(ctx)
		return err
	})
	if err != nil || len(list) != 0 {
		t.Fatalf("tenant B sees %+v, %v", list, err)
	}

	for _, bad := range []string{"", "a\nb", string(make([]byte, 101))} {
		err := e.store.InTenantTx(context.Background(), adminA, func(ctx context.Context, tx *Tx) error {
			_, _, err := tx.CreateEnrollment(ctx, bad)
			return err
		})
		if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("name %q: %v, want ErrInvalidInput", bad, err)
		}
	}
}

func TestEnrollmentNotForNodes(t *testing.T) {
	e := setup(t)
	e.newEnrollment(t, adminA, "node c")
	err := e.store.InTenantTx(context.Background(), nodeActA, func(ctx context.Context, tx *Tx) error {
		_, _, err := tx.CreateEnrollment(ctx, "x")
		return err
	})
	if !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("node CreateEnrollment: %v, want ErrNotAdmin", err)
	}
	err = e.store.InTenantTx(context.Background(), nodeActA, func(ctx context.Context, tx *Tx) error {
		_, err := tx.ListEnrollments(ctx)
		return err
	})
	if !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("node ListEnrollments: %v, want ErrNotAdmin", err)
	}

	// Past Go: the policy hides every row from a node, and refuses its
	// insert.
	c := appConn(t)
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	mustExecTx(t, tx, `SELECT set_config('app.actor_type', 'node', true), set_config('app.actor_id', $1, true), set_config('app.tenant_id', $2, true)`, nodeA, tenantA)
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM waystone.node_enrollments`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("node sees %d enrollments (%v)", n, err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO waystone.node_enrollments (tenant_id, enrollment_id, name, secret_sha256, created_by_type, created_by_id, created_by_name)
		VALUES ($1, gen_random_uuid(), 'x', $2, 'node', $3, 'n')`, tenantA, make([]byte, 32), nodeA)
	if sqlState(err) != "42501" {
		t.Fatalf("node insert: %v, want an RLS refusal", err)
	}
}

func TestEnrollmentAppCannotPickTimesOrRedeem(t *testing.T) {
	e := setup(t)
	en, _ := e.newEnrollment(t, adminA, "node c")
	c := appConn(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO waystone.node_enrollments (tenant_id, enrollment_id, name, secret_sha256, created_by_type, created_by_id, created_by_name, expires_at)
			VALUES ('` + tenantA + `', gen_random_uuid(), 'x', '\x` + hex32 + `', 'local_user', '1000', 'a', now() + interval '1 year')`,
		`INSERT INTO waystone.node_enrollments (tenant_id, enrollment_id, name, secret_sha256, created_by_type, created_by_id, created_by_name, used_at)
			VALUES ('` + tenantA + `', gen_random_uuid(), 'x', '\x` + hex32 + `', 'local_user', '1000', 'a', now())`,
		`UPDATE waystone.node_enrollments SET used_at = now() WHERE enrollment_id = '` + en.ID + `'`,
		`DELETE FROM waystone.node_enrollments`,
		`SELECT secret_sha256 FROM waystone.node_enrollments`,
	} {
		tx, err := c.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		mustExecTx(t, tx, `SELECT set_config('app.actor_type', 'local_user', true), set_config('app.actor_id', '1000', true), set_config('app.tenant_id', $1, true)`, tenantA)
		_, err = tx.Exec(ctx, q)
		tx.Rollback(ctx)
		if sqlState(err) != "42501" {
			t.Errorf("%s: %v, want permission denied", q, err)
		}
	}
}

const hex32 = "0101010101010101010101010101010101010101010101010101010101010101"

func TestEnrollNodeOutcomes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	// Unknown: nothing written.
	if r := e.enroll(t, pinOf(9), pinOf(1)); r.Outcome != EnrollUnknown {
		t.Fatalf("unknown secret: %+v", r)
	}
	if rows := e.auditRows(t, "node.enroll"); len(rows) != 0 {
		t.Fatalf("unknown secret audited: %+v", rows)
	}

	en, secret := e.newEnrollment(t, adminA, "node c")
	r := e.enroll(t, secret, pinOf(1))
	if r.Outcome != EnrollEnrolled || r.TenantID != tenantA || r.Name != "node c" || r.NodeID == "" {
		t.Fatalf("enroll: %+v", r)
	}
	n, err := e.store.AgentNodeByPin(ctx, pinOf(1))
	if err != nil || n.NodeID != r.NodeID || n.TenantID != tenantA || n.Name != "node c" || n.Revoked {
		t.Fatalf("AgentNodeByPin = %+v, %v", n, err)
	}
	// The new node can act in its tenant.
	err = e.store.InTenantTx(ctx, n.Actor(), func(ctx context.Context, tx *Tx) error {
		if tx.TenantID() != tenantA || tx.Role() != RoleNode {
			t.Errorf("node tx tenant %s role %s", tx.TenantID(), tx.Role())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("new node InTenantTx: %v", err)
	}
	var usedNode string
	if err := e.owner.QueryRow(ctx, `SELECT node_id::text FROM waystone.node_enrollments WHERE enrollment_id = $1 AND used_at IS NOT NULL`, en.ID).Scan(&usedNode); err != nil || usedNode != r.NodeID {
		t.Fatalf("enrollment row: node %q, %v", usedNode, err)
	}

	// Used.
	if r := e.enroll(t, secret, pinOf(2)); r.Outcome != EnrollUsed || r.TenantID != tenantA || r.NodeID != "" {
		t.Fatalf("second use: %+v", r)
	}
	// Expired: the owner moves the clock back, never the app.
	_, secret2 := e.newEnrollment(t, adminA, "node d")
	mustExec(t, e.owner, `UPDATE waystone.node_enrollments SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour' WHERE name = 'node d'`)
	if r := e.enroll(t, secret2, pinOf(3)); r.Outcome != EnrollExpired {
		t.Fatalf("expired: %+v", r)
	}
	// Pin taken: the token is spent and no node is made.
	en3, secret3 := e.newEnrollment(t, adminA, "node e")
	before := count(t, e.owner, "nodes")
	if r := e.enroll(t, secret3, pinOf(1)); r.Outcome != EnrollPinTaken || r.NodeID != "" {
		t.Fatalf("pin taken: %+v", r)
	}
	if count(t, e.owner, "nodes") != before {
		t.Fatal("pin_taken made a node")
	}
	var spent bool
	if err := e.owner.QueryRow(ctx, `SELECT used_at IS NOT NULL AND node_id IS NULL FROM waystone.node_enrollments WHERE enrollment_id = $1`, en3.ID).Scan(&spent); err != nil || !spent {
		t.Fatalf("pin_taken token spent=%v, %v", spent, err)
	}
	if r := e.enroll(t, secret3, pinOf(4)); r.Outcome != EnrollUsed {
		t.Fatalf("token after pin_taken: %+v", r)
	}

	rows := e.auditRows(t, "node.enroll")
	want := []struct{ actorType, outcome, reason string }{
		{"local_user", "recorded", "enrolled"},
		{"enrollment", "denied", "used"},
		{"enrollment", "denied", "expired"},
		{"enrollment", "denied", "pin_taken"},
		{"enrollment", "denied", "used"},
	}
	if len(rows) != len(want) {
		t.Fatalf("audit rows %+v, want %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("audit row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
	var ip, reqID string
	if err := e.owner.QueryRow(ctx, `SELECT client_ip, request_id FROM waystone.audit_events WHERE action = 'node.enroll' LIMIT 1`).Scan(&ip, &reqID); err != nil || ip != testAudit.ClientIP || reqID != testAudit.RequestID {
		t.Fatalf("audit labels %q %q, %v", ip, reqID, err)
	}

	// Bad labels never reach the database.
	for _, a := range []EnrollAudit{{RequestID: ""}, {RequestID: "r", ClientIP: "2001:DB8::1"}, {RequestID: "a\nb"}} {
		if _, err := e.store.EnrollNode(ctx, sha256.Sum256(secret[:]), pinOf(5), a); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%+v: %v, want ErrInvalidInput", a, err)
		}
	}
}

// N14: one token redeemed 20 times at once makes exactly one node.
func TestEnrollNodeRace(t *testing.T) {
	e := setup(t)
	_, secret := e.newEnrollment(t, adminA, "node c")
	before := count(t, e.owner, "nodes")
	var wg sync.WaitGroup
	results := make([]Enrolled, 20)
	errs := make([]error, 20)
	for i := range results {
		wg.Go(func() {
			results[i], errs[i] = e.store.EnrollNode(context.Background(), sha256.Sum256(secret[:]), pinOf(byte(i+1)), testAudit)
		})
	}
	wg.Wait()
	enrolled := 0
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("redeem %d: %v", i, errs[i])
		}
		switch r.Outcome {
		case EnrollEnrolled:
			enrolled++
		case EnrollUsed:
		default:
			t.Fatalf("redeem %d: %+v", i, r)
		}
	}
	if enrolled != 1 || count(t, e.owner, "nodes") != before+1 {
		t.Fatalf("%d enrolled, %d nodes added", enrolled, count(t, e.owner, "nodes")-before)
	}
}

func TestRevokeNode(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	_, secret := e.newEnrollment(t, adminA, "node c")
	r := e.enroll(t, secret, pinOf(1))
	n, _ := e.store.AgentNodeByPin(ctx, pinOf(1))

	revoke := func(actor Actor, id string) (bool, error) {
		var already bool
		err := e.store.InTenantTx(ctx, actor, func(ctx context.Context, tx *Tx) error {
			var err error
			already, err = tx.RevokeNode(ctx, id, "req-r")
			return err
		})
		return already, err
	}

	if _, err := revoke(nodeActA, r.NodeID); !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("node revokes: %v", err)
	}
	if _, err := revoke(adminB, r.NodeID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other tenant revokes: %v", err)
	}
	if already, err := revoke(adminA, r.NodeID); err != nil || already {
		t.Fatalf("revoke: %v %v", already, err)
	}
	if already, err := revoke(adminA, r.NodeID); err != nil || !already {
		t.Fatalf("second revoke: %v %v", already, err)
	}
	if got, err := e.store.AgentNodeByPin(ctx, pinOf(1)); err != nil || !got.Revoked {
		t.Fatalf("after revoke: %+v %v", got, err)
	}
	if got, err := e.store.AgentNodeByID(ctx, r.NodeID); err != nil || !got.Revoked {
		t.Fatalf("by ID after revoke: %+v %v", got, err)
	}
	// The node can't act any more.
	err := e.store.InTenantTx(ctx, n.Actor(), func(context.Context, *Tx) error { return nil })
	if !errors.Is(err, ErrNoMembership) {
		t.Fatalf("revoked node InTenantTx: %v, want ErrNoMembership", err)
	}
	rows := e.auditRows(t, "node.revoke")
	if len(rows) != 1 || rows[0].outcome != "recorded" || rows[0].actorType != "local_user" {
		t.Fatalf("revoke audit: %+v", rows)
	}

	// Past Go: a node calling the function itself is refused.
	c := appConn(t)
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	mustExecTx(t, tx, `SELECT set_config('app.actor_type', 'node', true), set_config('app.actor_id', $1, true), set_config('app.tenant_id', $2, true)`, nodeA, tenantA)
	if _, err := tx.Exec(ctx, `SELECT waystone.revoke_node($1)`, nodeA); err == nil {
		t.Fatal("node ran revoke_node")
	}
}

func TestAgentNodeLookups(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.store.AgentNodeByPin(ctx, pinOf(7)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown pin: %v", err)
	}
	if _, err := e.store.AgentNodeByID(ctx, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad ID: %v", err)
	}
	n, err := e.store.AgentNodeByID(ctx, nodeA)
	if err != nil || n.TenantID != tenantA || n.Revoked {
		t.Fatalf("local node: %+v %v", n, err)
	}
}

func TestSetAgentInfo(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	set := func(actor Actor, v string, caps []string) error {
		return e.store.InTenantTx(ctx, actor, func(ctx context.Context, tx *Tx) error {
			return tx.SetAgentInfo(ctx, v, caps)
		})
	}
	if err := set(nodeActA, "0.1.0-alpha", []string{"systemd", "minecraft-java"}); err != nil {
		t.Fatal(err)
	}
	var v string
	var caps []string
	if err := e.owner.QueryRow(ctx, `SELECT agent_version, capabilities FROM waystone.nodes WHERE node_id = $1`, nodeA).Scan(&v, &caps); err != nil ||
		v != "0.1.0-alpha" || len(caps) != 2 || caps[1] != "minecraft-java" {
		t.Fatalf("stored %q %v, %v", v, caps, err)
	}
	if err := set(adminA, "x", nil); !errors.Is(err, ErrNotNode) {
		t.Fatalf("admin: %v", err)
	}
	for _, bad := range []struct {
		v    string
		caps []string
	}{
		{"", nil}, {"a\x00", nil}, {"ok", []string{"UPPER"}}, {"ok", []string{""}},
		{"ok", []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}},
	} {
		if err := set(nodeActA, bad.v, bad.caps); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%q %v: %v", bad.v, bad.caps, err)
		}
	}

	// Past Go: the guard holds the same rules and the identity columns.
	c := appConn(t)
	for _, q := range []string{
		`UPDATE waystone.nodes SET capabilities = '{BAD}' WHERE node_id = '` + nodeA + `'`,
		`UPDATE waystone.nodes SET agent_version = E'a\n' WHERE node_id = '` + nodeA + `'`,
		`UPDATE waystone.nodes SET revoked_at = NULL WHERE node_id = '` + nodeA + `'`,
		`UPDATE waystone.nodes SET agent_spki_sha256 = '\x` + hex32 + `' WHERE node_id = '` + nodeA + `'`,
	} {
		tx, err := c.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		mustExecTx(t, tx, `SELECT set_config('app.actor_type', 'node', true), set_config('app.actor_id', $1, true), set_config('app.tenant_id', $2, true)`, nodeA, tenantA)
		_, err = tx.Exec(ctx, q)
		tx.Rollback(ctx)
		if err == nil {
			t.Errorf("%s: allowed", q)
		}
	}
}

func mustExecTx(t *testing.T, tx pgx.Tx, sql string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
