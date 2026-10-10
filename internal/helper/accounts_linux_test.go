//go:build linux

package helper

import (
	"errors"
	"testing"
)

func TestSystemAccountsLookup(t *testing.T) {
	a := SystemAccounts{}
	root, err := a.Lookup("root")
	if err != nil {
		t.Fatalf("Lookup(root): %v", err)
	}
	if root.Name != "root" || root.UID != 0 || root.GID != 0 {
		t.Errorf("Lookup(root) = %+v", root)
	}
	if _, err := a.Lookup("ws-000000000000"); !errors.Is(err, ErrNoAccount) {
		t.Errorf("missing account: err = %v, want ErrNoAccount", err)
	}
}
