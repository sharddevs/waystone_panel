-- SPDX-License-Identifier: AGPL-3.0-only

-- Stage 7b (notes/stage7-scope.md D2, draft 2): an admin sends one
-- command line to a running game's console.
--
--   * operations gain server.command, and config: the command line, kept
--     with its operation. Only server.command rows carry it, as exactly
--     {"command": <line>}, and the line follows console.ValidLine: 1 to
--     256 bytes, no control or bidi character, no leading "/". These are
--     CHECKs, so they hold for the owner too.
--   * config never changes after the insert (H2): the app has no UPDATE
--     grant on it, and the guard counts it among the identity columns.
--     The app's INSERT grant becomes a column list.
--   * a denied server.command or server.logs.read is audited like any
--     other denial; nothing new is recorded.
--
-- The agent journal and its node resolution path (7c, 7M3) come in a
-- later migration.

-- +goose Up

-- ------------------------------------------------------------- operations
ALTER TABLE waystone.operations ADD COLUMN config jsonb;

ALTER TABLE waystone.operations DROP CONSTRAINT operations_action_check;
ALTER TABLE waystone.operations ADD CONSTRAINT operations_action_check
    CHECK (action IN ('server.start', 'server.stop', 'server.restart',
                      'server.install', 'server.accept_eula', 'server.command'));

ALTER TABLE waystone.operations ADD CONSTRAINT operations_config_check
    CHECK ((action = 'server.command') = (config IS NOT NULL)
       AND (config IS NULL
            OR (jsonb_typeof(config) = 'object'
                AND config - 'command' = '{}'::jsonb
                AND jsonb_typeof(config -> 'command') = 'string'
                AND octet_length(config ->> 'command') BETWEEN 1 AND 256
                AND left(config ->> 'command', 1) <> '/'
                AND config ->> 'command'
                    !~ '[\x01-\x1f\x7f-\x9f\u061c\u200e\u200f\u202a-\u202e\u2066-\u2069]')));

REVOKE INSERT ON waystone.operations FROM waystone_app;
GRANT INSERT (tenant_id, operation_id, resource_type, resource_id, node_id, action, state,
              actor_type, actor_id, actor_name, requested_at, config)
    ON waystone.operations TO waystone_app;

-- The guard from 00009, word for word, except that config joins the
-- identity columns.
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
    -- 00010: config too (H2), for the owner as well.
    IF (NEW.tenant_id, NEW.operation_id, NEW.resource_type, NEW.resource_id, NEW.node_id,
        NEW.action, NEW.actor_type, NEW.actor_id, NEW.actor_name, NEW.requested_at, NEW.config)
       IS DISTINCT FROM
       (OLD.tenant_id, OLD.operation_id, OLD.resource_type, OLD.resource_id, OLD.node_id,
        OLD.action, OLD.actor_type, OLD.actor_id, OLD.actor_name, OLD.requested_at, OLD.config) THEN
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

-- +goose Down
-- Migrations only go forward (stage 1).
