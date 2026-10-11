//go:build devharness && liveintegration

package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sharddevs/waystone_panel/internal/helper"
	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/lifecycle"
)

func TestLiveStaleWrite(t *testing.T) {
	const serverID = "a3600878-96e7-426d-b8fd-3c550a178b9f"
	const name = "server.properties"

	runtime := lifecycle.HelperRuntime{
		Provisioner: helper.Client{
			SocketPath: helper.DefaultProvisionerSocketPath,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	newOperationID := func() string {
		t.Helper()

		id, err := ids.NewV7()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	original, err := runtime.ReadFile(ctx, serverID, newOperationID(), name)
	if err != nil {
		t.Fatal(err)
	}
	if !original.Exists {
		t.Fatal("expected existing server.properties")
	}

	updated := append([]byte(nil), original.Content...)
	updated = append(updated, []byte("# Waystone concurrency test\n")...)

	if err := runtime.WriteFile(
		ctx, serverID, newOperationID(), name, updated, original,
	); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	err = runtime.WriteFile(
		ctx, serverID, newOperationID(), name,
		[]byte("motd=stale write\n"), original,
	)
	if !errors.Is(err, lifecycle.ErrChanged) {
		t.Errorf("stale write: got %v, want ErrChanged", err)
	}

	current, err := runtime.ReadFile(ctx, serverID, newOperationID(), name)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(current.Content, updated) {
		t.Fatal("stale write altered the file")
	}

	if err := runtime.WriteFile(
		ctx, serverID, newOperationID(), name,
		original.Content, current,
	); err != nil {
		t.Fatalf("restore original content: %v", err)
	}
}
