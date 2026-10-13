-- SPDX-License-Identifier: AGPL-3.0-only
--
-- The foundation schema for stage 4a (notes/stage4-scope.md v3.1). goose
-- runs this file as waystone_owner, all in one transaction. Migrations
-- only go forward (decision B), so there is no Down section.
--
-- Two goose details worth knowing:
--   * goose splits the file at semicolons. A plpgsql function has
--     semicolons inside its body, so it sits between StatementBegin and
--     StatementEnd markers, which tell goose to keep it in one piece.
--   * PostgreSQL checks a SQL function's body when the function is
--     created, so each table has to exist before any function reading it.
--
-- Every table follows the same pattern:
--   1. CREATE TABLE with tenant_id uuid NOT NULL (I1);
--   2. ENABLE and FORCE row level security. FORCE means the policies
--      apply to the table's owner too;
--   3. an owner_all policy for waystone_owner, because migrations and
--      the seed run as the owner and FORCE would otherwise block them;
--   4. a tenant_rows policy for waystone_app, with the same rule for
--      reading (USING) and writing (WITH CHECK) (I2, I5);
--   5. grants to waystone_app for only what stage 4 needs. UPDATE is
--      granted column by column.

-- +goose Up
GRANT USAGE ON SCHEMA waystone TO waystone_app;

-- ---------------------------------------------------------------- tenants

CREATE TABLE waystone.tenants (
    tenant_id  uuid PRIMARY KEY,
    name       text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------ memberships

-- Who may act in which tenant (C5). Only the owner writes this table.
CREATE TABLE waystone.memberships (
    tenant_id  uuid NOT NULL REFERENCES waystone.tenants (tenant_id),
    actor_type text NOT NULL CHECK (actor_type IN ('local_user', 'node')),
    actor_id   text NOT NULL CHECK (length(actor_id) BETWEEN 1 AND 64),
    role       text NOT NULL CHECK (role IN ('admin', 'node')),
    PRIMARY KEY (tenant_id, actor_type, actor_id)
);

-- ------------------------------------------------- policy helper functions

-- The transaction's tenant, or NULL when it isn't set. A transaction-local
-- setting reads as '' (not NULL) once an earlier transaction on the same
-- connection has ended, and ''::uuid would raise an error, hence NULLIF.
-- NULL compares as unknown, so a policy using this matches no row.
CREATE FUNCTION waystone.current_tenant() RETURNS uuid
LANGUAGE sql STABLE
AS $$ SELECT NULLIF(current_setting('app.tenant_id', true), '')::uuid $$;

-- True if the transaction's actor has a membership in tenant t. It runs
-- with the caller's rights (no SECURITY DEFINER), so the memberships
-- policy below filters it. That policy reads only the actor settings and
-- no other table, so there is no recursion.
CREATE FUNCTION waystone.actor_is_member(t uuid) RETURNS boolean
LANGUAGE sql STABLE
AS $$
    SELECT EXISTS (
        SELECT 1 FROM waystone.memberships m
        WHERE m.tenant_id = t
          AND m.actor_type = current_setting('app.actor_type', true)
          AND m.actor_id = current_setting('app.actor_id', true))
$$;

REVOKE ALL ON FUNCTION waystone.current_tenant() FROM PUBLIC;
REVOKE ALL ON FUNCTION waystone.actor_is_member(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION waystone.current_tenant() TO waystone_app;
GRANT EXECUTE ON FUNCTION waystone.actor_is_member(uuid) TO waystone_app;

-- ------------------------------------------- RLS for tenants and memberships

ALTER TABLE waystone.tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE waystone.tenants FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON waystone.tenants TO waystone_owner USING (true) WITH CHECK (true);
CREATE POLICY tenant_rows ON waystone.tenants TO waystone_app
    USING (tenant_id = waystone.current_tenant() AND waystone.actor_is_member(tenant_id))
    WITH CHECK (tenant_id = waystone.current_tenant() AND waystone.actor_is_member(tenant_id));
GRANT SELECT ON waystone.tenants TO waystone_app;

ALTER TABLE waystone.memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE waystone.memberships FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON waystone.memberships TO waystone_owner USING (true) WITH CHECK (true);
-- The one policy that ignores app.tenant_id: the lookup in InTenantTx runs
-- before the tenant is known. SELECT only; the app can't write here.
CREATE POLICY own_memberships ON waystone.memberships FOR SELECT TO waystone_app
    USING (actor_type = current_setting('app.actor_type', true)
       AND actor_id = current_setting('app.actor_id', true));
GRANT SELECT ON waystone.memberships TO waystone_app;

-- ============================================================ the rest
-- From here on every table has the same tenant_rows rule: waystone_app
-- sees a row only when the transaction's tenant matches it and the actor
-- is a member of that tenant.

-- ---------------------------------------------------------------- nodes
-- session_epoch goes up each time an agent session takes over, so
-- messages from an older connection can be recognized and ignored (4c).
-- UNIQUE (tenant_id, node_id) exists so servers can reference both.
CREATE TABLE waystone.nodes (
    tenant_id uuid NOT NULL REFERENCES waystone.tenants (tenant_id),
    node_id uuid PRIMARY KEY,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    session_epoch bigint NOT NULL DEFAULT 0,
    last_seen_at timestamptz,
    UNIQUE (tenant_id, node_id)
);

ALTER TABLE waystone.nodes ENABLE ROW LEVEL SECURITY;
ALTER TABLE waystone.nodes FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON waystone.nodes TO waystone_owner
    USING (true) WITH CHECK (true);
CREATE POLICY tenant_rows ON waystone.nodes TO waystone_app
    USING (tenant_id = waystone.current_tenant() AND waystone.actor_is_member(tenant_id))
    WITH CHECK (tenant_id = waystone.current_tenant() AND waystone.actor_is_member(tenant_id));
GRANT SELECT ON waystone.nodes TO waystone_app;

-- --------------------------------------------------------------- servers
-- desired_state is what the user asked for. process_state and readiness
-- are what the agent last saw, each with when it changed and when it was
-- last checked. The report_* columns are for 4b.
-- The two-column foreign key means a server can only point at a node in
-- its own tenant.
CREATE TABLE waystone.servers (
    tenant_id uuid NOT NULL REFERENCES waystone.tenants (tenant_id),
    server_id uuid PRIMARY KEY,
    node_id uuid NOT NULL,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    profile text NOT NULL CHECK (profile IN ('minecraft-java')),
    runtime text NOT NULL CHECK (runtime IN ('systemd')),
    config jsonb NOT NULL DEFAULT '{}'::jsonb,
    config_version integer NOT NULL DEFAULT 1,
    desired_state text NOT NULL DEFAULT 'stopped'
        CHECK (desired_state IN ('running', 'stopped')),
    process_state text NOT NULL DEFAULT 'unknown'
        CHECK (process_state IN ('stopped', 'starting', 'running', 'stopping', 'failed', 'unknown')),
    process_since timestamptz,
    process_observed_at timestamptz,
    readiness text NOT NULL DEFAULT 'unknown'
        CHECK (readiness IN ('ready', 'not_ready', 'unknown', 'not_supported')),
    readiness_since timestamptz,
    readiness_observed_at timestamptz,
    report_epoch bigint NOT NULL DEFAULT 0,
    report_seq bigint NOT NULL DEFAULT 0,
    agent_reported_at timestamptz,
    UNIQUE (tenant_id, server_id),
    FOREIGN KEY (tenant_id, node_id) REFERENCES waystone.nodes (tenant_id, node_id)
);

ALTER TABLE waystone.servers ENABLE ROW LEVEL SECURITY;
ALTER TABLE waystone.servers FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON waystone.servers TO waystone_owner
    USING (true) WITH CHECK (true);
CREATE POLICY tenant_rows ON waystone.servers TO waystone_app
    USING (tenant_id = waystone.current_tenant() AND waystone.actor_is_member(tenant_id))
    WITH CHECK (tenant_id = waystone.current_tenant() AND waystone.actor_is_member(tenant_id));
GRANT SELECT ON waystone.servers TO waystone_app;
-- SELECT ... FOR UPDATE needs UPDATE on at least one column, and
-- desired_state is the one column the app does change.
GRANT UPDATE (desired_state) ON waystone.servers TO waystone_app;

-- ------------------------------------------------------------- operations
-- resource_id, node_id and the actor columns are copies taken at request
-- time, not foreign keys. History has to outlive the server and the node.
-- The last two CHECKs: a resolution is all or nothing, and only an
-- uncertain operation can have one.
CREATE TABLE waystone.operations (
    tenant_id uuid NOT NULL REFERENCES waystone.tenants (tenant_id),
    operation_id uuid PRIMARY KEY,
    resource_type text NOT NULL CHECK (resource_type = 'server'),
    resource_id uuid NOT NULL,
    node_id uuid NOT NULL,
    action text NOT NULL CHECK (action IN ('server.start', 'server.stop', 'server.restart')),
    state text NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'uncertain')),
    phase text CHECK (phase IN ('stopping', 'starting')),
    actor_type text NOT NULL,
    actor_id text NOT NULL,
    actor_name text NOT NULL,
    error_code text,
    error_summary text,
    requested_at timestamptz NOT NULL,
    started_at timestamptz,
    finished_at timestamptz,
    claimed_epoch bigint,
    resolved_at timestamptz,
    resolved_outcome text CHECK (resolved_outcome IN ('succeeded', 'failed')),
    resolved_by_type text,
    resolved_by_id text,
    resolved_by_name text,
    resolution_note text CHECK (length(resolution_note) <= 500),
    CHECK ((resolved_at IS NULL) = (resolved_outcome IS NULL)),
    CHECK (resolved_at IS NULL OR state = 'uncertain')
);

-- I3: at most one unfinished operation per server. An uncertain one
-- counts as unfinished until someone resolves it, because nobody knows
-- yet whether it really ran.
CREATE UNIQUE INDEX operations_one_unfinished
    ON waystone.operations (tenant_id, resource_type, resource_id)
    WHERE state IN ('pending', 'running')
       OR (state = 'uncertain' AND resolved_at IS NULL);

-- I11: this trigger enforces the allowed state changes in the database
-- itself, so a bug in the Go code can't rewrite history. Finished rows
-- never change, an uncertain row can only be resolved once, and the state
-- can only move along pending -> running -> succeeded/failed/uncertain
-- (or straight from pending to failed).
-- +goose StatementBegin
CREATE FUNCTION waystone.guard_operation_history()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    -- A finished operation is historical evidence; never rewrite it.
    IF OLD.state IN ('succeeded', 'failed') THEN
        RAISE EXCEPTION 'finished operations cannot be updated';
    END IF;

    -- Resolution records an outcome but intentionally leaves state uncertain.
    -- Only the first resolution is allowed.
    IF OLD.state = 'uncertain' THEN
        IF NEW.state IS DISTINCT FROM 'uncertain'
           OR OLD.resolved_at IS NOT NULL
           OR NEW.resolved_at IS NULL THEN
            RAISE EXCEPTION 'invalid uncertain operation update';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.state IS DISTINCT FROM OLD.state THEN
        IF NOT (
            (OLD.state = 'pending' AND NEW.state IN ('running', 'failed'))
            OR (OLD.state = 'running'
                AND NEW.state IN ('succeeded', 'failed', 'uncertain'))
        ) THEN
            RAISE EXCEPTION 'invalid operation state transition';
        END IF;
    END IF;

    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER operation_history_guard
BEFORE UPDATE ON waystone.operations
FOR EACH ROW
EXECUTE FUNCTION waystone.guard_operation_history();

ALTER TABLE waystone.operations ENABLE ROW LEVEL SECURITY;
ALTER TABLE waystone.operations FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON waystone.operations TO waystone_owner
    USING (true) WITH CHECK (true);
CREATE POLICY tenant_rows ON waystone.operations TO waystone_app
    USING (tenant_id = waystone.current_tenant() AND waystone.actor_is_member(tenant_id))
    WITH CHECK (tenant_id = waystone.current_tenant() AND waystone.actor_is_member(tenant_id));
GRANT SELECT, INSERT ON waystone.operations TO waystone_app;
-- The actor, resource, action and requested_at stay as they were
-- inserted. No DELETE either: history is never removed.
GRANT UPDATE (
    state, phase, started_at, finished_at, error_code, error_summary,
    claimed_epoch, resolved_at, resolved_outcome, resolved_by_type,
    resolved_by_id, resolved_by_name, resolution_note
) ON waystone.operations TO waystone_app;

-- ------------------------------------------------------ idempotency_keys
-- C1: a key belongs to one actor in one tenant and points at the
-- operation it created. CreateOperation decides when an old key may be
-- reused, inside its own transaction.
CREATE TABLE waystone.idempotency_keys (
    tenant_id uuid NOT NULL REFERENCES waystone.tenants (tenant_id),
    actor_type text NOT NULL,
    actor_id text NOT NULL,
    key text NOT NULL CHECK (key ~ '^[A-Za-z0-9_-]{1,64}$'),
    request_sha256 bytea NOT NULL CHECK (octet_length(request_sha256) = 32),
    operation_id uuid NOT NULL REFERENCES waystone.operations (operation_id),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, actor_type, actor_id, key)
);

ALTER TABLE waystone.idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE waystone.idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON waystone.idempotency_keys TO waystone_owner
    USING (true) WITH CHECK (true);
CREATE POLICY tenant_rows ON waystone.idempotency_keys TO waystone_app
    USING (tenant_id = waystone.current_tenant() AND waystone.actor_is_member(tenant_id))
    WITH CHECK (tenant_id = waystone.current_tenant() AND waystone.actor_is_member(tenant_id));
GRANT SELECT, INSERT ON waystone.idempotency_keys TO waystone_app;
GRANT UPDATE (request_sha256, operation_id, created_at)
    ON waystone.idempotency_keys TO waystone_app;

-- --------------------------------------------------------- audit_events
-- Denied requests (H4). The app can add rows and read them, but never
-- change or delete them.
CREATE TABLE waystone.audit_events (
    tenant_id uuid NOT NULL REFERENCES waystone.tenants (tenant_id),
    event_id uuid PRIMARY KEY,
    actor_type text NOT NULL,
    actor_id text NOT NULL,
    actor_name text NOT NULL,
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id text NOT NULL,
    reason text NOT NULL,
    request_id text NOT NULL,
    outcome text NOT NULL CHECK (outcome = 'denied'),
    at timestamptz NOT NULL
);

ALTER TABLE waystone.audit_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE waystone.audit_events FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON waystone.audit_events TO waystone_owner
    USING (true) WITH CHECK (true);
CREATE POLICY tenant_rows ON waystone.audit_events TO waystone_app
    USING (tenant_id = waystone.current_tenant() AND waystone.actor_is_member(tenant_id))
    WITH CHECK (tenant_id = waystone.current_tenant() AND waystone.actor_is_member(tenant_id));
GRANT SELECT, INSERT ON waystone.audit_events TO waystone_app;
