-- SPDX-License-Identifier: AGPL-3.0-only

-- Stage 5a-1 (notes/stage5-scope.md v1.1, §4): browser accounts.
--
--   * A third actor type, "user": a browser account, identified by its
--     user_id (C4). Memberships still decide what it may see.
--   * users: the accounts. The app reads only its own row, never the
--     password hash, and may only change the appearance settings.
--   * sessions: one row per signed-in browser. Only the token's SHA-256
--     is stored. The app sees and deletes only its own sessions.
--   * login_lookup and session_lookup: the only two ways to read across
--     RLS before an actor exists (Q4, safeguard S-Q4).
--   * The operation guard lets an admin user resolve, like a local admin.
--   * The audit log gets a third outcome, "recorded", for the account
--     commands (S-Q6b).
--
-- Nothing here gives waystone_app a write on memberships, or on users
-- beyond the three appearance columns (S-Q6a).

-- +goose Up

-- ------------------------------------------------------ the actor type

ALTER TABLE waystone.memberships DROP CONSTRAINT memberships_actor_type_check;
ALTER TABLE waystone.memberships ADD CONSTRAINT memberships_actor_type_check
    CHECK (actor_type IN ('local_user', 'node', 'user'));

-- ------------------------------------------------------------------ users
-- username is unique across the install, because the login form has no
-- organization field (Q3). tenant_id is the home organization; access
-- still comes only from memberships. The hash is a PHC string, so its
-- parameters can change later without a migration.
CREATE TABLE waystone.users (
    tenant_id uuid NOT NULL REFERENCES waystone.tenants (tenant_id),
    user_id uuid PRIMARY KEY,
    username text NOT NULL UNIQUE CHECK (username ~ '^[a-z0-9][a-z0-9._-]{0,31}$'),
    display_name text NOT NULL CHECK (length(display_name) BETWEEN 1 AND 100),
    password_hash text NOT NULL CHECK (password_hash LIKE '$argon2id$%'),
    theme text NOT NULL DEFAULT 'stone' CHECK (theme IN ('stone', 'slate')),
    density text NOT NULL DEFAULT 'comfortable' CHECK (density IN ('comfortable', 'compact')),
    mode text NOT NULL DEFAULT 'dark' CHECK (mode IN ('dark', 'light', 'system')),
    disabled_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, user_id)
);

ALTER TABLE waystone.users ENABLE ROW LEVEL SECURITY;
ALTER TABLE waystone.users FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON waystone.users TO waystone_owner
    USING (true) WITH CHECK (true);
-- The tenant rule, plus: only the actor's own account.
CREATE POLICY own_user ON waystone.users TO waystone_app
    USING (tenant_id = waystone.current_tenant() AND waystone.actor_is_member(tenant_id)
           AND current_setting('app.actor_type', true) = 'user'
           AND user_id::text = current_setting('app.actor_id', true))
    WITH CHECK (tenant_id = waystone.current_tenant() AND waystone.actor_is_member(tenant_id)
           AND current_setting('app.actor_type', true) = 'user'
           AND user_id::text = current_setting('app.actor_id', true));
-- Every column except password_hash: the app never reads a hash, except
-- through login_lookup.
GRANT SELECT (tenant_id, user_id, username, display_name, theme, density, mode, disabled_at, created_at)
    ON waystone.users TO waystone_app;
GRANT UPDATE (theme, density, mode) ON waystone.users TO waystone_app;

-- --------------------------------------------------------------- sessions
-- One row per signed-in browser. token_sha256 is the SHA-256 of the
-- cookie value, so a copy of this table can't sign anyone in. The CSRF
-- token is a plain random value, bound to this session (C-W5).
CREATE TABLE waystone.sessions (
    tenant_id uuid NOT NULL REFERENCES waystone.tenants (tenant_id),
    session_id uuid PRIMARY KEY,
    user_id uuid NOT NULL,
    token_sha256 bytea NOT NULL UNIQUE CHECK (octet_length(token_sha256) = 32),
    csrf_token text NOT NULL CHECK (csrf_token ~ '^[A-Za-z0-9_-]{43}$'),
    created_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    client_ip text NOT NULL CHECK (octet_length(client_ip) BETWEEN 1 AND 45),
    user_agent text NOT NULL CHECK (octet_length(user_agent) <= 200),
    CHECK (last_seen_at >= created_at AND expires_at > created_at),
    FOREIGN KEY (tenant_id, user_id) REFERENCES waystone.users (tenant_id, user_id) ON DELETE CASCADE
);

ALTER TABLE waystone.sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE waystone.sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON waystone.sessions TO waystone_owner
    USING (true) WITH CHECK (true);
-- The tenant rule, plus: only the actor's own sessions.
CREATE POLICY own_sessions ON waystone.sessions TO waystone_app
    USING (tenant_id = waystone.current_tenant() AND waystone.actor_is_member(tenant_id)
           AND current_setting('app.actor_type', true) = 'user'
           AND user_id::text = current_setting('app.actor_id', true))
    WITH CHECK (tenant_id = waystone.current_tenant() AND waystone.actor_is_member(tenant_id)
           AND current_setting('app.actor_type', true) = 'user'
           AND user_id::text = current_setting('app.actor_id', true));
GRANT SELECT, INSERT, DELETE ON waystone.sessions TO waystone_app;
GRANT UPDATE (last_seen_at) ON waystone.sessions TO waystone_app;

-- ------------------------------------------------- the pre-actor lookups
-- Q4, S-Q4. Before sign-in there is no actor, so RLS shows nothing. These
-- two functions are the only way across, and each one is as narrow as it
-- can be:
--   * SECURITY DEFINER, owned by waystone_owner, whose owner_all policy
--     lets it see the row;
--   * a fixed search_path (pg_catalog, then pg_temp last) and every name
--     schema-qualified, so nothing the caller creates can be picked up;
--   * one argument, the lookup key. No tenant, actor or other context
--     comes from the caller;
--   * at most one row (username and token_sha256 are both UNIQUE), with
--     only the columns listed;
--   * EXECUTE revoked from PUBLIC and granted to waystone_app alone.
-- Expiry is decided in Go, with the store's clock, so the tests control it.

CREATE FUNCTION waystone.login_lookup(p_username text)
RETURNS TABLE (user_id uuid, tenant_id uuid, display_name text, password_hash text, disabled boolean)
LANGUAGE sql STABLE
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
    SELECT u.user_id, u.tenant_id, u.display_name, u.password_hash, u.disabled_at IS NOT NULL
    FROM waystone.users u
    WHERE u.username = p_username
$$;

-- A disabled account's sessions never come back, even if a row survived.
CREATE FUNCTION waystone.session_lookup(p_token_sha256 bytea)
RETURNS TABLE (session_id uuid, user_id uuid, tenant_id uuid, display_name text, csrf_token text,
               created_at timestamptz, last_seen_at timestamptz, expires_at timestamptz)
LANGUAGE sql STABLE
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
    SELECT s.session_id, s.user_id, s.tenant_id, u.display_name, s.csrf_token,
           s.created_at, s.last_seen_at, s.expires_at
    FROM waystone.sessions s
    JOIN waystone.users u ON u.tenant_id = s.tenant_id AND u.user_id = s.user_id
    WHERE s.token_sha256 = p_token_sha256
      AND u.disabled_at IS NULL
$$;

REVOKE ALL ON FUNCTION waystone.login_lookup(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION waystone.session_lookup(bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION waystone.login_lookup(text) TO waystone_app;
GRANT EXECUTE ON FUNCTION waystone.session_lookup(bytea) TO waystone_app;

-- ------------------------------------------------------- the audit log
-- "recorded": an account command run by a local admin (S-Q6b). Only the
-- owner writes these (the app has no reason to), so the CHECK ties them
-- to a local_user actor and a user.* action.
ALTER TABLE waystone.audit_events DROP CONSTRAINT audit_events_outcome_check;
ALTER TABLE waystone.audit_events ADD CONSTRAINT audit_events_outcome_check
    CHECK (outcome = 'denied'
        OR (outcome = 'flagged' AND actor_type = 'node')
        OR (outcome = 'recorded' AND actor_type = 'local_user' AND action LIKE 'user.%'));

-- ------------------------------------------------- the operation guard
-- The function from 00004, word for word, except the admin rule (layer 3):
-- a "user" admin may resolve too, and the membership it needs is one of
-- its own type. Only those two lines differ.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION waystone.guard_operation_history()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    v_actor_type text := NULLIF(current_setting('app.actor_type', true), '');
    v_actor_id   text := NULLIF(current_setting('app.actor_id', true), '');
    epoch      bigint;
    ch_state      boolean := NEW.state IS DISTINCT FROM OLD.state;
    ch_phase      boolean := NEW.phase IS DISTINCT FROM OLD.phase;
    ch_claim      boolean := (NEW.claimed_epoch, NEW.started_at)
                             IS DISTINCT FROM (OLD.claimed_epoch, OLD.started_at);
    ch_result     boolean := (NEW.finished_at, NEW.error_code, NEW.error_summary)
                             IS DISTINCT FROM (OLD.finished_at, OLD.error_code, OLD.error_summary);
    ch_resolution boolean := (NEW.resolved_at, NEW.resolved_outcome, NEW.resolved_by_type,
                              NEW.resolved_by_id, NEW.resolved_by_name, NEW.resolution_note)
                             IS DISTINCT FROM
                             (OLD.resolved_at, OLD.resolved_outcome, OLD.resolved_by_type,
                              OLD.resolved_by_id, OLD.resolved_by_name, OLD.resolution_note);
BEGIN
    -- Layer 1: history. Unchanged from 00001.
    IF OLD.state IN ('succeeded', 'failed') THEN
        RAISE EXCEPTION 'finished operations cannot be updated';
    END IF;

    IF OLD.state = 'uncertain' THEN
        IF NEW.state IS DISTINCT FROM 'uncertain'
           OR OLD.resolved_at IS NOT NULL
           OR NEW.resolved_at IS NULL THEN
            RAISE EXCEPTION 'invalid uncertain operation update';
        END IF;
    ELSIF ch_state THEN
        IF NOT (
            (OLD.state = 'pending' AND NEW.state IN ('running', 'failed'))
            OR (OLD.state = 'running'
                AND NEW.state IN ('succeeded', 'failed', 'uncertain'))
        ) THEN
            RAISE EXCEPTION 'invalid operation state transition';
        END IF;
    END IF;

    -- Layer 2: what was asked for, by whom, and when, is fixed.
    IF (NEW.tenant_id, NEW.operation_id, NEW.resource_type, NEW.resource_id, NEW.node_id,
        NEW.action, NEW.actor_type, NEW.actor_id, NEW.actor_name, NEW.requested_at)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.operation_id, OLD.resource_type, OLD.resource_id, OLD.node_id,
        OLD.action, OLD.actor_type, OLD.actor_id, OLD.actor_name, OLD.requested_at) THEN
        RAISE EXCEPTION 'operations: identity columns cannot change';
    END IF;

    -- Layer 3, the owner: the recovery function, or a deliberate repair.
    IF current_user = 'waystone_owner' THEN
        IF current_setting('waystone.owner_override', true) = 'on' THEN
            RETURN NEW;
        END IF;
        IF OLD.state = 'running' AND NEW.state = 'uncertain'
           AND NOT (ch_phase OR ch_claim OR ch_resolution) THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'operations: the owner may only mark running operations uncertain';
    END IF;

    IF current_user <> 'waystone_app' THEN
        RAISE EXCEPTION 'operations: % may not update operations', current_user;
    END IF;

    -- Layer 3, an admin: resolving an unresolved uncertain row in a
    -- tenant they administer, under their own name, and nothing else.
    -- Layer 1 already made sure the row is uncertain and unresolved.
    IF v_actor_type IN ('local_user', 'user') THEN
        IF OLD.state = 'uncertain'
           AND ch_resolution AND NOT (ch_phase OR ch_claim OR ch_result)
           AND NEW.resolved_by_type IS NOT DISTINCT FROM v_actor_type
           AND NEW.resolved_by_id IS NOT DISTINCT FROM v_actor_id
           AND EXISTS (
               SELECT 1 FROM waystone.memberships m
               WHERE m.tenant_id = OLD.tenant_id
                 AND m.actor_type = v_actor_type
                 AND m.actor_id = v_actor_id
                 AND m.role = 'admin') THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'operations: an admin may only resolve an uncertain operation';
    END IF;

    -- Layer 3, the operation's own node. Nothing else is left.
    IF v_actor_type IS DISTINCT FROM 'node' OR v_actor_id IS DISTINCT FROM OLD.node_id::text THEN
        RAISE EXCEPTION 'operations: only the operation''s node may change it';
    END IF;
    IF ch_resolution THEN
        RAISE EXCEPTION 'operations: a node cannot resolve an operation';
    END IF;

    -- The node's current session. RLS applies here too, and a node can
    -- see its own row.
    SELECT n.session_epoch INTO epoch
    FROM waystone.nodes n
    WHERE n.node_id = OLD.node_id;
    IF epoch IS NULL THEN
        RAISE EXCEPTION 'operations: the node is not visible';
    END IF;

    -- Claim (I9): stamped with the current session, never anything else.
    IF OLD.state = 'pending' AND NEW.state = 'running' THEN
        IF NEW.claimed_epoch IS DISTINCT FROM epoch
           OR NEW.started_at IS NULL
           OR ch_result
           OR NEW.phase IS DISTINCT FROM
              (CASE WHEN OLD.action = 'server.restart' THEN 'stopping' END) THEN
            RAISE EXCEPTION 'operations: invalid claim';
        END IF;
        RETURN NEW;
    END IF;

    -- F10: refused before it was ever sent.
    IF OLD.state = 'pending' AND NEW.state = 'failed' THEN
        IF ch_phase OR ch_claim OR NEW.finished_at IS NULL OR NEW.error_code IS NULL THEN
            RAISE EXCEPTION 'operations: invalid refusal';
        END IF;
        RETURN NEW;
    END IF;

    IF OLD.state = 'running' AND ch_state THEN
        IF ch_claim OR NEW.finished_at IS NULL THEN
            RAISE EXCEPTION 'operations: invalid result';
        END IF;
        -- C3: a result from the session that claimed it.
        IF OLD.claimed_epoch = epoch THEN
            RETURN NEW;
        END IF;
        -- I8, inside the hello: an older session's operation can only
        -- become uncertain, and its phase stays as the old session left it.
        IF NEW.state = 'uncertain' AND NOT ch_phase
           AND (OLD.claimed_epoch IS NULL OR OLD.claimed_epoch < epoch) THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'operations: result from a superseded session';
    END IF;

    -- Progress: a restart moves from stopping to starting, nothing else.
    IF OLD.state = 'running' AND NOT ch_state THEN
        IF OLD.claimed_epoch = epoch
           AND OLD.action = 'server.restart'
           AND OLD.phase = 'stopping' AND NEW.phase = 'starting'
           AND NOT (ch_claim OR ch_result) THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'operations: invalid progress';
    END IF;

    RAISE EXCEPTION 'operations: update not allowed for a node';
END;
$$;
-- +goose StatementEnd
