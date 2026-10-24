// SPDX-License-Identifier: AGPL-3.0-only

// Command waystone-agent runs on a node as the unprivileged agent user. It
// connects to the controller, every report interval it asks the helper
// for each server's state and reports it, and it carries out the
// controller's start, stop and restart commands through the helper.
//
// It reaches the controller one of two ways: on the same machine through
// its agent socket (-controller, -controller-uid), or from another
// machine over TCP with mutual TLS and pinned keys (-controller-addr,
// -controller-pin), which "waystone-agent enroll" sets up (stage 6a).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/netip"
	"os"
	"os/signal"
	"syscall"

	"github.com/sharddevs/waystone_panel/internal/agent"
	"github.com/sharddevs/waystone_panel/internal/agenttls"
	"github.com/sharddevs/waystone_panel/internal/helper"
	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/lifecycle"
	"github.com/sharddevs/waystone_panel/internal/profile/minecraft"
)

const version = "0.1.0-alpha"

// defaultTLSDir holds this node's agent link key (remote mode).
const defaultTLSDir = "/var/lib/waystone-agent/agent-tls"

func main() {
	log.SetFlags(0) // journald adds timestamps
	// "enroll" turns this machine into a node (6a); it runs as root and
	// exits.
	if len(os.Args) > 1 && os.Args[1] == "enroll" {
		os.Exit(runEnroll(os.Args[2:], os.Stdout, os.Stderr, realEnrollDeps()))
	}
	os.Exit(run(os.Args[1:], os.Getuid(), os.Stderr))
}

// config is the parsed command line.
type config struct {
	controller    string
	controllerUID uint32
	// Remote mode (6a): the controller's address and pin, and this
	// node's key directory.
	controllerAddr netip.AddrPort
	controllerPin  agenttls.Pin
	tlsDir         string
	node           string
	helper         string
}

func (c config) remote() bool { return c.controllerAddr.IsValid() }

// parse reads the flags. -node is required, and either -controller with
// -controller-uid (the controller on this machine) or -controller-addr
// with -controller-pin (another machine), never both. The agent refuses
// to run as root.
func parse(args []string, uid int, stderr io.Writer) (config, error) {
	flags := flag.NewFlagSet("waystone-agent", flag.ContinueOnError)
	flags.SetOutput(stderr)
	controller := flags.String("controller", "", "The controller's agent socket")
	controllerUID := flags.Int("controller-uid", -1, "The only uid the agent talks to")
	controllerAddr := flags.String("controller-addr", "", "The controller's agent listener, ip:port (remote nodes)")
	controllerPin := flags.String("controller-pin", "", "The controller's key pin (remote nodes)")
	tlsDir := flags.String("tls-dir", defaultTLSDir, "This node's key directory (remote nodes)")
	node := flags.String("node", "", "This node's ID (UUID)")
	helperSocket := flags.String("helper", helper.DefaultSocketPath, "The helper's socket")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	local := *controller != "" || *controllerUID != -1
	remote := *controllerAddr != "" || *controllerPin != ""
	switch {
	case flags.NArg() != 0:
		return config{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	case uid == 0:
		return config{}, fmt.Errorf("refusing to run as root")
	case *helperSocket == "":
		return config{}, fmt.Errorf("-helper is required")
	case local == remote:
		return config{}, fmt.Errorf("give either -controller and -controller-uid, or -controller-addr and -controller-pin")
	case local && *controller == "":
		return config{}, fmt.Errorf("-controller is required")
	case local && (*controllerUID < 0 || *controllerUID > 1<<32-1):
		return config{}, fmt.Errorf("-controller-uid is required")
	}
	if err := ids.Check(*node); err != nil {
		return config{}, fmt.Errorf("-node: %w", err)
	}
	cfg := config{node: *node, helper: *helperSocket}
	if local {
		cfg.controller, cfg.controllerUID = *controller, uint32(*controllerUID)
		return cfg, nil
	}
	addr, err := netip.ParseAddrPort(*controllerAddr)
	if err != nil || addr.Port() == 0 {
		return config{}, fmt.Errorf("-controller-addr: an IP and a port, like 10.0.20.80:7443")
	}
	pin, err := agenttls.ParsePin(*controllerPin)
	if err != nil {
		return config{}, fmt.Errorf("-controller-pin: %w", err)
	}
	if *tlsDir == "" {
		return config{}, fmt.Errorf("-tls-dir is required")
	}
	cfg.controllerAddr, cfg.controllerPin, cfg.tlsDir = addr, pin, *tlsDir
	return cfg, nil
}

// run returns 2 for bad flags or running as root, and 0 after SIGTERM or
// SIGINT. Losing the controller is never fatal: the agent reconnects.
func run(args []string, uid int, stderr io.Writer) int {
	cfg, err := parse(args, uid, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "waystone-agent:", err)
		return 2
	}
	acfg := agent.Config{
		ControllerSocket: cfg.controller,
		ControllerUID:    cfg.controllerUID,
		NodeID:           cfg.node,
		Version:          version,
		Helper:           helper.Client{SocketPath: cfg.helper},
		Runner:           lifecycle.HelperRuntime{Client: helper.Client{SocketPath: cfg.helper}},
		Prober:           minecraft.StatusPing{},
	}
	var where string
	if cfg.remote() {
		// The key is enroll's; the agent only reads it. A missing key is
		// a configuration error, never a reason to make a new one.
		key, err := agenttls.Load(cfg.tlsDir)
		if err != nil {
			fmt.Fprintln(stderr, "waystone-agent: node key:", err)
			return 2
		}
		acfg.ControllerAddr, acfg.TLS = cfg.controllerAddr.String(), agenttls.ClientConfig(key, cfg.controllerPin)
		where = fmt.Sprintf("%s (pin %s)", cfg.controllerAddr, cfg.controllerPin)
	} else {
		where = fmt.Sprintf("%s (uid %d)", cfg.controller, cfg.controllerUID)
	}
	a, err := agent.New(acfg)
	if err != nil {
		fmt.Fprintln(stderr, "waystone-agent:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("waystone-agent %s: node %s, controller %s, helper %s",
		version, cfg.node, where, cfg.helper)
	_ = a.Run(ctx)
	log.Print("waystone-agent: stopped")
	return 0
}
