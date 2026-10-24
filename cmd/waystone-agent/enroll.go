// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	agentv1 "github.com/sharddevs/waystone_panel/gen/waystone/agent/v1"
	"github.com/sharddevs/waystone_panel/internal/agenttls"
	"github.com/sharddevs/waystone_panel/internal/enrollbundle"
	"github.com/sharddevs/waystone_panel/internal/ids"
)

// "waystone-agent enroll" turns this machine into a node (stage 6a, D2):
// it takes the one-time bundle an admin copied from the panel, makes this
// node's key, redeems the token over the pinned TLS link, writes the
// agent's configuration and starts the agent.
//
// It runs as root only to write those files and start the unit (6H8); the
// agent itself still runs as waystone-agent. The bundle is read from a
// no-echo prompt or from stdin, never from the command line (6H2).

const (
	defaultEnvFile = "/etc/waystone/agent.env"
	defaultDropIn  = "/etc/systemd/system/waystone-agent.service.d/remote.conf"
	agentUser      = "waystone-agent"
	enrollTimeout  = 30 * time.Second
	// pendingDir, inside the key directory, holds a key that hasn't
	// enrolled yet. A failed try leaves it, and the next try uses it
	// again; a success moves it in place. So a machine that enrolls again
	// after a revocation gets a new key, never the revoked one.
	pendingDir = "pending"
)

// enrollDeps are the outside world for runEnroll.
type enrollDeps struct {
	uid func() int
	// readBundle prompts on a terminal without echo, or reads one line.
	readBundle func() (string, error)
	// lookupAgent returns the agent user's uid and gid.
	lookupAgent func() (uid, gid int, err error)
	// enroll calls Enroll on the controller and returns the node.
	enroll    func(ctx context.Context, addr string, tc *tls.Config, secret []byte) (nodeID, name string, err error)
	systemctl func(args ...string) error
}

func realEnrollDeps() enrollDeps {
	return enrollDeps{
		uid:         os.Getuid,
		readBundle:  func() (string, error) { return readBundle(os.Stdin, os.Stderr) },
		lookupAgent: lookupAgentUser,
		enroll:      enrollRPC,
		systemctl: func(args ...string) error {
			out, err := exec.Command("systemctl", args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
			}
			return nil
		},
	}
}

const enrollUsage = "usage: sudo waystone-agent enroll [-tls-dir <dir>] [-env-file <path>] [-drop-in <path>]\n" +
	"Paste the enrollment bundle from the panel when asked."

// runEnroll returns 2 for usage problems or not running as root, 1 if
// enrolling fails, and 0 once the agent has been started.
func runEnroll(args []string, stdout, stderr io.Writer, d enrollDeps) int {
	flags := flag.NewFlagSet("enroll", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tlsDir := flags.String("tls-dir", defaultTLSDir, "This node's key directory")
	envFile := flags.String("env-file", defaultEnvFile, "The agent's environment file")
	dropIn := flags.String("drop-in", defaultDropIn, "The agent unit's drop-in for remote mode")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, enrollUsage)
		return 2
	}
	if d.uid() != 0 {
		fmt.Fprintln(stderr, "waystone-agent enroll: run it as root (sudo); it writes the agent's configuration and starts it")
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "waystone-agent enroll: "+format+"\n", a...)
		return 1
	}

	if node, err := enrolledNode(*envFile); err != nil {
		return fail("%v", err)
	} else if node != "" {
		return fail("this machine is already node %s. To enroll it again, revoke it in the panel, then remove %s", node, *envFile)
	}
	agentUID, agentGID, err := d.lookupAgent()
	if err != nil {
		return fail("%v", err)
	}

	text, err := d.readBundle()
	if err != nil {
		return fail("reading the bundle: %v", err)
	}
	b, err := enrollbundle.Decode(text)
	if err != nil {
		return fail("%v", err)
	}

	// The key directory may sit in a directory the agent user can write
	// (6a-M1), so it is used only through checked, open directories:
	// agent-tls may be root's (a first try) or the agent's (enrolling
	// again), and pending is always root's alone, so the key in it can't
	// have come from anyone else.
	self := os.Geteuid()
	keys, err := agenttls.OpenKeyDir(*tlsDir, true, self, agentUID)
	if err != nil {
		return fail("%v", err)
	}
	defer keys.Close()
	pending, err := keys.Sub(pendingDir, true, self)
	if err != nil {
		return fail("%v", err)
	}
	defer pending.Close()
	key, err := pending.LoadOrCreate()
	if err != nil {
		return fail("node key: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), enrollTimeout)
	defer cancel()
	nodeID, name, err := d.enroll(ctx, b.Addr.String(), agenttls.ClientConfig(key, b.Pin), b.Secret[:])
	if err != nil {
		return fail("the controller refused or couldn't be reached: %v. The key is kept for the next try; a used or expired token needs a new one from the panel", err)
	}
	if ids.Check(nodeID) != nil {
		return fail("the controller answered with a bad node ID")
	}

	// From here on this machine is the node: put everything in place.
	if err := pending.MoveTo(keys, agentUID, agentGID); err != nil {
		return fail("moving the key in place: %v", err)
	}
	if err := keys.RemoveSub(pendingDir); err != nil {
		return fail("%v", err)
	}
	if err := keys.Chown(agentUID, agentGID); err != nil {
		return fail("%v", err)
	}

	env := fmt.Sprintf("# Written by waystone-agent enroll. No secrets here.\nWAYSTONE_NODE_ID=%s\nWAYSTONE_CONTROLLER_ADDR=%s\nWAYSTONE_CONTROLLER_PIN=%s\n",
		nodeID, b.Addr, b.Pin)
	if err := os.MkdirAll(filepath.Dir(*envFile), 0o755); err != nil {
		return fail("%v", err)
	}
	if err := agenttls.WriteFileAtomic(*envFile, []byte(env), 0o640, -1, agentGID); err != nil {
		return fail("%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(*dropIn), 0o755); err != nil {
		return fail("%v", err)
	}
	if err := agenttls.WriteFileAtomic(*dropIn, []byte(dropInFor(b.Addr.Addr().String(), *tlsDir)), 0o644, -1, -1); err != nil {
		return fail("%v", err)
	}

	if err := d.systemctl("daemon-reload"); err != nil {
		return fail("%v", err)
	}
	if err := d.systemctl("enable", "--now", "waystone-agent.service"); err != nil {
		return fail("%v", err)
	}
	fmt.Fprintf(stdout, "Enrolled as node %q (%s). The agent is running.\n", name, nodeID)
	return 0
}

// dropInFor is the agent unit's remote mode: the TCP flags instead of the
// agent socket, and the kernel filter widened from localhost to exactly
// the controller's address (REVIEW-4d2).
func dropInFor(controllerIP, tlsDir string) string {
	return "# Written by waystone-agent enroll (stage 6a): this node reaches its\n" +
		"# controller over TCP with mutual TLS.\n" +
		"[Service]\n" +
		"ExecStart=\n" +
		"ExecStart=/usr/local/bin/waystone-agent -controller-addr ${WAYSTONE_CONTROLLER_ADDR} -controller-pin ${WAYSTONE_CONTROLLER_PIN} -node ${WAYSTONE_NODE_ID} -tls-dir " + tlsDir + "\n" +
		"IPAddressAllow=" + controllerIP + "\n"
}

// enrolledNode is the WAYSTONE_NODE_ID in an existing env file, or "".
func enrolledNode(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "WAYSTONE_NODE_ID="); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), nil
		}
	}
	return "", nil
}

// readBundle asks for the bundle without echo on a terminal, or reads the
// first line of stdin otherwise (sudo waystone-agent enroll < file).
func readBundle(stdin *os.File, prompt io.Writer) (string, error) {
	if term.IsTerminal(int(stdin.Fd())) {
		fmt.Fprint(prompt, "Paste the enrollment bundle from the panel (it isn't shown): ")
		b, err := term.ReadPassword(int(stdin.Fd()))
		fmt.Fprintln(prompt)
		return string(b), err
	}
	r := bufio.NewReader(io.LimitReader(stdin, enrollbundle.MaxEncodedBytes+2))
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return line, nil
}

func lookupAgentUser() (int, int, error) {
	u, err := user.Lookup(agentUser)
	if err != nil {
		return 0, 0, fmt.Errorf("the %s user: %w (install the agent first)", agentUser, err)
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil || uid == 0 {
		return 0, 0, fmt.Errorf("the %s user has an unusable uid or gid", agentUser)
	}
	return uid, gid, nil
}

// enrollRPC calls Enroll once over the pinned TLS link.
func enrollRPC(ctx context.Context, addr string, tc *tls.Config, secret []byte) (string, string, error) {
	cc, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(credentials.NewTLS(tc)))
	if err != nil {
		return "", "", err
	}
	defer cc.Close()
	r, err := agentv1.NewEnrollServiceClient(cc).Enroll(ctx, &agentv1.EnrollRequest{Secret: secret})
	if err != nil {
		return "", "", err
	}
	return r.GetNodeId(), r.GetName(), nil
}
