// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver for database/sql
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// migrationFiles holds the SQL migrations. They're compiled into the
// binary, so a deployed controller can't end up with the wrong files.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// VersionTable is where goose keeps track of which migrations ran. It
// lives in waystone_meta so that every table in the waystone schema is a
// tenant table (I1).
const VersionTable = "waystone_meta.goose_db_version"

// Migrations returns the embedded files with the migrations/ prefix taken
// off, so goose sees "00001_foundation.sql" at the top level.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err) // can't happen: the go:embed line guarantees the directory
	}
	return sub
}

// Migrate applies every migration that hasn't run yet. The dsn must log
// in as waystone_owner, since only the owner may change the schema. The
// controller never migrates on its own at startup (decision B); only
// "controller migrate" and the integration tests call this.
func Migrate(ctx context.Context, dsn string) error {
	// goose wants a database/sql handle rather than a pgx pool, which is
	// why the stdlib driver is imported above.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	// An advisory lock, so two migrate runs at once can't interleave.
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("create migration locker: %w", err)
	}

	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		db,
		Migrations(),
		goose.WithTableName(VersionTable),
		goose.WithSessionLocker(locker),
		goose.WithDisableGlobalRegistry(true), // SQL files only, no Go migrations
	)
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}

	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	return nil
}
