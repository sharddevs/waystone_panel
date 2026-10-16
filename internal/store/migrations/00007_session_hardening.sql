-- SPDX-License-Identifier: AGPL-3.0-only

-- Stage 5 security audit fixes (audits/stage5-baseline-2b23a52-0c1a092.md):
--
--   * A5: the database caps a session's lifetime at the absolute timeout
--     (7 days, Q7), so a bug in Go can't make a session that lives longer.
--   * A6: audit rows can carry the client's address. A failed sign-in's
--     row is filed under the account that was tried (the schema needs an
--     actor); the address is what points at whoever tried it.
--   * I4: purge_sessions() deletes sessions past their absolute expiry or
--     idle longer than the idle timeout (12 hours), in every tenant. The
--     controller runs it at start and then every hour. It is the third
--     SECURITY DEFINER function next to the two lookups, and as narrow:
--     no argument at all, the database's own clock, delete only.
--
-- No grant to waystone_app widens: it already inserts whole audit rows,
-- and purge_sessions can only remove sessions nobody can use any more.

-- +goose Up

-- In hours, not days: '7 days' follows the session's time zone across a
-- DST change, and Go's AbsoluteTimeout is exactly 168 hours.
ALTER TABLE waystone.sessions ADD CONSTRAINT sessions_lifetime_check
    CHECK (expires_at <= created_at + interval '168 hours');

ALTER TABLE waystone.audit_events ADD COLUMN client_ip text
    CHECK (client_ip IS NULL OR octet_length(client_ip) BETWEEN 1 AND 45);

-- +goose StatementBegin
CREATE FUNCTION waystone.purge_sessions()
RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    n integer;
BEGIN
    DELETE FROM waystone.sessions
    WHERE expires_at <= now() OR last_seen_at <= now() - interval '12 hours';
    GET DIAGNOSTICS n = ROW_COUNT;
    RETURN n;
END;
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION waystone.purge_sessions() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION waystone.purge_sessions() TO waystone_app;
