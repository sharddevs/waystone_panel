-- SPDX-License-Identifier: AGPL-3.0-only

-- Stage 4d-2 (notes/stage5-api-requirements.md D6): the agent reports
-- readiness, the result of the profile's own probe (a Minecraft status
-- ping), with every report. waystone_app may now write the three
-- readiness columns, and servers_report_guard covers them with the same
-- rules as the process columns: only the server's node, only in its
-- current session, observed at now(), and readiness_since = now() exactly
-- when readiness changes. The trigger itself stays; only its function is
-- replaced (rules 2.7 to 2.9 are new, the rest is 00002 unchanged).

-- +goose Up

GRANT UPDATE (readiness, readiness_since, readiness_observed_at)
    ON waystone.servers TO waystone_app;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION waystone.guard_server_report()
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
        NEW.readiness, NEW.readiness_since, NEW.readiness_observed_at,
        NEW.report_epoch, NEW.report_seq, NEW.agent_reported_at)
       IS NOT DISTINCT FROM
       (OLD.process_state, OLD.process_since, OLD.process_observed_at,
        OLD.readiness, OLD.readiness_since, OLD.readiness_observed_at,
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

    -- Rule 2.7: readiness is observed with the same report, at the
    -- controller's receipt time.
    IF NEW.readiness_observed_at IS DISTINCT FROM now() THEN
        RAISE EXCEPTION 'servers: readiness_observed_at must be now()';
    END IF;

    -- Rule 2.8: readiness_since moves only when readiness does.
    IF NEW.readiness IS DISTINCT FROM OLD.readiness THEN
        IF NEW.readiness_since IS DISTINCT FROM now() THEN
            RAISE EXCEPTION 'servers: readiness_since must be now() when readiness changes';
        END IF;
    ELSIF NEW.readiness_since IS DISTINCT FROM OLD.readiness_since THEN
        RAISE EXCEPTION 'servers: readiness_since cannot change without a readiness change';
    END IF;

    -- Rule 2.9: only a running process can be ready.
    IF NEW.readiness = 'ready' AND NEW.process_state IS DISTINCT FROM 'running' THEN
        RAISE EXCEPTION 'servers: readiness can only be ready while the process is running';
    END IF;

    RETURN NEW;
END;
$$;
-- +goose StatementEnd
