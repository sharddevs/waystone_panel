//go:build linux

// Command waystone-helper is the small helper that runs as root. systemd
// hands it a socket, and it serves requests from the agent's user only,
// as either the runtime or the provision role.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"

	"github.com/sharddevs/waystone_panel/internal/helper"
)

func main() {
	agentUser := flag.String("agent-user", "waystone-agent", "the only user allowed to send requests")
	role := flag.String("role", helper.RoleRuntime, "helper role: runtime or provision")
	upgrade := flag.Bool("upgrade-units", false, "rewrite every Waystone server unit for this build, restarting nothing, and exit")
	flag.Parse()
	log.SetFlags(0) // journald adds timestamps

	if *upgrade {
		upgradeUnits()
		return
	}

	account, err := user.Lookup(*agentUser)
	if err != nil {
		log.Fatalf("cannot look up agent user %q: %v", *agentUser, err)
	}

	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		log.Fatalf("invalid UID for agent user %q: %v", *agentUser, err)
	}

	// Authorizing uid 0 would let any root process in, so refuse to start.
	if uid == 0 {
		log.Fatal("refusing to authorize root as the agent user")
	}
	listener, err := activationListener(os.Getpid(), os.Getenv)
	if err != nil {
		log.Fatalf("socket activation failed: %v", err)
	}
	defer listener.Close()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)

	// Closing the listener makes Serve return, so main can exit cleanly.
	go func() {
		<-signals
		log.Print("shutdown signal received")
		listener.Close()
	}()
	router, err := helper.NewRouter(*role, helper.ExecRunner{})
	if err != nil {
		log.Fatalf("cannot configure helper role: %v", err)
	}

	server := &helper.Server{
		AgentUID: uint32(uid),
		PeerCred: peerCred,
		Handle:   router.Handle,
		Log:      log.Default(),
	}
	log.Printf("waystone-helper ready; role=%s authorized agent user=%s uid=%d", *role, *agentUser, uid)

	if err := server.Serve(listener); err != nil {
		log.Fatalf("helper server failed: %v", err)
	}
}

// upgradeUnits is -upgrade-units (7b, H4): run once by hand, as root, on
// deploy. No game is started, stopped or restarted.
func upgradeUnits() {
	results, err := helper.UpgradeUnits(context.Background(), helper.UnitFiles{Dir: helper.DefaultUnitDir}, helper.ExecRunner{}, helper.DefaultRuntimes)
	for _, r := range results {
		what := "unchanged"
		if r.Changed {
			what = "rewritten; console after its next start"
		}
		log.Printf("server %s: %s", r.ServerID, what)
	}
	if err != nil {
		log.Fatalf("upgrade stopped: %v", err)
	}
	log.Printf("%d servers checked; nothing was restarted", len(results))
}
