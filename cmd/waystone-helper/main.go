//go:build linux

// Command waystone-helper is the root privileged helper. systemd starts it
// through waystone-helper.socket; it serves requests from the agent's user
// only and performs a small, fixed set of actions.
package main

import (
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
	flag.Parse()
	log.SetFlags(0) // journald adds timestamps

	account, err := user.Lookup(*agentUser)
	if err != nil {
		log.Fatalf("cannot look up agent user %q: %v", *agentUser, err)
	}

	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		log.Fatalf("invalid UID for agent user %q: %v", *agentUser, err)
	}

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
