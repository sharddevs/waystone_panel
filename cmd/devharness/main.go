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
	"github.com/sharddevs/waystone_panel/internal/profile/minecraft"
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
	iAcceptTheMinecraftEula := flag.Bool("i-accept-the-minecraft-eula", false, "Explicitly agree to the Minecraft EULA")
	port := flag.Uint("port", 25565, "Minecraft server port")
	motd := flag.String("motd", "A Waystone server", "Minecraft server message of the day")
	maxPlayers := flag.Uint("max-players", 20, "Maximum number of players")
	name := flag.String("name", "", "File name to read")

	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: devharness [-socket path] [-provisioner-socket path] start|stop|status <server-id>")
		fmt.Fprintln(os.Stderr, "       devharness [-provisioner-socket path] [-runtime name] [-heap MiB] provision <server-id>")
		fmt.Fprintln(os.Stderr, "       devharness -i-accept-the-minecraft-eula accept-eula <server-id>")
		fmt.Fprintln(os.Stderr, "       devharness [-port n] [-motd text] [-max-players n] write-properties <server-id>")
		fmt.Fprintln(os.Stderr, "       devharness -name filename read-file <server-id>")
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
	case "accept-eula":
		if !*iAcceptTheMinecraftEula {
			fmt.Fprintln(
				os.Stderr,
				"accept-eula requires -i-accept-the-minecraft-eula",
			)
			os.Exit(2)
		}

		actionErr = minecraft.AcceptEula(
			ctx,
			runtime,
			serverID,
			operationID,
		)
	case "write-properties":
		if uint64(*port) > uint64(^uint32(0)) ||
			uint64(*maxPlayers) > uint64(^uint32(0)) {
			fmt.Fprintln(os.Stderr, "property value exceeds uint32 range")
			os.Exit(2)
		}

		actionErr = minecraft.WriteProperties(
			ctx,
			runtime,
			serverID,
			operationID,
			minecraft.Properties{
				ServerPort: uint32(*port),
				Motd:       *motd,
				MaxPlayers: uint32(*maxPlayers),
			},
		)
	case "read-file":
		if *name == "" {
			fmt.Fprintln(os.Stderr, "read-file requires -name")
			os.Exit(2)
		}

		var file lifecycle.File
		file, actionErr = runtime.ReadFile(
			ctx,
			serverID,
			operationID,
			*name,
		)

		if actionErr == nil {
			fmt.Printf("exists: %t\n", file.Exists)
			if file.Exists {
				fmt.Printf("content:\n%s", file.Content)
			}
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
