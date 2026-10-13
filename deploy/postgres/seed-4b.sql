-- SPDX-License-Identifier: AGPL-3.0-only
--
-- Stage 4b development seed: gives the dev VM's node its membership, so
-- the controller can act as that node when its agent says hello. Run it
-- once, as the owner, after seed-dev.sql:
--
--   psql "host=/var/run/postgresql dbname=waystone user=waystone_owner" \
--     -v ON_ERROR_STOP=1 -f deploy/postgres/seed-4b.sql
--
-- One row, nothing on the host changes. Development only: never part of
-- a release (stage 6 enrollment creates this row for real nodes).

BEGIN;

INSERT INTO waystone.memberships (tenant_id, actor_type, actor_id, role)
VALUES ('01a13180-7c18-750b-ab5b-4d327168cb6f', 'node', '01a13180-7c18-760c-870d-164e46211f28', 'node');

COMMIT;
