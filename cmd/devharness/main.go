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
	provisionSocket := flag.String(
		"provisioner-socket",
		helper.DefaultProvisionerSocketPath,
		"provisioner socket",
	)
	runtimeName := flag.String("runtime", "java25", "runtime for provisioning")
	heapMiB := flag.Uint("heap", 2048, "heap size in MiB")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: devharness [-socket path] [-provisioner-socket path] start|stop|status <server-id>")
		fmt.Fprintln(os.Stderr, "       devharness [-provisioner-socket path] [-runtime name] [-heap MiB] provision <server-id>")
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
		Provisioner: helper.Client{
			SocketPath: *provisionSocket,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var actionErr error

	switch action {
	case "provision":
		if uint64(*heapMiB) > uint64(^uint32(0)) {
			fmt.Fprintln(os.Stderr, "heap size exceeds uint32 range")
			os.Exit(2)
		}

		var changed bool
		changed, actionErr = runtime.Provision(
			ctx,
			serverID,
			operationID,
			lifecycle.ProvisionSpec{
				Runtime: *runtimeName,
				HeapMiB: uint32(*heapMiB),
			},
		)

		if actionErr == nil {
			fmt.Printf("changed: %t\n", changed)
		}
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
