// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"
)

// migrateTimeout is how long one "controller migrate" run may take before
// we give up, so a stuck lock or database can't hang it forever.
const migrateTimeout = 2 * time.Minute

// runMigrate handles "controller migrate -db <dsn>". args is everything
// after "migrate", and the return value is the exit code:
//   - 2 for a usage problem (unknown flag, missing or empty -db, extra
//     arguments). The usage line goes to stderr and migrate never runs.
//   - 1 if the migration fails, with the error on stderr.
//   - 0 on success, with "migrations applied" on stdout.
//
// stdout only ever gets the success line, so a script can rely on it.
//
// migrate is store.Migrate in the real program. Passing it in lets the
// tests use a fake and check the exit codes without a database.
func runMigrate(args []string, stdout, stderr io.Writer, migrate func(context.Context, string) error) int {
	// ContinueOnError makes Parse return an error instead of exiting the
	// process, so we choose the exit code ourselves.
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)

	flags.SetOutput(stderr)

	dsn := flags.String("db", "", "PostgreSQL connection string")

	usage := func() {
		fmt.Fprintln(stderr, "usage: controller migrate -db <dsn>")
	}
	// On a bad flag, Parse prints the error and then calls flags.Usage, so
	// the usage line appears exactly once.
	flags.Usage = usage
	if err := flags.Parse(args); err != nil {
		return 2
	}

	if *dsn == "" || flags.NArg() != 0 {
		usage()
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), migrateTimeout)
	defer cancel()

	if err := migrate(ctx, *dsn); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	fmt.Fprintln(stdout, "migrations applied")
	return 0
}
