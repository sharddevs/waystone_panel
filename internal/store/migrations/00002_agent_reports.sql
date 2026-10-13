-- SPDX-License-Identifier: AGPL-3.0-only

-- Stage 4b-1 (notes/stage4b-scope.md v2.1): agents say hello and report
-- what their servers are doing. waystone_app gets the few extra columns
-- it needs for that, and two triggers make sure only the right node, in
-- the right session, can write them (R4). Column grants alone can't tell
-- an admin from a node, because both log in as waystone_app.
--
-- Both triggers leave the owner alone (current_user check), so migrations
-- and seed scripts can still set anything.

-- +goose Up

-- ------------------------------------------------------------------ nodes
-- session_epoch for the hello bump, last_seen_at for the heartbeat. Row
-- locks (the FOR SHARE in a report) also need UPDATE on some column.
GRANT UPDATE (session_epoch, last_seen_at) ON waystone.nodes TO waystone_app;

-- R4 nodes_guard: only the node itself may touch its row, the epoch only
-- ever goes up by exactly one, and last_seen_at can only be "now".
-- +goose StatementBegin
CREATE FUNCTION waystone.guard_node_update()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF current_user <> 'waystone_app' THEN
        RETURN NEW;
    END IF;

    -- Rule 1: the transaction's actor is this node.
    IF NULLIF(current_setting('app.actor_type', true), '') IS DISTINCT FROM 'node'
       OR NULLIF(current_setting('app.actor_id', true), '') IS DISTINCT FROM OLD.node_id::text THEN
        RAISE EXCEPTION 'nodes: only the node itself may update its row';
    END IF;

    -- The column grant already covers these; checking again costs nothing.
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.node_id IS DISTINCT FROM OLD.node_id
       OR NEW.name IS DISTINCT FROM OLD.name THEN
        RAISE EXCEPTION 'nodes: identity columns cannot change';
    END IF;

    -- Rule 2: unchanged, or exactly one higher.
    IF NEW.session_epoch <> OLD.session_epoch
       AND NEW.session_epoch <> OLD.session_epoch + 1 THEN
        RAISE EXCEPTION 'nodes: session_epoch may only go up by one';
    END IF;

    -- Rule 3: unchanged, or the transaction's own time.
    IF NEW.last_seen_at IS DISTINCT FROM OLD.last_seen_at
       AND NEW.last_seen_at IS DISTINCT FROM now() THEN
        RAISE EXCEPTION 'nodes: last_seen_at must be now()';
    END IF;

    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER nodes_guard
BEFORE UPDATE ON waystone.nodes
FOR EACH ROW
EXECUTE FUNCTION waystone.guard_node_update();

-- ---------------------------------------------------------------- servers
GRANT UPDATE (
    process_state, process_since, process_observed_at,
    report_epoch, report_seq, agent_reported_at
) ON waystone.servers TO waystone_app;

-- R4 servers_report_guard: report columns move together, only for the
-- server's own node, only in that node's current session, and only
-- forward. desired_state is never a node's to change.
-- +goose StatementBegin
CREATE FUNCTION waystone.guard_server_report()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    actor_type text := NULLIF(current_setting('app.actor_type', true), '');
    actor_id   text := NULLIF(current_setting('app.actor_id', true), '');
    epoch      bigint;
BEGIN
    IF current_user <> 'waystone_app' THEN
        RETURN NEW;
    END IF;

    -- Rule 1: a node never changes what someone asked for.
    IF NEW.desired_state IS DISTINCT FROM OLD.desired_state
       AND (actor_type IS NULL OR actor_type = 'node') THEN
        RAISE EXCEPTION 'servers: a node cannot change desired_state';
    END IF;

    IF (NEW.process_state, NEW.process_since, NEW.process_observed_at,
        NEW.report_epoch, NEW.report_seq, NEW.agent_reported_at)
       IS NOT DISTINCT FROM
       (OLD.process_state, OLD.process_since, OLD.process_observed_at,
        OLD.report_epoch, OLD.report_seq, OLD.agent_reported_at) THEN
        RETURN NEW;
    END IF;

    -- Rule 2.1: only the server's own node reports on it.
    IF actor_type IS DISTINCT FROM 'node'
       OR actor_id IS DISTINCT FROM OLD.node_id::text THEN
        RAISE EXCEPTION 'servers: only the server''s node may write report columns';
    END IF;

    -- Rule 2.2: the report belongs to the node's current session. RLS
    -- applies here too, and the node can see its own row.
    SELECT n.session_epoch INTO epoch
    FROM waystone.nodes n
    WHERE n.node_id = OLD.node_id;
    IF epoch IS NULL OR NEW.report_epoch IS DISTINCT FROM epoch THEN
        RAISE EXCEPTION 'servers: report_epoch is not the node''s current epoch';
    END IF;

    -- Rule 2.3: strictly newer than what is stored.
    IF NOT ((NEW.report_epoch, NEW.report_seq) > (OLD.report_epoch, OLD.report_seq)) THEN
        RAISE EXCEPTION 'servers: report (epoch, seq) must move forward';
    END IF;

    -- Rule 2.4: observed at the controller's receipt time.
    IF NEW.process_observed_at IS DISTINCT FROM now() THEN
        RAISE EXCEPTION 'servers: process_observed_at must be now()';
    END IF;

    -- Rule 2.5: since moves only when the state does.
    IF NEW.process_state IS DISTINCT FROM OLD.process_state THEN
        IF NEW.process_since IS DISTINCT FROM now() THEN
            RAISE EXCEPTION 'servers: process_since must be now() when the state changes';
        END IF;
    ELSIF NEW.process_since IS DISTINCT FROM OLD.process_since THEN
        RAISE EXCEPTION 'servers: process_since cannot change without a state change';
    END IF;

    -- Rule 2.6: every report carries the agent's own clock.
    IF NEW.agent_reported_at IS NULL THEN
        RAISE EXCEPTION 'servers: agent_reported_at is required';
    END IF;

    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER servers_report_guard
BEFORE UPDATE ON waystone.servers
FOR EACH ROW
EXECUTE FUNCTION waystone.guard_server_report();
