// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/sharddevs/waystone_panel/internal/agenttls"
)

// runAgentPin handles "controller agent-pin [-agent-tls-dir <dir>]": it
// prints the pin of the agent link's key, making the key first if there
// is none (the same as the controller's first start). Run it as the
// controller's user, so the key stays that user's; as root it refuses
// (6a-I1), since a key root made is one the service can't read. Exit
// codes: 2 for bad arguments or root, 1 if the key can't be read or made,
// 0 with the pin on stdout.
func runAgentPin(args []string, uid int, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("agent-pin", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("agent-tls-dir", defaultAgentTLSDir, "The agent link's key directory")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: controller agent-pin [-agent-tls-dir <dir>]")
		return 2
	}
	if uid == 0 {
		fmt.Fprintln(stderr, "controller agent-pin: run it as the controller's user: sudo -u waystone-controller controller agent-pin")
		return 2
	}
	cert, err := agenttls.LoadOrCreate(*dir)
	if err != nil {
		fmt.Fprintln(stderr, "agent link key:", err)
		return 1
	}
	fmt.Fprintln(stdout, agenttls.PinOf(cert.Leaf))
	return 0
}
