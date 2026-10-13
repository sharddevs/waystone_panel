// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

// These tests need no database. The database contract is in the
// *_integration_test.go files (build tag "integration").

func TestValidateActor(t *testing.T) {
	const node = "0199e1a0-0000-7000-8000-0000000000a1"
	good := []Actor{
		{Type: ActorLocalUser, ID: "1000", Name: "trevor"},
		{Type: ActorLocalUser, ID: "0", Name: "root"},
		{Type: ActorLocalUser, ID: "4294967295", Name: "max"},
		{Type: ActorNode, ID: node, Name: "dev node"},
		{Type: ActorLocalUser, ID: "1000", Name: strings.Repeat("é", 50)}, // 100 bytes
	}
	for _, a := range good {
		if err := ValidateActor(a); err != nil {
			t.Errorf("ValidateActor(%+v) = %v, want nil", a, err)
		}
	}
	bad := []Actor{
		{Type: "", ID: "1000", Name: "x"},
		{Type: "user", ID: "1000", Name: "x"},
		{Type: ActorLocalUser, ID: "", Name: "x"},
		{Type: ActorLocalUser, ID: "01000", Name: "x"},
		{Type: ActorLocalUser, ID: "-1", Name: "x"},
		{Type: ActorLocalUser, ID: "+1", Name: "x"},
		{Type: ActorLocalUser, ID: "4294967296", Name: "x"},
		{Type: ActorLocalUser, ID: "10 00", Name: "x"},
		{Type: ActorLocalUser, ID: "trevor", Name: "x"},
		{Type: ActorNode, ID: "1000", Name: "x"},
		{Type: ActorNode, ID: strings.ToUpper(node), Name: "x"},
		{Type: ActorLocalUser, ID: "1000", Name: ""},
		{Type: ActorLocalUser, ID: "1000", Name: strings.Repeat("a", 101)},
		{Type: ActorLocalUser, ID: "1000", Name: "bad\nname"},
		{Type: ActorLocalUser, ID: "1000", Name: "bad\x00name"},
		{Type: ActorLocalUser, ID: "1000", Name: "\xff"},
	}
	for _, a := range bad {
		err := ValidateActor(a)
		if !errors.Is(err, ErrInvalidActor) {
			t.Errorf("ValidateActor(%+v) = %v, want ErrInvalidActor", a, err)
		}
	}
}

func TestValidIdempotencyKey(t *testing.T) {
	for _, k := range []string{"a", "retry-1", "A_b-C_9", strings.Repeat("k", 64)} {
		if !ValidIdempotencyKey(k) {
			t.Errorf("ValidIdempotencyKey(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"", " ", "has space", "semi;colon", "ü", strings.Repeat("k", 65), "a\n"} {
		if ValidIdempotencyKey(k) {
			t.Errorf("ValidIdempotencyKey(%q) = true, want false", k)
		}
	}
}

func TestInProgressErrorIs(t *testing.T) {
	var err error = &InProgressError{OperationID: "x", State: StateUncertain}
	if !errors.Is(err, ErrOperationInProgress) {
		t.Fatal("errors.Is(InProgressError, ErrOperationInProgress) = false")
	}
	var ipe *InProgressError
	if !errors.As(err, &ipe) || ipe.State != StateUncertain {
		t.Fatal("errors.As lost the blocking state")
	}
}

// The migration files are embedded and goose can find them.
func TestMigrationsEmbedded(t *testing.T) {
	data, err := fs.ReadFile(Migrations(), "00001_foundation.sql")
	if err != nil {
		t.Fatalf("00001_foundation.sql not embedded: %v", err)
	}
	if !strings.Contains(string(data), "-- +goose Up") {
		t.Fatal("00001_foundation.sql has no '-- +goose Up' marker")
	}
}

// InTenantTx refuses a malformed actor before it touches the database,
// so a nil pool is never reached.
func TestInTenantTxRejectsBadActorFirst(t *testing.T) {
	s := &Store{}
	err := s.InTenantTx(t.Context(), Actor{Type: "bogus"}, nil)
	if !errors.Is(err, ErrInvalidActor) {
		t.Fatalf("InTenantTx(bad actor) = %v, want ErrInvalidActor", err)
	}
}
