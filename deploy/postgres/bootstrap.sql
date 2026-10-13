-- SPDX-License-Identifier: AGPL-3.0-only
--
-- One-time PostgreSQL setup for Waystone. Run it as the postgres superuser:
--
--   sudo -u postgres psql -v ON_ERROR_STOP=1 -f deploy/postgres/bootstrap.sql
--
-- It creates two roles and two databases. Nothing in Waystone ever connects
-- as a superuser after this.
--
--   waystone_owner  owns the schema. Migrations and the dev seed run as it.
--                   Only the person administering the host uses it.
--   waystone_app    what the controller runs as. It can read and write rows
--                   it is granted, never change the schema, grants or RLS.
--
-- Neither role is a superuser or has BYPASSRLS: FORCE ROW LEVEL SECURITY
-- does nothing against roles that have either.
--
-- There are no passwords. pg_hba.conf lets local Unix users in through peer
-- authentication, and pg_ident.conf says which Unix user may use which role
-- (see pg_hba-waystone.conf and pg_ident-waystone.conf next to this file).

CREATE ROLE waystone_owner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE waystone_app   LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;

-- waystone is the real database; waystone_test is wiped by the integration
-- tests on every run. Never point the tests at waystone.
CREATE DATABASE waystone      OWNER waystone_owner;
CREATE DATABASE waystone_test OWNER waystone_owner;

REVOKE ALL ON DATABASE waystone      FROM PUBLIC;
REVOKE ALL ON DATABASE waystone_test FROM PUBLIC;
GRANT CONNECT ON DATABASE waystone      TO waystone_app;
GRANT CONNECT ON DATABASE waystone_test TO waystone_app;

-- Two schemas per database: waystone holds the tables, waystone_meta holds
-- goose's version table, which has no tenant and so stays out of the
-- schema that the "every table has tenant_id" test checks.
\connect waystone
REVOKE ALL ON SCHEMA public FROM PUBLIC;
CREATE SCHEMA waystone      AUTHORIZATION waystone_owner;
CREATE SCHEMA waystone_meta AUTHORIZATION waystone_owner;
ALTER ROLE waystone_owner IN DATABASE waystone SET search_path = waystone;
ALTER ROLE waystone_app   IN DATABASE waystone SET search_path = waystone;

\connect waystone_test
REVOKE ALL ON SCHEMA public FROM PUBLIC;
CREATE SCHEMA waystone      AUTHORIZATION waystone_owner;
CREATE SCHEMA waystone_meta AUTHORIZATION waystone_owner;
ALTER ROLE waystone_owner IN DATABASE waystone_test SET search_path = waystone;
ALTER ROLE waystone_app   IN DATABASE waystone_test SET search_path = waystone;
