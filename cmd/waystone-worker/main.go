// SPDX-License-Identifier: AGPL-3.0-only

// Command waystone-worker does file work inside one server's directory. The
// provisioning service starts it with systemd-run, as the server's own user,
// with the server directory as its working directory. It has no socket and
// no root code, it refuses to run as root, and it knows nothing about any
// game.
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

// run is the whole program, minus the process: main passes the real
// arguments, streams, effective uid and working directory, so tests can
// call run with their own. It returns the exit code.
//
//  1. worker.NotRoot(euid) first, before looking at anything else. On
//     error: print it, return 1.
//  2. Usage errors (no verb, unknown verb, wrong number of arguments):
//     print a usage line to stderr, return 2. read-file takes exactly one
//     argument, write-file exactly two.
//  3. read-file <name>: worker.ReadFile(dir, name), then write the result
//     to stdout with a json.Encoder. stdin is never read.
//  4. write-file <name> <expect>: worker.ParseExpect(expect). Read at most
//     worker.MaxFileBytes+1 bytes from stdin; more than MaxFileBytes is an
//     error. Then worker.WriteFile(dir, name, content, expect).
//     - Success: write WriteResult{Outcome: OutcomeWritten} to stdout.
//     - An error wrapping worker.ErrChanged: write
//     WriteResult{Outcome: OutcomeChanged} and return 0. A changed file is
//     an answer, not a failure.
//  5. Any other error: print "waystone-worker: <error>" to stderr, write
//     nothing to stdout, return 1.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, euid int, dir string) int {
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
