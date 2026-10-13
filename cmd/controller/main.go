// SPDX-License-Identifier: AGPL-3.0-only

// Command controller runs the Waystone controller. It serves the HTTP API
// on a Unix socket only and, when configured, the agent link on a second
// one. "controller migrate -db <dsn>" applies the database migrations.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/sharddevs/waystone_panel/internal/agentlink"
	"github.com/sharddevs/waystone_panel/internal/api"
	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/sdlisten"
	"github.com/sharddevs/waystone_panel/internal/store"
)

const version = "0.1.0-alpha"

// defaultDSN is the controller's database: the waystone database as
// waystone_app, over the local socket with peer authentication.
const defaultDSN = "host=/var/run/postgresql dbname=waystone user=waystone_app"

// shutdownTimeout is how long in-flight requests get to finish after
// SIGTERM or SIGINT. The HTTP server and the agent link share it (R10).
const shutdownTimeout = 10 * time.Second

// The names systemd gives the two sockets (FileDescriptorName= in the
// socket units, installed in 4c).
const (
	apiSocketName   = "waystone-controller-api"
	agentSocketName = "waystone-controller-agent"
)

// adminGroup: an agent uid in this group would hold admin rights too
// (decision E), so the controller refuses it.
const adminGroup = "waystone-admin"

func main() {
	// "migrate" is a separate mode: it updates the schema and exits
	// without starting the server.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		os.Exit(runMigrate(os.Args[2:], os.Stdout, os.Stderr, store.Migrate))
	}
	os.Exit(run(os.Args[1:], os.Stderr))
}

// newServer returns the http.Server for handler with the I12 limits:
// ReadHeaderTimeout 5 s, ReadTimeout 10 s, WriteTimeout 30 s, IdleTimeout
// 60 s, MaxHeaderBytes 16 KiB. ConnContext is api.ConnContext, which is
// what gives every request its caller's identity. There is no Addr: the
// server only ever runs on a listener we hand it.
//
// TODO
func newServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ConnContext:       api.ConnContext,
	}
}

// listeners returns the API socket and the agent socket, which may be
// nil.
//
// Dev mode is -api-socket: listenDev for the API, and listenAgentDev for
// -agent-socket if it's set (localnode builds only). Otherwise systemd
// must have passed the sockets (socket activation), named apiSocketName
// and, optionally, agentSocketName (D2). Any other socket is refused.
// There is never a fallback that opens a socket or port of its own.
func listeners(apiSocket, agentSocket string, pid int, getenv func(string) string) (api, agent net.Listener, err error) {
	if apiSocket != "" {
		api, err = listenDev(apiSocket)
		if err != nil || agentSocket == "" {
			return api, nil, err
		}
		agent, err = listenAgentDev(agentSocket)
		if err != nil {
			api.Close()
			return nil, nil, fmt.Errorf("agent socket: %w", err)
		}
		return api, agent, nil
	}
	if agentSocket != "" {
		return nil, nil, errors.New("-agent-socket is dev mode and needs -api-socket")
	}

	fds, err := sdlisten.Listeners(pid, getenv)
	if err != nil {
		return nil, nil, err
	}
	api, agent = fds[apiSocketName], fds[agentSocketName]
	delete(fds, apiSocketName)
	delete(fds, agentSocketName)
	if api == nil || len(fds) != 0 {
		for _, l := range []net.Listener{api, agent} {
			if l != nil {
				l.Close()
			}
		}
		for _, l := range fds {
			l.Close()
		}
		return nil, nil, fmt.Errorf("systemd must pass %s and at most %s, nothing else", apiSocketName, agentSocketName)
	}
	return api, agent, nil
}

// checkAgentUID refuses an agent uid that would give the agent more than
// a node's rights (R2): root, the controller's own uid, or a member of
// adminGroup. member answers the last question; see adminMember.
func checkAgentUID(uid, self int, member func(uid int) (bool, error)) error {
	if uid <= 0 || uid > 1<<32-1 {
		return fmt.Errorf("-agent-uid %d: must be a uid other than 0", uid)
	}
	if uid == self {
		return fmt.Errorf("-agent-uid %d is the controller's own uid", uid)
	}
	in, err := member(uid)
	if err != nil {
		return fmt.Errorf("-agent-uid %d: %w", uid, err)
	}
	if in {
		return fmt.Errorf("-agent-uid %d is a member of %s", uid, adminGroup)
	}
	return nil
}

// adminMember reports whether uid is in adminGroup. If the group doesn't
// exist, nobody is. If it does and the uid has no user entry, membership
// can't be ruled out, so that's an error, never a "no".
func adminMember(uid int) (bool, error) {
	g, err := user.LookupGroup(adminGroup)
	if err != nil {
		var unknown user.UnknownGroupError
		if errors.As(err, &unknown) {
			return false, nil
		}
		return false, fmt.Errorf("look up %s: %w", adminGroup, err)
	}
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return false, fmt.Errorf("look up the agent user: %w", err)
	}
	gids, err := u.GroupIds()
	if err != nil {
		return false, fmt.Errorf("look up the agent user's groups: %w", err)
	}
	return slices.Contains(gids, g.Gid), nil
}

// run starts the controller and returns the exit code:
//   - 2 for bad flags;
//   - 1 if the database, a listener or a server fails;
//   - 0 after a clean shutdown on SIGTERM or SIGINT.
//
// Flags: -db (default defaultDSN), -api-socket (dev mode), and for the
// agent link -agent-uid and -agent-node, plus -agent-socket in dev mode.
// The agent link runs only if there is an agent socket, and then both
// -agent-uid and -agent-node are required.
func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("controller", flag.ContinueOnError)
	flags.SetOutput(stderr)

	dsn := flags.String("db", defaultDSN, "PostgreSQL connection string")
	socketPath := flags.String("api-socket", "", "Development Unix socket path")
	agentSocket := flags.String("agent-socket", "", "Development agent socket path (localnode builds)")
	agentUID := flags.Int("agent-uid", -1, "The only uid the agent link accepts")
	agentNode := flags.String("agent-node", "", "The node the agent speaks for (UUID)")

	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: controller [-db <dsn>] [-api-socket <path>] [-agent-socket <path>] [-agent-uid <uid> -agent-node <uuid>]")
	}

	if err := flags.Parse(args); err != nil {
		return 2
	}

	if flags.NArg() != 0 || *dsn == "" {
		flags.Usage()
		return 2
	}

	agentConfigured := *agentUID != -1 || *agentNode != ""
	if agentConfigured {
		if err := ids.Check(*agentNode); err != nil {
			fmt.Fprintln(stderr, "-agent-node:", err)
			return 2
		}
		if err := checkAgentUID(*agentUID, os.Getuid(), adminMember); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	if *agentSocket != "" && !agentConfigured {
		fmt.Fprintln(stderr, "-agent-socket needs -agent-uid and -agent-node")
		return 2
	}

	ctx := context.Background()

	db, err := store.Open(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(stderr, "database:", err)
		return 1
	}
	defer db.Close()

	listener, agentListener, err := listeners(*socketPath, *agentSocket, os.Getpid(), os.Getenv)
	if err != nil {
		fmt.Fprintln(stderr, "listener:", err)
		return 1
	}
	defer listener.Close()
	if agentListener != nil {
		defer agentListener.Close()
	}
	if (agentListener != nil) != agentConfigured {
		fmt.Fprintln(stderr, "agent link: -agent-uid and -agent-node go with an agent socket, and an agent socket needs them")
		return 1
	}

	var link *agentlink.Server
	if agentListener != nil {
		link, err = agentlink.New(agentlink.Config{
			Store:    db,
			NodeID:   *agentNode,
			AgentUID: uint32(*agentUID),
		})
		if err != nil {
			fmt.Fprintln(stderr, "agent link:", err)
			return 1
		}
	}

	handler := api.New(api.Config{
		Version: version,
		Store:   db,
	})

	server := newServer(handler)

	fmt.Fprintf(stderr, "Waystone controller listening on %s\n", listener.Addr())

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.Serve(listener)
	}()

	agentErrors := make(chan error, 1)
	if link != nil {
		fmt.Fprintf(stderr, "Waystone agent link listening on %s for uid %d, node %s\n", agentListener.Addr(), *agentUID, *agentNode)
		go func() {
			agentErrors <- link.Serve(agentListener)
		}()
	}

	code := 0
	select {
	case err := <-serveErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, "HTTP server:", err)
			code = 1
		}
	case err := <-agentErrors:
		fmt.Fprintln(stderr, "agent link:", err)
		code = 1
	case sig := <-signals:
		fmt.Fprintln(stderr, "received signal:", sig)
	}

	if !shutdown(server, link, shutdownTimeout, stderr) {
		code = 1
	}
	return code
}

// shutdown is R10: end every agent session first (those streams never end
// on their own), then stop the HTTP server and the agent link together,
// within one budget. Whatever is still running then is cut off. It
// reports whether everything finished in time.
func shutdown(server *http.Server, link *agentlink.Server, budget time.Duration, stderr io.Writer) bool {
	if link != nil {
		link.CancelSessions()
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	var wg sync.WaitGroup
	var httpErr, linkErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		if httpErr = server.Shutdown(ctx); httpErr != nil {
			_ = server.Close()
		}
	}()
	if link != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			linkErr = link.Stop(ctx)
		}()
	}
	wg.Wait()
	if httpErr != nil {
		fmt.Fprintln(stderr, "HTTP shutdown:", httpErr)
	}
	if linkErr != nil {
		fmt.Fprintln(stderr, "agent link shutdown:", linkErr)
	}
	return httpErr == nil && linkErr == nil
}
