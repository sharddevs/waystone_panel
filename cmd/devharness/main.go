//go:build devharness

// Command devharness drives a game server through the real runtime and
// helper, for stage 3 only. It is compiled only with -tags devharness, so
// no release build can contain it, and it is deleted in stage 4 when the
// controller drives the agent.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sharddevs/waystone_panel/internal/helper"
	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/lifecycle"
)

func main() {
	socket := flag.String("socket", helper.DefaultSocketPath, "helper socket")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: devharness [-socket path] start|stop|status <server-id>")
	}
	flag.Parse()

	args := flag.Args()

	if len(args) != 2 {
		flag.Usage()
		os.Exit(2)
	}

	action := args[0]
	serverID := args[1]
	operationID, err := ids.NewV7()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to generate operation ID: %v\n", err)
		os.Exit(1)
	}
	var runtime lifecycle.Runtime = lifecycle.HelperRuntime{
		Client: helper.Client{
			SocketPath: *socket,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var actionErr error

	switch action {
	case "start":
		actionErr = runtime.Start(ctx, serverID, operationID)

	case "stop":
		actionErr = runtime.Stop(ctx, serverID, operationID)
	case "status":
		var status lifecycle.Status
		status, actionErr = runtime.Status(ctx, serverID, operationID)

		if actionErr == nil {
			fmt.Printf("state: %s\n", status.State)
			fmt.Printf("native: %s\n", status.Native)
		}
	default:
		flag.Usage()
		os.Exit(2)
	}
	if actionErr != nil {
		fmt.Fprintf(os.Stderr, "op %s: %v\n", operationID, actionErr)
		os.Exit(1)
	}

	fmt.Printf("op %s: ok\n", operationID)
}
