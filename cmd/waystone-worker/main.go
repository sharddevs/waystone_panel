// SPDX-License-Identifier: AGPL-3.0-only

// Command waystone-worker reads, writes and installs files in one game
// server's directory. The root helper runs it through systemd-run as the
// server's own user, with the server directory as its working directory.
// It refuses to run as root.
//
// Usage:
//
//	waystone-worker read-file <name>
//	waystone-worker write-file <name> <absent|sha256>   (content on stdin)
//	waystone-worker install-file <name> <sha256>        (content on stdin)
//
// Each one prints its result as JSON on stdout.
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

// run does all the work and returns the exit code: 0 on success, 1 on an
// error, 2 for bad usage. main hands it the real args, streams, euid and
// directory, so tests can pass in their own instead.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, euid int, dir string) int {
	// Refuse root before touching anything else.
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

		// One byte more than allowed tells "too big" apart from "exactly the limit".
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

		// ErrChanged is a normal answer, not a failure, so we still exit 0
		// and report it in the JSON.
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
	case "install-file":
		if len(args) != 3 {
			fmt.Fprintln(stderr, "usage: waystone-worker install-file <name> <sha256>")
			return 2
		}

		sum, err := worker.ParseSHA256(args[2])
		if err != nil {
			fmt.Fprintf(stderr, "waystone-worker: %v\n", err)
			return 1
		}

		n, err := worker.InstallFile(dir, args[1], stdin, sum)
		if err != nil {
			fmt.Fprintf(stderr, "waystone-worker: %v\n", err)
			return 1
		}

		if err := json.NewEncoder(stdout).Encode(worker.InstallResult{
			Bytes: n,
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
