// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package store

import (
	"context"
	"slices"
	"testing"
)

// Stage 5a-1, safeguards S-Q4 and S-Q6a, checked in the catalog.

// S-Q4: the two lookups are the only new way across RLS, and each is
// locked down: SECURITY DEFINER owned by waystone_owner, a fixed
// search_path, one argument, EXECUTE for waystone_app and nobody else.
// purge_sessions (00007, I4) is held to the same, with no argument.
func TestLookupFunctionsLockedDown(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	for name, args := range map[string]string{
		"login_lookup":   "p_username text",
		"session_lookup": "p_token_sha256 bytea",
		"purge_sessions": "",
	} {
		var definer bool
		var owner, identity string
		var config []string
		err := e.owner.QueryRow(ctx, `
			SELECT p.prosecdef, pg_get_userbyid(p.proowner), pg_get_function_identity_arguments(p.oid),
			       coalesce(p.proconfig, '{}')
			FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = 'waystone' AND p.proname = $1`, name).Scan(&definer, &owner, &identity, &config)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !definer || owner != "waystone_owner" {
			t.Errorf("%s: security definer=%v owner=%s, want true and waystone_owner", name, definer, owner)
		}
		if identity != args {
			t.Errorf("%s: arguments %q, want exactly %q", name, identity, args)
		}
		if !slices.Equal(config, []string{"search_path=pg_catalog, pg_temp"}) {
			t.Errorf("%s: config %q, want only search_path=pg_catalog, pg_temp", name, config)
		}

		// Who may run it: the owner (implicitly) and waystone_app, never
		// PUBLIC (grantee 0) or anyone else.
		rows, err := e.owner.Query(ctx, `
			SELECT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END
			FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace,
			     aclexplode(coalesce(p.proacl, acldefault('f', p.proowner))) a
			WHERE n.nspname = 'waystone' AND p.proname = $1 AND a.privilege_type = 'EXECUTE'`, name)
		if err != nil {
			t.Fatal(err)
		}
		var grantees []string
		for rows.Next() {
			var g string
			if err := rows.Scan(&g); err != nil {
				t.Fatal(err)
			}
			grantees = append(grantees, g)
		}
		rows.Close()
		slices.Sort(grantees)
		if !slices.Equal(grantees, []string{"waystone_app", "waystone_owner"}) {
			t.Errorf("%s: EXECUTE granted to %v, want only waystone_app (and the owner)", name, grantees)
		}
	}
}

// Every SECURITY DEFINER function in the schema is a known one, and each
// pins its search_path.
func TestOnlyKnownSecurityDefinerFunctions(t *testing.T) {
	e := setup(t)
	rows, err := e.owner.Query(context.Background(), `
		SELECT p.proname, coalesce(array_to_string(p.proconfig, ','), '')
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'waystone' AND p.prosecdef
		ORDER BY p.proname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name, config string
		if err := rows.Scan(&name, &config); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
		if !slices.Contains([]string{"search_path=pg_catalog, pg_temp", "search_path=pg_catalog, waystone"}, config) {
			t.Errorf("%s: config %q has no fixed search_path", name, config)
		}
	}
	want := []string{"login_lookup", "purge_sessions", "recover_running_operations", "session_lookup"}
	if !slices.Equal(names, want) {
		t.Fatalf("SECURITY DEFINER functions %v, want %v", names, want)
	}
}

// S-Q6a: waystone_app can never create an account or a membership. Its
// only write on users is the three appearance columns, and it can't even
// read a password hash.
func TestAppCannotWriteAccounts(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	check := func(sql string, want bool) {
		t.Helper()
		var got bool
		if err := e.owner.QueryRow(ctx, sql).Scan(&got); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		if got != want {
			t.Errorf("%s = %v, want %v", sql, got, want)
		}
	}
	for _, priv := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE"} {
		check(`SELECT has_table_privilege('waystone_app', 'waystone.memberships', '`+priv+`')`, false)
	}
	for _, priv := range []string{"INSERT", "DELETE", "TRUNCATE"} {
		check(`SELECT has_table_privilege('waystone_app', 'waystone.users', '`+priv+`')`, false)
	}
	for _, col := range []string{"tenant_id", "user_id", "username", "display_name", "password_hash", "disabled_at", "created_at"} {
		check(`SELECT has_column_privilege('waystone_app', 'waystone.users', '`+col+`', 'UPDATE')`, false)
	}
	for _, col := range []string{"theme", "density", "mode"} {
		check(`SELECT has_column_privilege('waystone_app', 'waystone.users', '`+col+`', 'UPDATE')`, true)
	}
	check(`SELECT has_column_privilege('waystone_app', 'waystone.users', 'password_hash', 'SELECT')`, false)
	check(`SELECT has_column_privilege('waystone_app', 'waystone.users', 'username', 'SELECT')`, true)
	for _, col := range []string{"tenant_id", "session_id", "user_id", "token_sha256", "csrf_token", "created_at", "expires_at", "client_ip", "user_agent"} {
		check(`SELECT has_column_privilege('waystone_app', 'waystone.sessions', '`+col+`', 'UPDATE')`, false)
	}
	check(`SELECT has_column_privilege('waystone_app', 'waystone.sessions', 'last_seen_at', 'UPDATE')`, true)

	// And in practice, with a browser actor's settings in place.
	app := appConn(t)
	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_type', 'user', true), set_config('app.actor_id', '0199e1a0-0000-7000-8000-0000000000f1', true), set_config('app.tenant_id', $1, true)`, tenantA); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO waystone.memberships (tenant_id, actor_type, actor_id, role)
		VALUES ($1, 'user', '0199e1a0-0000-7000-8000-0000000000f1', 'admin')`, tenantA)
	if code := sqlState(err); code != "42501" {
		t.Fatalf("app inserting a membership: %v (state %s), want insufficient_privilege (42501)", err, code)
	}
}

// The lookup keys are UNIQUE, so each function returns at most one row.
func TestLookupKeysUnique(t *testing.T) {
	e := setup(t)
	for _, col := range []string{"users.username", "sessions.token_sha256"} {
		var n int
		if err := e.owner.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_index i
			JOIN pg_class c ON c.oid = i.indrelid JOIN pg_namespace ns ON ns.oid = c.relnamespace
			JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = i.indkey[0]
			WHERE ns.nspname = 'waystone' AND i.indisunique AND i.indnatts = 1
			  AND c.relname || '.' || a.attname = $1`, col).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("%s: %d single-column unique indexes, want 1", col, n)
		}
	}
}
