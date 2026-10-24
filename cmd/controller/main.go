// SPDX-License-Identifier: AGPL-3.0-only

// Command controller runs the Waystone controller. It serves the local
// HTTP API on a Unix socket, the browser-facing side (behind the proxy) on
// a second one and, when configured, the agent link on a third. It never
// opens a TCP port itself: the one TCP socket it may serve, the agent
// listener for other machines' agents (stage 6a), systemd opens and hands
// over, and it speaks only gRPC with mutual TLS. "controller migrate -db
// <dsn>" applies the database migrations, "controller user ..." manages
// browser accounts, and "controller agent-pin" prints the agent link's
// key pin.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"os/user"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/sharddevs/waystone_panel/internal/agentlink"
	"github.com/sharddevs/waystone_panel/internal/agenttls"
	"github.com/sharddevs/waystone_panel/internal/api"
	"github.com/sharddevs/waystone_panel/internal/ids"
	"github.com/sharddevs/waystone_panel/internal/sdlisten"
	"github.com/sharddevs/waystone_panel/internal/service"
	"github.com/sharddevs/waystone_panel/internal/session"
	"github.com/sharddevs/waystone_panel/internal/store"
	"github.com/sharddevs/waystone_panel/internal/web"
)

const version = "0.1.0-alpha"

// defaultDSN is the controller's database: the waystone database as
// waystone_app, over the local socket with peer authentication.
const defaultDSN = "host=/var/run/postgresql dbname=waystone user=waystone_app"

// shutdownTimeout is how long in-flight requests get to finish after
// SIGTERM or SIGINT. The HTTP server and the agent link share it (R10).
const shutdownTimeout = 10 * time.Second

// The names systemd gives the sockets (FileDescriptorName= in the socket
// units: api and agent from 4c, web from stage WS).
const (
	apiSocketName   = "waystone-controller-api"
	agentSocketName = "waystone-controller-agent"
	webSocketName   = "waystone-controller-web"
	// The agent listener for other machines (6a, D3): TCP, gRPC with
	// mutual TLS only.
	agentTCPSocketName = "waystone-controller-agent-tcp"
)

// defaultAgentTLSDir holds the controller's agent link key, in its
// StateDirectory.
const defaultAgentTLSDir = "/var/lib/waystone-controller/agent-tls"

// adminGroup: an agent uid in this group would hold admin rights too
// (decision E), so the controller refuses it.
const adminGroup = "waystone-admin"

func main() {
	// "migrate" is a separate mode: it updates the schema and exits
	// without starting the server.
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		os.Exit(runMigrate(os.Args[2:], os.Stdout, os.Stderr, store.Migrate))
	}
	// "user" manages browser accounts as the database owner (5a-1) and
	// exits, like migrate.
	if len(os.Args) > 1 && os.Args[1] == "user" {
		os.Exit(runUser(os.Args[2:], os.Stdout, os.Stderr, realUserDeps()))
	}
	if len(os.Args) > 1 && os.Args[1] == "agent-pin" {
		os.Exit(runAgentPin(os.Args[2:], os.Getuid(), os.Stdout, os.Stderr))
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

// sockets are the controller's listeners. agent and agentTCP may be nil.
type sockets struct {
	api, web, agent, agentTCP net.Listener
}

func (s sockets) close() {
	for _, l := range []net.Listener{s.api, s.web, s.agent, s.agentTCP} {
		if l != nil {
			l.Close()
		}
	}
}

// listeners returns the API, web and agent sockets; the agent socket may
// be nil.
//
// Dev mode is -api-socket, which also needs -web-socket (W5: there is no
// controller without its browser-facing socket): listenDev for both, and
// listenAgentDev for -agent-socket if it's set (localnode builds only).
// Otherwise systemd must have passed the sockets (socket activation),
// named apiSocketName and webSocketName and, optionally, agentSocketName
// (D2) and the TCP agentTCPSocketName (6a). Any other socket is refused. There is never a fallback that opens
// a socket or port of its own.
func listeners(apiSocket, webSocket, agentSocket string, pid int, getenv func(string) string) (sockets, error) {
	var s sockets
	if apiSocket != "" || webSocket != "" {
		if apiSocket == "" || webSocket == "" {
			return s, errors.New("dev mode needs both -api-socket and -web-socket")
		}
		var err error
		if s.api, err = listenDev(apiSocket); err != nil {
			return sockets{}, err
		}
		if s.web, err = listenDev(webSocket); err != nil {
			s.close()
			return sockets{}, fmt.Errorf("web socket: %w", err)
		}
		if agentSocket == "" {
			return s, nil
		}
		if s.agent, err = listenAgentDev(agentSocket); err != nil {
			s.close()
			return sockets{}, fmt.Errorf("agent socket: %w", err)
		}
		return s, nil
	}
	if agentSocket != "" {
		return s, errors.New("-agent-socket is dev mode and needs -api-socket")
	}

	fds, err := sdlisten.Listeners(pid, getenv, agentTCPSocketName)
	if err != nil {
		return s, err
	}
	s = sockets{api: fds[apiSocketName], web: fds[webSocketName], agent: fds[agentSocketName], agentTCP: fds[agentTCPSocketName]}
	delete(fds, apiSocketName)
	delete(fds, webSocketName)
	delete(fds, agentSocketName)
	delete(fds, agentTCPSocketName)
	if s.api == nil || s.web == nil || len(fds) != 0 {
		s.close()
		for _, l := range fds {
			l.Close()
		}
		return sockets{}, fmt.Errorf("systemd must pass %s and %s, at most %s and %s, and nothing else", apiSocketName, webSocketName, agentSocketName, agentTCPSocketName)
	}
	return s, nil
}

// checkProxyUID refuses a proxy uid that is root, the agent's or the
// controller's own (self): the proxy faces the internet and must be its
// own unprivileged user (W2).
func checkProxyUID(uid, agentUID, self int) error {
	if uid == -1 {
		return errors.New("-proxy-uid is required: the reverse proxy's uid (id -u caddy)")
	}
	if uid <= 0 || uid > 1<<32-1 {
		return fmt.Errorf("-proxy-uid %d: must be a uid other than 0", uid)
	}
	if uid == agentUID {
		return fmt.Errorf("-proxy-uid %d is the agent's uid", uid)
	}
	if uid == self {
		return fmt.Errorf("-proxy-uid %d is the controller's own uid", uid)
	}
	return nil
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
// Flags: -db (default defaultDSN), -api-socket and -web-socket (dev
// mode); for the browser-facing side, -public-url and -proxy-uid (both
// required) and -hsts-max-age; for the agent link -agent-uid and
// -agent-node, plus -agent-socket in dev mode. The agent link runs only
// if there is an agent socket, and then both -agent-uid and -agent-node
// are required.
// -dev-ui-dir serves the web UI from disk, only in -tags devweb builds
// (Q13); otherwise the UI is the one built in with -tags ui, or the "UI
// not built" page.
func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("controller", flag.ContinueOnError)
	flags.SetOutput(stderr)

	dsn := flags.String("db", defaultDSN, "PostgreSQL connection string")
	socketPath := flags.String("api-socket", "", "Development Unix socket path")
	agentSocket := flags.String("agent-socket", "", "Development agent socket path (localnode builds)")
	agentUID := flags.Int("agent-uid", -1, "The only uid the agent link accepts")
	agentNode := flags.String("agent-node", "", "The node the agent speaks for (UUID)")
	webSocket := flags.String("web-socket", "", "Development web socket path (with -api-socket)")
	publicURL := flags.String("public-url", "", "The canonical public URL, e.g. https://panel.example.com (required)")
	proxyUID := flags.Int("proxy-uid", -1, "The only uid web.sock accepts: the reverse proxy's (required)")
	hstsMaxAge := flags.Int("hsts-max-age", web.DefaultHSTSMaxAge, "HSTS max-age in seconds")
	devUIDir := flags.String("dev-ui-dir", "", "Development: serve the web UI from this directory (devweb builds)")
	agentTLSDir := flags.String("agent-tls-dir", defaultAgentTLSDir, "The agent link's key, for the TCP agent listener")

	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: controller -public-url <url> -proxy-uid <uid> [-hsts-max-age <s>] [-db <dsn>] [-api-socket <path> -web-socket <path>] [-agent-socket <path>] [-agent-uid <uid> -agent-node <uuid>] [-dev-ui-dir <dir>]")
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
	public, err := web.ParsePublicURL(*publicURL)
	if err != nil {
		fmt.Fprintln(stderr, "-public-url:", err)
		return 2
	}
	if err := checkProxyUID(*proxyUID, *agentUID, os.Getuid()); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *hstsMaxAge < 1 || *hstsMaxAge > web.MaxHSTSMaxAge {
		fmt.Fprintf(stderr, "-hsts-max-age %d: must be 1 to %d\n", *hstsMaxAge, web.MaxHSTSMaxAge)
		return 2
	}
	ui, uiSource, err := uiRoutes(*devUIDir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx := context.Background()

	db, err := store.Open(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(stderr, "database:", err)
		return 1
	}
	defer db.Close()

	socks, err := listeners(*socketPath, *webSocket, *agentSocket, os.Getpid(), os.Getenv)
	if err != nil {
		fmt.Fprintln(stderr, "listener:", err)
		return 1
	}
	defer socks.close()
	listener, agentListener := socks.api, socks.agent
	if (agentListener != nil) != agentConfigured {
		fmt.Fprintln(stderr, "agent link: -agent-uid and -agent-node go with an agent socket, and an agent socket needs them")
		return 1
	}

	var link *agentlink.Server
	var enrollAt *service.AgentLink
	if agentListener != nil || socks.agentTCP != nil {
		linkCfg := agentlink.Config{Store: db, Version: version}
		if agentListener != nil {
			linkCfg.NodeID, linkCfg.AgentUID = *agentNode, uint32(*agentUID)
		}
		if socks.agentTCP != nil {
			// The key is made on first start and kept (D1). A new key
			// would be a new pin, and every enrolled node would refuse it.
			cert, err := agenttls.LoadOrCreate(*agentTLSDir)
			if err != nil {
				fmt.Fprintln(stderr, "agent link key:", err)
				return 1
			}
			linkCfg.TLS = &cert
			// A bundle names the listener's own address, from its socket
			// unit (6H3); the service refuses tokens if it's unspecified.
			enrollAt = &service.AgentLink{Addr: listenerAddr(socks.agentTCP), Pin: agenttls.PinOf(cert.Leaf)}
			fmt.Fprintf(stderr, "Waystone agent link TCP listening on %s, controller pin %s\n", socks.agentTCP.Addr(), agenttls.PinOf(cert.Leaf))
		}
		link, err = agentlink.New(linkCfg)
		if err != nil {
			fmt.Fprintln(stderr, "agent link:", err)
			return 1
		}
	}

	svcCfg := service.Config{Store: db}
	if link != nil {
		// A new operation wakes the agent session at once (Q7), instead
		// of waiting for its next report.
		svcCfg.OnOperation = func(op store.Operation) { link.Kick(op.NodeID) }
		// A revocation cuts the node's live stream at once (D5).
		svcCfg.OnNodeRevoked = link.Revoke
		svcCfg.AgentLink = enrollAt
	}
	// One service for both sockets (5b, Q1): the same handlers and one
	// POST rate limit, per actor. Each socket has its own concurrency
	// slots, so the browser side can't crowd out api.sock (5b audit H1).
	svc := service.New(svcCfg)
	handler := api.New(api.Config{Version: version, Service: svc})

	sessions, err := session.New(session.Config{Store: db})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	webHandler, err := web.New(web.Config{Public: public, ProxyUID: uint32(*proxyUID), HSTSMaxAge: *hstsMaxAge, Sessions: sessions}, webRoutes(sessions, svc, ui)...)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	server := newServer(handler)
	webServer := webHandler.NewServer(socks.web)

	// I7, F2: whatever was running when the last controller stopped may
	// or may not have happened. That becomes uncertain before anything
	// is served, so nothing can be sent or finished in between. It comes
	// after the listeners on purpose: a second controller that can't get
	// the sockets must not touch the first one's operations (L7).
	recovered, err := db.RecoverRunningOperations(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "startup recovery:", err)
		return 1
	}
	fmt.Fprintf(stderr, "startup recovery: %d running operation(s) marked uncertain\n", recovered)

	// I4: sessions whose browser never comes back are deleted at start
	// and then every hour.
	stopPurge := make(chan struct{})
	defer close(stopPurge)
	go purgeSessions(db, purgeEvery, stopPurge, func(format string, args ...any) {
		fmt.Fprintf(stderr, format+"\n", args...)
	})

	fmt.Fprintf(stderr, "Waystone controller listening on %s\n", listener.Addr())
	fmt.Fprintf(stderr, "Waystone web listening on %s for %s, proxy uid %d\n", webServer.Addr(), public, *proxyUID)
	fmt.Fprintf(stderr, "web UI: %s\n", uiSource)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.Serve(listener)
	}()
	webErrors := make(chan error, 1)
	go func() {
		webErrors <- webServer.Serve()
	}()

	agentErrors := make(chan error, 2)
	if agentListener != nil {
		fmt.Fprintf(stderr, "Waystone agent link listening on %s for uid %d, node %s\n", agentListener.Addr(), *agentUID, *agentNode)
		go func() {
			agentErrors <- link.Serve(agentListener)
		}()
	}
	if socks.agentTCP != nil {
		go func() {
			agentErrors <- link.ServeTCP(socks.agentTCP)
		}()
	}

	code := 0
	select {
	case err := <-serveErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, "HTTP server:", err)
			code = 1
		}
	case err := <-webErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, "web server:", err)
			code = 1
		}
	case err := <-agentErrors:
		fmt.Fprintln(stderr, "agent link:", err)
		code = 1
	case sig := <-signals:
		fmt.Fprintln(stderr, "received signal:", sig)
	}

	if !shutdown(server, webServer, link, shutdownTimeout, stderr) {
		code = 1
	}
	// Failed sign-ins' audit rows are written after their responses (A3);
	// let the last ones land before the database closes.
	sessions.Wait()
	return code
}

// shutdown is R10: end every agent session first (those streams never end
// on their own), then stop the HTTP server, the web server and the agent
// link together, within one budget. Whatever is still running then is cut
// off. It reports whether everything finished in time. webServer may be
// nil (only in tests).
// listenerAddr is l's address as an IP and port, or the zero value if it
// isn't a TCP listener.
func listenerAddr(l net.Listener) netip.AddrPort {
	a, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return netip.AddrPort{}
	}
	ap := a.AddrPort()
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

func shutdown(server *http.Server, webServer *web.Server, link *agentlink.Server, budget time.Duration, stderr io.Writer) bool {
	if link != nil {
		link.CancelSessions()
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	var wg sync.WaitGroup
	var httpErr, webErr, linkErr error
	if webServer != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if webErr = webServer.Shutdown(ctx); webErr != nil {
				_ = webServer.Close()
			}
		}()
	}
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
	if webErr != nil {
		fmt.Fprintln(stderr, "web shutdown:", webErr)
	}
	if linkErr != nil {
		fmt.Fprintln(stderr, "agent link shutdown:", linkErr)
	}
	return httpErr == nil && webErr == nil && linkErr == nil
}
