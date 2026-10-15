// SPDX-License-Identifier: AGPL-3.0-only

// Command waystone-agent runs on a node as the unprivileged agent user. It
// connects to the controller, every report interval it asks the helper
// for each server's state and reports it, and it carries out the
// controller's start, stop and restart commands through the helper.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/sharddevs/waystone_panel/internal/agent"
	"github.com/sharddevs/waystone_panel/internal/helper"
	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/lifecycle"
	"github.com/sharddevs/waystone_panel/internal/profile/minecraft"
)

const version = "0.1.0-alpha"

func main() {
	log.SetFlags(0) // journald adds timestamps
	os.Exit(run(os.Args[1:], os.Getuid(), os.Stderr))
}

// config is the parsed command line.
type config struct {
	controller    string
	controllerUID uint32
	node          string
	helper        string
}

// parse reads the flags. Every one but -helper is required, and the agent
// refuses to run as root.
func parse(args []string, uid int, stderr io.Writer) (config, error) {
	flags := flag.NewFlagSet("waystone-agent", flag.ContinueOnError)
	flags.SetOutput(stderr)
	controller := flags.String("controller", "", "The controller's agent socket")
	controllerUID := flags.Int("controller-uid", -1, "The only uid the agent talks to")
	node := flags.String("node", "", "This node's ID (UUID)")
	helperSocket := flags.String("helper", helper.DefaultSocketPath, "The helper's socket")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	switch {
	case flags.NArg() != 0:
		return config{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	case uid == 0:
		return config{}, fmt.Errorf("refusing to run as root")
	case *controller == "" || *helperSocket == "":
		return config{}, fmt.Errorf("-controller and -helper are required")
	case *controllerUID < 0 || *controllerUID > 1<<32-1:
		return config{}, fmt.Errorf("-controller-uid is required")
	}
	if err := ids.Check(*node); err != nil {
		return config{}, fmt.Errorf("-node: %w", err)
	}
	return config{
		controller:    *controller,
		controllerUID: uint32(*controllerUID),
		node:          *node,
		helper:        *helperSocket,
	}, nil
}

// run returns 2 for bad flags or running as root, and 0 after SIGTERM or
// SIGINT. Losing the controller is never fatal: the agent reconnects.
func run(args []string, uid int, stderr io.Writer) int {
	cfg, err := parse(args, uid, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "waystone-agent:", err)
		return 2
	}
	a, err := agent.New(agent.Config{
		ControllerSocket: cfg.controller,
		ControllerUID:    cfg.controllerUID,
		NodeID:           cfg.node,
		Version:          version,
		Helper:           helper.Client{SocketPath: cfg.helper},
		Runner:           lifecycle.HelperRuntime{Client: helper.Client{SocketPath: cfg.helper}},
		Prober:           minecraft.StatusPing{},
	})
	if err != nil {
		fmt.Fprintln(stderr, "waystone-agent:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("waystone-agent %s: node %s, controller %s (uid %d), helper %s",
		version, cfg.node, cfg.controller, cfg.controllerUID, cfg.helper)
	_ = a.Run(ctx)
	log.Print("waystone-agent: stopped")
	return 0
}
