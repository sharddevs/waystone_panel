-- SPDX-License-Identifier: AGPL-3.0-only

-- Stage 4c-1 (notes/stage4c-scope.md v2.1): operations actually run.
--
--   * recover_running_operations(), the controller's startup recovery (I7).
--   * The operation history trigger learns who may make each change (Q5,
--     S4). Before this, any waystone_app session with a membership could
--     move an operation along; only the Go code kept it honest.
--   * An index for the claim: the oldest pending operation of a node.
--
-- No grants change. Every column the claim and the results write is
-- already in waystone_app's UPDATE grant from 00001.

-- +goose Up

-- ------------------------------------------------- the history trigger
-- The same function name, so the trigger from 00001 keeps calling it.
--
-- Three layers, in order:
--   1. The history rules from 00001, for everyone, the owner included:
--      finished rows never change, an uncertain row is resolved at most
--      once, and the state only moves forward.
--   2. The identity columns never change. waystone_app can't touch them
--      anyway (column grant); this also holds the owner to it.
--   3. Who is asking (S4). Each allowed change lists the columns it may
--      set; every other granted column must stay exactly as it was.
--
-- waystone.owner_override: a setting the owner may turn on for a manual
-- repair or a test fixture. It skips layer 3 only, never 1 or 2, and only
-- when the session really is waystone_owner. The owner can already
-- disable this trigger outright, so it gives nobody anything new.
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
    IF v_actor_type = 'local_user' THEN
        IF OLD.state = 'uncertain'
           AND ch_resolution AND NOT (ch_phase OR ch_claim OR ch_result)
           AND NEW.resolved_by_type IS NOT DISTINCT FROM v_actor_type
           AND NEW.resolved_by_id IS NOT DISTINCT FROM v_actor_id
           AND EXISTS (
               SELECT 1 FROM waystone.memberships m
               WHERE m.tenant_id = OLD.tenant_id
                 AND m.actor_type = 'local_user'
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

-- ------------------------------------------------------ startup recovery
-- I7, F2. The controller calls this once at startup, before it listens:
-- whatever was running when the last controller stopped may or may not
-- have happened, so it becomes uncertain. It runs as the owner, across
-- every tenant, and can do exactly this one UPDATE: no parameters and a
-- fixed search_path. The owner override makes no difference here, since
-- the owner rule already allows exactly this change. It assumes one
-- controller per database (L7).
-- +goose StatementBegin
CREATE FUNCTION waystone.recover_running_operations()
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, waystone
AS $$
DECLARE
    n integer;
BEGIN
    UPDATE waystone.operations
    SET state = 'uncertain', finished_at = now(),
        error_code = 'controller_restarted',
        error_summary = 'The controller restarted while this operation was running.'
    WHERE state = 'running';
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END;
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION waystone.recover_running_operations() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION waystone.recover_running_operations() TO waystone_app;

-- ------------------------------------------------------------- the claim
CREATE INDEX operations_pending_by_node
    ON waystone.operations (node_id, requested_at)
    WHERE state = 'pending';
