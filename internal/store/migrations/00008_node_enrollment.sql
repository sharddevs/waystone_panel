-- SPDX-License-Identifier: AGPL-3.0-only

-- Stage 6a-1 (notes/stage6-scope.md draft 2, skeletons/stage6/6a):
-- enrolling a node with a one-time token, its pinned key, and revoking it.
--
--   * nodes gain the agent's key pin (the SHA-256 of its certificate's
--     SubjectPublicKeyInfo), when it enrolled and when it was revoked, and
--     the version and capabilities its agent last said hello with. The
--     local node on agent.sock has no pin.
--   * node_enrollments holds one-time tokens: only the SHA-256 of the
--     secret, made by an admin, valid one hour, redeemed once. The app may
--     add one and list them without the hash; it can never redeem one,
--     change one or pick its expiry.
--   * enroll_node redeems a token (6M3): one atomic UPDATE, the
--     database's own clock, the tenant and name from the token's row. The
--     pin comes from the TLS handshake in Go (6M1).
--   * agent_node_by_pin and agent_node_by_id let the agent link find a
--     connection's node before any tenant is known (6M2, 6H6).
--   * revoke_node revokes a node and removes its membership, so every
--     later transaction as that node fails.
--
-- The servers, ports and new operation actions come in 00009 (6c).

-- +goose Up

-- ------------------------------------------------------------------ nodes
ALTER TABLE waystone.nodes
    ADD COLUMN agent_spki_sha256 bytea UNIQUE CHECK (octet_length(agent_spki_sha256) = 32),
    ADD COLUMN enrolled_at timestamptz,
    ADD COLUMN revoked_at timestamptz,
    ADD COLUMN agent_version text CHECK (agent_version ~ '^[\x20-\x7e]{1,64}$'),
    ADD COLUMN capabilities text[] NOT NULL DEFAULT '{}' CHECK (cardinality(capabilities) <= 8);

GRANT UPDATE (agent_version, capabilities) ON waystone.nodes TO waystone_app;

-- The 00002 guard, plus: the new identity columns can't change from the
-- app, and each capability is a short lowercase word.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION waystone.guard_node_update()
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
       OR NEW.name IS DISTINCT FROM OLD.name
       OR NEW.agent_spki_sha256 IS DISTINCT FROM OLD.agent_spki_sha256
       OR NEW.enrolled_at IS DISTINCT FROM OLD.enrolled_at
       OR NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN
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

    -- Rule 4: capabilities are words like "minecraft-java".
    IF EXISTS (SELECT 1 FROM unnest(NEW.capabilities) c WHERE c IS NULL OR c !~ '^[a-z0-9-]{1,32}$') THEN
        RAISE EXCEPTION 'nodes: invalid capability';
    END IF;

    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- ------------------------------------------------- policy helper function

-- Like actor_is_member, with the role too. Caller's rights, so the
-- memberships policy filters it the same way.
CREATE FUNCTION waystone.actor_has_role(t uuid, r text) RETURNS boolean
LANGUAGE sql STABLE
AS $$
    SELECT EXISTS (
        SELECT 1 FROM waystone.memberships m
        WHERE m.tenant_id = t
          AND m.role = r
          AND m.actor_type = current_setting('app.actor_type', true)
          AND m.actor_id = current_setting('app.actor_id', true))
$$;

REVOKE ALL ON FUNCTION waystone.actor_has_role(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION waystone.actor_has_role(uuid, text) TO waystone_app;

-- ------------------------------------------------------- node_enrollments
-- used_at, node_id and denials are set by enroll_node only. A spent token
-- without a node is one whose key was already a node's (pin_taken).
-- denials counts the refusals audited for this token (6a-H1).
CREATE TABLE waystone.node_enrollments (
    tenant_id uuid NOT NULL REFERENCES waystone.tenants (tenant_id),
    enrollment_id uuid PRIMARY KEY,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    secret_sha256 bytea NOT NULL UNIQUE CHECK (octet_length(secret_sha256) = 32),
    created_by_type text NOT NULL,
    created_by_id text NOT NULL,
    created_by_name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL DEFAULT now() + interval '1 hour',
    used_at timestamptz,
    node_id uuid,
    denials integer NOT NULL DEFAULT 0 CHECK (denials BETWEEN 0 AND 5),
    CHECK (expires_at > created_at AND expires_at <= created_at + interval '1 hour'),
    CHECK (node_id IS NULL OR used_at IS NOT NULL)
);

ALTER TABLE waystone.node_enrollments ENABLE ROW LEVEL SECURITY;
ALTER TABLE waystone.node_enrollments FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON waystone.node_enrollments TO waystone_owner
    USING (true) WITH CHECK (true);
-- Admins only: a node has no business with tokens.
CREATE POLICY admin_rows ON waystone.node_enrollments TO waystone_app
    USING (tenant_id = waystone.current_tenant() AND waystone.actor_has_role(tenant_id, 'admin'))
    WITH CHECK (tenant_id = waystone.current_tenant() AND waystone.actor_has_role(tenant_id, 'admin'));
-- The times come from the defaults: the app can't name them, and it never
-- reads a secret's hash back.
GRANT INSERT (tenant_id, enrollment_id, name, secret_sha256, created_by_type, created_by_id, created_by_name)
    ON waystone.node_enrollments TO waystone_app;
GRANT SELECT (tenant_id, enrollment_id, name, created_by_type, created_by_id, created_by_name,
              created_at, expires_at, used_at, node_id)
    ON waystone.node_enrollments TO waystone_app;

-- ---------------------------------------------------------------- audit
-- "recorded" also covers a node's enrollment (filed under the admin who
-- made the token) and its revocation.
ALTER TABLE waystone.audit_events DROP CONSTRAINT audit_events_outcome_check;
ALTER TABLE waystone.audit_events ADD CONSTRAINT audit_events_outcome_check
    CHECK (outcome = 'denied'
        OR (outcome = 'flagged' AND actor_type = 'node')
        OR (outcome = 'recorded' AND actor_type = 'local_user' AND action LIKE 'user.%')
        OR (outcome = 'recorded' AND action IN ('node.enroll', 'node.revoke')));

-- ------------------------------------------------------- definer functions
-- The same rules as the 00006 lookups: owned by waystone_owner, a fixed
-- search_path, every name qualified, EXECUTE for waystone_app alone.

-- enroll_node redeems one token for the key that the TLS handshake proved
-- (6M1, 6M3). p_request_id and p_client_ip only label the audit row. The
-- address is typed inet, so the database checks it too.
--
-- outcome:
--   enrolled   the node, its membership and a recorded audit row exist;
--   used       the token was redeemed before;
--   expired    the token is past its hour;
--   pin_taken  the key is already a node's; the token is spent anyway.
-- An unknown secret returns no row and writes nothing (no tenant to file
-- it under). A used or expired token gets a denied audit row for its
-- first 5 refusals only, so whoever holds an old token can't fill the
-- audit table (6a-H1); the outcome is the same either way.
-- +goose StatementBegin
CREATE FUNCTION waystone.enroll_node(p_secret_sha256 bytea, p_agent_spki_sha256 bytea,
                                     p_request_id text, p_client_ip inet)
RETURNS TABLE (outcome text, node_id uuid, tenant_id uuid, name text)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    e waystone.node_enrollments%ROWTYPE;
    v_node uuid := gen_random_uuid();
    v_outcome text;
BEGIN
    IF octet_length(p_secret_sha256) IS DISTINCT FROM 32
       OR octet_length(p_agent_spki_sha256) IS DISTINCT FROM 32 THEN
        RAISE EXCEPTION 'enroll_node: bad argument';
    END IF;
    -- One address, never a network.
    IF p_client_ip IS NOT NULL
       AND masklen(p_client_ip) <> (CASE family(p_client_ip) WHEN 4 THEN 32 ELSE 128 END) THEN
        RAISE EXCEPTION 'enroll_node: bad argument';
    END IF;

    -- One statement decides who wins: of two concurrent redemptions the
    -- second waits for the first's row lock, then sees used_at set.
    UPDATE waystone.node_enrollments ne
    SET used_at = now(), node_id = v_node
    WHERE ne.secret_sha256 = p_secret_sha256
      AND ne.used_at IS NULL
      AND ne.expires_at > now()
    RETURNING ne.* INTO e;

    IF FOUND THEN
        IF EXISTS (SELECT 1 FROM waystone.nodes n WHERE n.agent_spki_sha256 = p_agent_spki_sha256) THEN
            v_outcome := 'pin_taken';
            -- The token stays spent (one token, one try), with no node.
            UPDATE waystone.node_enrollments ne SET node_id = NULL
            WHERE ne.enrollment_id = e.enrollment_id;
        ELSE
            INSERT INTO waystone.nodes (tenant_id, node_id, name, agent_spki_sha256, enrolled_at)
            VALUES (e.tenant_id, v_node, e.name, p_agent_spki_sha256, now());
            INSERT INTO waystone.memberships (tenant_id, actor_type, actor_id, role)
            VALUES (e.tenant_id, 'node', v_node::text, 'node');
            INSERT INTO waystone.audit_events (tenant_id, event_id, actor_type, actor_id, actor_name,
                action, resource_type, resource_id, reason, request_id, outcome, at, client_ip)
            VALUES (e.tenant_id, gen_random_uuid(), e.created_by_type, e.created_by_id, e.created_by_name,
                'node.enroll', 'node', v_node::text, 'enrolled', p_request_id, 'recorded', now(), host(p_client_ip));
            RETURN QUERY SELECT 'enrolled'::text, v_node, e.tenant_id, e.name;
            RETURN;
        END IF;
    ELSE
        SELECT ne.* INTO e FROM waystone.node_enrollments ne WHERE ne.secret_sha256 = p_secret_sha256;
        IF NOT FOUND THEN
            RETURN;
        END IF;
        v_outcome := CASE WHEN e.used_at IS NOT NULL THEN 'used' ELSE 'expired' END;
        UPDATE waystone.node_enrollments ne SET denials = ne.denials + 1
        WHERE ne.enrollment_id = e.enrollment_id AND ne.denials < 5;
        IF NOT FOUND THEN
            RETURN QUERY SELECT v_outcome, NULL::uuid, e.tenant_id, e.name;
            RETURN;
        END IF;
    END IF;

    INSERT INTO waystone.audit_events (tenant_id, event_id, actor_type, actor_id, actor_name,
        action, resource_type, resource_id, reason, request_id, outcome, at, client_ip)
    VALUES (e.tenant_id, gen_random_uuid(), 'enrollment', e.enrollment_id::text, 'enrollment ' || e.name,
        'node.enroll', 'node_enrollment', e.enrollment_id::text, v_outcome, p_request_id, 'denied', now(), host(p_client_ip));
    RETURN QUERY SELECT v_outcome, NULL::uuid, e.tenant_id, e.name;
END;
$$;
-- +goose StatementEnd

-- The agent link's lookups, before any tenant is known. At most one row.
CREATE FUNCTION waystone.agent_node_by_pin(p_agent_spki_sha256 bytea)
RETURNS TABLE (node_id uuid, tenant_id uuid, name text, revoked boolean)
LANGUAGE sql STABLE
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
    SELECT n.node_id, n.tenant_id, n.name, n.revoked_at IS NOT NULL
    FROM waystone.nodes n
    WHERE n.agent_spki_sha256 = p_agent_spki_sha256
$$;

CREATE FUNCTION waystone.agent_node_by_id(p_node_id uuid)
RETURNS TABLE (node_id uuid, tenant_id uuid, name text, revoked boolean)
LANGUAGE sql STABLE
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
    SELECT n.node_id, n.tenant_id, n.name, n.revoked_at IS NOT NULL
    FROM waystone.nodes n
    WHERE n.node_id = p_node_id
$$;

-- revoke_node revokes a node of the transaction's tenant and removes its
-- membership. Only an admin of that tenant may: a definer sees past RLS,
-- so it checks that itself. The audit row is the caller's, in the same
-- transaction. Returns revoked, already_revoked or not_found.
-- +goose StatementBegin
CREATE FUNCTION waystone.revoke_node(p_node_id uuid)
RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    t uuid := NULLIF(current_setting('app.tenant_id', true), '')::uuid;
BEGIN
    IF t IS NULL OR NOT EXISTS (
        SELECT 1 FROM waystone.memberships m
        WHERE m.tenant_id = t
          AND m.role = 'admin'
          AND m.actor_type = current_setting('app.actor_type', true)
          AND m.actor_id = current_setting('app.actor_id', true)) THEN
        RAISE EXCEPTION 'revoke_node: not an admin of this tenant';
    END IF;

    UPDATE waystone.nodes n SET revoked_at = now()
    WHERE n.node_id = p_node_id AND n.tenant_id = t AND n.revoked_at IS NULL;
    IF NOT FOUND THEN
        IF EXISTS (SELECT 1 FROM waystone.nodes n WHERE n.node_id = p_node_id AND n.tenant_id = t) THEN
            RETURN 'already_revoked';
        END IF;
        RETURN 'not_found';
    END IF;

    DELETE FROM waystone.memberships m
    WHERE m.tenant_id = t AND m.actor_type = 'node' AND m.actor_id = p_node_id::text;
    RETURN 'revoked';
END;
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION waystone.enroll_node(bytea, bytea, text, inet) FROM PUBLIC;
REVOKE ALL ON FUNCTION waystone.agent_node_by_pin(bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION waystone.agent_node_by_id(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION waystone.revoke_node(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION waystone.enroll_node(bytea, bytea, text, inet) TO waystone_app;
GRANT EXECUTE ON FUNCTION waystone.agent_node_by_pin(bytea) TO waystone_app;
GRANT EXECUTE ON FUNCTION waystone.agent_node_by_id(uuid) TO waystone_app;
GRANT EXECUTE ON FUNCTION waystone.revoke_node(uuid) TO waystone_app;

-- +goose Down
-- Migrations only go forward (stage 1).
