-- SPDX-License-Identifier: AGPL-3.0-only

-- Stage 6c-1 (notes/stage6-scope.md D7, skeletons/stage6/6c): an admin
-- creates a server, and Waystone installs it and records the EULA.
--
--   * servers gain installed_at and eula_accepted_at. A server the app
--     creates starts with neither (the insert guard clears them), and only
--     the trigger below sets them, when an install or EULA operation
--     succeeds; the app has no grant on them. Servers the owner adds, and
--     those made before this migration, were set up by hand and already
--     run, so they default to now().
--   * the app may INSERT servers (some columns only), through a guard: an
--     admin of the tenant, a node of that tenant that isn't revoked, and a
--     config of exactly the profile's keys. The port and heap limits are
--     CHECKs, so they hold for the owner too (6H4), and a port is unique
--     per node.
--   * operations gain server.install and server.accept_eula, and the
--     install phases provisioning, fetching and configuring.
--   * server.create and node.enrollment.create (6b-H1) are audited as
--     recorded.
--   * the servers this migration marks installed and accepted each get a
--     recorded server.accept_eula row from system/migration-00009, so
--     their history says where the acceptance came from (6c-H1).

-- +goose Up

-- ---------------------------------------------------------------- servers
ALTER TABLE waystone.servers
    ADD COLUMN installed_at timestamptz DEFAULT now(),
    ADD COLUMN eula_accepted_at timestamptz DEFAULT now();

-- A key that is there must be valid; the insert guard says which keys a
-- new server must have. ->> reads a number and a string alike.
ALTER TABLE waystone.servers
    ADD CONSTRAINT servers_config_object CHECK (jsonb_typeof(config) = 'object'),
    ADD CONSTRAINT servers_config_port CHECK (NOT (config ? 'port')
        OR (config->>'port' ~ '^[0-9]{1,5}$' AND (config->>'port')::int BETWEEN 1024 AND 65535)),
    ADD CONSTRAINT servers_config_heap CHECK (NOT (config ? 'heap_mib')
        OR (config->>'heap_mib' ~ '^[0-9]{1,5}$' AND (config->>'heap_mib')::int BETWEEN 512 AND 32768)),
    ADD CONSTRAINT servers_config_version CHECK (NOT (config ? 'version')
        OR (jsonb_typeof(config->'version') = 'string' AND config->>'version' ~ '^[0-9A-Za-z.+-]{1,32}$')),
    ADD CONSTRAINT servers_config_motd CHECK (NOT (config ? 'motd')
        OR (jsonb_typeof(config->'motd') = 'string' AND config->>'motd' ~ '^[\x20-\x5b\x5d-\x7e]{0,64}$'));

-- One server per port on a node: the port reservation (ROADMAP:71).
CREATE UNIQUE INDEX servers_node_port ON waystone.servers (node_id, ((config->>'port')::int))
    WHERE config ? 'port';

GRANT INSERT (tenant_id, server_id, node_id, name, profile, runtime, config, config_version)
    ON waystone.servers TO waystone_app;

-- The insert guard. The column grant already keeps desired_state and the
-- report columns at their defaults, and RLS keeps the row in the actor's
-- own tenant. A new server is neither installed nor accepted.
-- +goose StatementBegin
CREATE FUNCTION waystone.guard_server_insert()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    v_actor_type text := NULLIF(current_setting('app.actor_type', true), '');
    v_actor_id   text := NULLIF(current_setting('app.actor_id', true), '');
BEGIN
    IF current_user <> 'waystone_app' THEN
        RETURN NEW;
    END IF;
    IF v_actor_type NOT IN ('local_user', 'user') OR NOT EXISTS (
        SELECT 1 FROM waystone.memberships m
        WHERE m.tenant_id = NEW.tenant_id
          AND m.actor_type = v_actor_type
          AND m.actor_id = v_actor_id
          AND m.role = 'admin') THEN
        RAISE EXCEPTION 'servers: only an admin may create a server';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM waystone.nodes n
        WHERE n.tenant_id = NEW.tenant_id AND n.node_id = NEW.node_id
          AND n.revoked_at IS NULL) THEN
        RAISE EXCEPTION 'servers: the node is revoked or not in this tenant';
    END IF;
    IF NOT (NEW.config ?& ARRAY['version', 'port', 'heap_mib', 'motd'])
       OR (SELECT count(*) FROM jsonb_object_keys(NEW.config)) <> 4
       OR jsonb_typeof(NEW.config->'port') <> 'number'
       OR jsonb_typeof(NEW.config->'heap_mib') <> 'number' THEN
        RAISE EXCEPTION 'servers: config must be exactly version, port, heap_mib and motd';
    END IF;
    NEW.installed_at := NULL;
    NEW.eula_accepted_at := NULL;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER servers_insert_guard
    BEFORE INSERT ON waystone.servers
    FOR EACH ROW EXECUTE FUNCTION waystone.guard_server_insert();

-- ------------------------------------------------------------- operations
ALTER TABLE waystone.operations DROP CONSTRAINT operations_action_check;
ALTER TABLE waystone.operations ADD CONSTRAINT operations_action_check
    CHECK (action IN ('server.start', 'server.stop', 'server.restart',
                      'server.install', 'server.accept_eula'));
ALTER TABLE waystone.operations DROP CONSTRAINT operations_phase_check;
ALTER TABLE waystone.operations ADD CONSTRAINT operations_phase_check
    CHECK (phase IN ('stopping', 'starting', 'provisioning', 'fetching', 'configuring'));

-- The guard from 00006, word for word, except two rules for the node: an
-- install is claimed in phase provisioning, and its progress moves
-- provisioning → fetching → configuring.
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
              (CASE OLD.action WHEN 'server.restart' THEN 'stopping'
                               WHEN 'server.install' THEN 'provisioning' END) THEN
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

    -- Progress: a restart moves from stopping to starting, and an install
    -- from provisioning to fetching to configuring. Nothing else.
    IF OLD.state = 'running' AND NOT ch_state THEN
        IF OLD.claimed_epoch = epoch
           AND NOT (ch_claim OR ch_result)
           AND ((OLD.action = 'server.restart'
                 AND OLD.phase = 'stopping' AND NEW.phase = 'starting')
             OR (OLD.action = 'server.install'
                 AND ((OLD.phase = 'provisioning' AND NEW.phase = 'fetching')
                   OR (OLD.phase = 'fetching' AND NEW.phase = 'configuring')))) THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'operations: invalid progress';
    END IF;

    RAISE EXCEPTION 'operations: update not allowed for a node';
END;
$$;
-- +goose StatementEnd

-- The app never writes the two columns itself, even with a grant added
-- by mistake later (6c-H2). Only the owner, and so the trigger below,
-- can.
-- +goose StatementBegin
CREATE FUNCTION waystone.guard_server_marks()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF current_user = 'waystone_app'
       AND (NEW.installed_at, NEW.eula_accepted_at)
           IS DISTINCT FROM (OLD.installed_at, OLD.eula_accepted_at) THEN
        RAISE EXCEPTION 'servers: installed_at and eula_accepted_at are set by operations only';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER servers_marks_guard
    BEFORE UPDATE ON waystone.servers
    FOR EACH ROW EXECUTE FUNCTION waystone.guard_server_marks();

-- An install or EULA operation that succeeded, or that a person resolved
-- as succeeded, marks its server: only the server it names, in its own
-- tenant, and only its action's column (6c-H2). It runs as the owner, so
-- the app needs no grant on the two columns.
-- +goose StatementBegin
CREATE FUNCTION waystone.mark_server_from_operation()
RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    -- IS [NOT] DISTINCT FROM throughout: a NULL here must never read as
    -- "succeeded".
    IF NEW.resource_type IS DISTINCT FROM 'server' THEN
        RETURN NULL;
    END IF;
    IF NOT ((NEW.state IS NOT DISTINCT FROM 'succeeded' AND OLD.state IS DISTINCT FROM 'succeeded')
            OR (NEW.resolved_outcome IS NOT DISTINCT FROM 'succeeded'
                AND OLD.resolved_outcome IS DISTINCT FROM 'succeeded')) THEN
        RETURN NULL;
    END IF;
    IF NEW.action = 'server.install' THEN
        UPDATE waystone.servers s SET installed_at = now()
        WHERE s.tenant_id = NEW.tenant_id AND s.server_id = NEW.resource_id;
    ELSIF NEW.action = 'server.accept_eula' THEN
        UPDATE waystone.servers s SET eula_accepted_at = now()
        WHERE s.tenant_id = NEW.tenant_id AND s.server_id = NEW.resource_id;
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION waystone.mark_server_from_operation() FROM PUBLIC;

CREATE TRIGGER operations_mark_server
    AFTER UPDATE OF state, resolved_outcome ON waystone.operations
    FOR EACH ROW
    WHEN (NEW.action IN ('server.install', 'server.accept_eula'))
    EXECUTE FUNCTION waystone.mark_server_from_operation();

-- ---------------------------------------------------------------- audit
ALTER TABLE waystone.audit_events DROP CONSTRAINT audit_events_outcome_check;
ALTER TABLE waystone.audit_events ADD CONSTRAINT audit_events_outcome_check
    CHECK (outcome = 'denied'
        OR (outcome = 'flagged' AND actor_type = 'node')
        OR (outcome = 'recorded' AND actor_type = 'local_user' AND action LIKE 'user.%')
        OR (outcome = 'recorded' AND action IN ('node.enroll', 'node.revoke', 'server.create',
                                                'node.enrollment.create'))
        OR (outcome = 'recorded' AND actor_type = 'system' AND actor_id = 'migration-00009'
            AND action = 'server.accept_eula'));

-- 6c-H1: every server here now was set up by hand in stages 3 and 4, and
-- the columns above marked it installed and accepted. The EULA is never
-- inferred silently, so each one gets a row that says so.
INSERT INTO waystone.audit_events
    (tenant_id, event_id, actor_type, actor_id, actor_name, action,
     resource_type, resource_id, reason, request_id, outcome, at)
SELECT s.tenant_id, gen_random_uuid(), 'system', 'migration-00009', 'migration 00009',
       'server.accept_eula', 'server', s.server_id::text,
       'accepted before 00009, outside Waystone', 'migration-00009', 'recorded', now()
FROM waystone.servers s
WHERE s.eula_accepted_at IS NOT NULL;

-- +goose Down
-- Migrations only go forward (stage 1).
