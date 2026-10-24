// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

// Stage 7b (notes/stage7-scope.md D2): a console command's line is set on
// server.command and nothing else, and goes only to the node running it,
// while it runs.

import (
	"context"
	"errors"
	"testing"
)

func (e *env) markRunning(t *testing.T, server string) {
	t.Helper()
	mustExec(t, e.owner, `UPDATE waystone.servers SET process_state = 'running', process_since = now(), process_observed_at = now()
		WHERE server_id = $1`, server)
}

func TestCreateCommandInput(t *testing.T) {
	e := setup(t)
	e.mustHello(t, nodeActA)
	e.markRunning(t, serverA1)
	for name, in := range map[string]CreateOperationInput{
		"no line":        {ServerID: serverA1, Action: ActionCommand},
		"bad line":       {ServerID: serverA1, Action: ActionCommand, Command: "/stop"},
		"two lines":      {ServerID: serverA1, Action: ActionCommand, Command: "say a\nstop"},
		"a stop's line":  {ServerID: serverA1, Action: ActionStop, Command: "say hi"},
		"a start's line": {ServerID: serverA1, Action: ActionStart, Command: "say hi"},
	} {
		if _, _, err := e.create(adminA, in); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %v, want ErrInvalidInput", name, err)
		}
	}
	mustExec(t, e.owner, `UPDATE waystone.servers SET process_state = 'stopped' WHERE server_id = $1`, serverA1)
	if _, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionCommand, Command: "say hi"}); !errors.Is(err, ErrNotRunning) {
		t.Errorf("stopped: %v, want ErrNotRunning", err)
	}
}

// 7b, like 6M5 for installs.
func TestRunningCommand(t *testing.T) {
	e := setup(t)
	epoch := e.mustHello(t, nodeActA)
	e.markRunning(t, serverA1)
	op, _, err := e.create(adminA, CreateOperationInput{ServerID: serverA1, Action: ActionCommand, Command: "say hi é"})
	if err != nil {
		t.Fatal(err)
	}
	if op.Command != "say hi é" {
		t.Errorf("created %+v", op)
	}
	get := func(actor Actor, id string) (CommandSpec, error) {
		var spec CommandSpec
		err := e.store.InTenantTx(context.Background(), actor, func(ctx context.Context, tx *Tx) error {
			var err error
			spec, err = tx.RunningCommand(ctx, id)
			return err
		})
		return spec, err
	}
	if _, err := get(nodeActA, op.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("pending: %v", err)
	}
	if c, ok, err := e.claim(nodeActA, epoch); err != nil || !ok || c.OperationID != op.ID {
		t.Fatalf("claim %+v %v %v", c, ok, err)
	}
	if spec, err := get(nodeActA, op.ID); err != nil || spec != (CommandSpec{ServerID: serverA1, Command: "say hi é"}) {
		t.Errorf("spec %+v %v", spec, err)
	}
	if _, err := get(adminA, op.ID); !errors.Is(err, ErrNotNode) {
		t.Errorf("a person: %v", err)
	}
	if _, err := get(nodeActA, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a bad ID: %v", err)
	}

	// Another node of the same tenant.
	other := Actor{Type: ActorNode, ID: remoteA, Name: "remote"}
	mustExec(t, e.owner, `INSERT INTO waystone.nodes (tenant_id, node_id, name, last_seen_at) VALUES ($1, $2, 'remote', now())`, tenantA, remoteA)
	mustExec(t, e.owner, `INSERT INTO waystone.memberships (tenant_id, actor_type, actor_id, role) VALUES ($1, 'node', $2, 'node')`, tenantA, remoteA)
	if _, err := get(other, op.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another node: %v", err)
	}
	// The server moved away under the running command.
	mustExec(t, e.owner, `UPDATE waystone.servers SET node_id = $1 WHERE server_id = $2`, remoteA, serverA1)
	if _, err := get(nodeActA, op.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("moved server: %v", err)
	}
	if _, err := get(other, op.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("moved server, asked by its new node: %v", err)
	}
	mustExec(t, e.owner, `UPDATE waystone.servers SET node_id = $1 WHERE server_id = $2`, nodeA, serverA1)

	// Finished: nothing any more.
	if err := e.complete(nodeActA, epoch, op.ID, succeeded); err != nil {
		t.Fatal(err)
	}
	if _, err := get(nodeActA, op.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("finished: %v", err)
	}
}
