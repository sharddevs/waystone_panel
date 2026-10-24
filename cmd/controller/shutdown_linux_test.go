// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/local"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	"github.com/sharddevs/waystone_panel/internal/agentlink"
	"github.com/sharddevs/waystone_panel/internal/store"
)

// R10 at the controller: an agent stream that hasn't said hello yet would
// hold GracefulStop until its 10 s hello deadline. shutdown ends it first,
// so everything stops well inside the budget. (No hello is sent, so the
// store is never used; the node lookup is a fake.)
func TestShutdownEndsAgentStreams(t *testing.T) {
	link, err := agentlink.New(agentlink.Config{
		Store:    new(store.Store),
		NodeID:   "0199e1a0-0000-7000-8000-0000000000a1",
		AgentUID: uint32(os.Getuid()),
		Version:  version,
		Nodes:    fakeNodes{},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agent.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	go link.Serve(l)

	cc, err := grpc.NewClient("unix://"+path, grpc.WithTransportCredentials(local.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	stream, err := agentv1.NewAgentLinkServiceClient(cc).Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = stream
	// Give the stream time to reach the server's handler.
	time.Sleep(200 * time.Millisecond)

	server := newServer(http.NotFoundHandler())
	start := time.Now()
	if !shutdown(server, nil, link, 3*time.Second, io.Discard) {
		t.Error("shutdown ran out of budget")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("shutdown took %s", took)
	}

}

// fakeNodes says every node is node A, not revoked.
type fakeNodes struct{}

func (fakeNodes) AgentNodeByPin(context.Context, [32]byte) (store.AgentNode, error) {
	return store.AgentNode{NodeID: "0199e1a0-0000-7000-8000-0000000000a1", Name: "a"}, nil
}

func (fakeNodes) AgentNodeByID(_ context.Context, id string) (store.AgentNode, error) {
	return store.AgentNode{NodeID: id, Name: "a"}, nil
}
