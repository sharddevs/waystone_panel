// SPDX-License-Identifier: AGPL-3.0-only

// Command waystone-worker reads and writes small files in one game server's
// directory. The root helper starts it with systemd-run as the server's own
// user, in a sandboxed unit whose working directory is the server directory.
// It refuses to run as root and knows nothing about any game.
//
// Usage:
//
//	waystone-worker read-file <name>                    (worker.ReadResult as JSON on stdout)
//	waystone-worker write-file <name> <absent|sha256>   (content on stdin, worker.WriteResult as JSON on stdout)
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/sharddevs/waystone_panel/internal/worker"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.Geteuid(), "."))
}

// run is the whole program except for the process itself. main passes in
// the real args, streams, euid and directory, so tests can call run with
// their own. It returns the exit code: 0 on success, 1 on an error, 2 for a
// usage mistake.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, euid int, dir string) int {
	// Check this before looking at anything else.
	if err := worker.NotRoot(euid); err != nil {
		fmt.Fprintf(stderr, "waystone-worker: %v\n", err)
		return 1
	}

	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: waystone-worker <read-file|write-file> ...")
		return 2
	}

	switch args[0] {
	case "read-file":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: waystone-worker read-file <name>")
			return 2
		}
		result, err := worker.ReadFile(dir, args[1])
		if err != nil {
			fmt.Fprintf(stderr, "waystone-worker: %v\n", err)
			return 1
		}

		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			fmt.Fprintf(stderr, "waystone-worker: %v\n", err)
			return 1
		}

		return 0

	case "write-file":
		if len(args) != 3 {
			fmt.Fprintln(stderr, "usage: waystone-worker write-file <name> <expect>")
			return 2
		}
		expect, err := worker.ParseExpect(args[2])
		if err != nil {
			fmt.Fprintf(stderr, "waystone-worker: %v\n", err)
			return 1
		}

		// Read one byte more than allowed so we can tell when stdin was too big.
		content, err := io.ReadAll(io.LimitReader(stdin, worker.MaxFileBytes+1))
		if err != nil {
			fmt.Fprintf(stderr, "waystone-worker: %v\n", err)
			return 1
		}

		if len(content) > worker.MaxFileBytes {
			fmt.Fprintln(stderr, "waystone-worker: content exceeds size limit")
			return 1
		}

		err = worker.WriteFile(dir, args[1], content, expect)
		if err != nil && !errors.Is(err, worker.ErrChanged) {
			fmt.Fprintf(stderr, "waystone-worker: %v\n", err)
			return 1
		}

		// A changed file is an answer, not a failure, so it still exits 0.
		outcome := worker.OutcomeWritten
		if errors.Is(err, worker.ErrChanged) {
			outcome = worker.OutcomeChanged
		}

		if err := json.NewEncoder(stdout).Encode(worker.WriteResult{
			Outcome: outcome,
		}); err != nil {
			fmt.Fprintf(stderr, "waystone-worker: %v\n", err)
			return 1
		}

		return 0
	default:
		fmt.Fprintln(stderr, "usage: waystone-worker <read-file|write-file> ...")
		return 2
	}
}
