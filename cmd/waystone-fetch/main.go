// SPDX-License-Identifier: AGPL-3.0-only

// Command waystone-fetch downloads one pinned server jar for the
// controller (stage 6c-3, internal/fetch). systemd starts one per
// connection on waystone-fetch.socket, with the connection as stdin and
// stdout: it reads the request, answers, and exits. It refuses to run as
// root.
//
// Exit codes: 0 fetched, 1 failed, 2 refused (not a pinned hash, or bad
// flags).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/sharddevs/waystone_panel/internal/fetch"
)

func main() {
	log.SetFlags(0) // journald adds timestamps
	os.Exit(run(os.Args[1:], os.Getuid(), os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, uid int, in io.Reader, out, stderr io.Writer) int {
	flags := flag.NewFlagSet("waystone-fetch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", fetch.DefaultDir, "Where a fetched jar waits for the controller")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	switch {
	case flags.NArg() != 0:
		fmt.Fprintf(stderr, "waystone-fetch: unexpected argument %q\n", flags.Arg(0))
		return 2
	case uid == 0:
		fmt.Fprintln(stderr, "waystone-fetch: refusing to run as root")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	f := &fetch.Fetcher{Dir: *dir, Log: log.New(stderr, "", 0)}
	err := f.Serve(ctx, in, out)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, fetch.ErrRefused):
		return 2
	}
	return 1
}
