-- SPDX-License-Identifier: AGPL-3.0-only
--
-- Development seed for the dev VM (decision J). Run it once, as the owner,
-- after "controller migrate", passing your own uid:
--
--   psql "host=/var/run/postgresql dbname=waystone user=waystone_owner" \
--     -v ON_ERROR_STOP=1 -v admin_uid="$(id -u)" \
--     -f deploy/postgres/seed-dev.sql
--
-- It inserts rows only. Nothing on the host changes, so no operation is
-- recorded. The two servers already exist on the VM from stage 3.
-- Development only: never part of a release.

BEGIN;

INSERT INTO waystone.tenants (tenant_id, name)
VALUES ('01a13180-7c18-750b-ab5b-4d327168cb6f', 'Default organization');

INSERT INTO waystone.nodes (tenant_id, node_id, name)
VALUES ('01a13180-7c18-750b-ab5b-4d327168cb6f', '01a13180-7c18-760c-870d-164e46211f28', 'waystone-dev');

-- You, as the organization's admin. The node's membership comes in 4b,
-- with the agent.
INSERT INTO waystone.memberships (tenant_id, actor_type, actor_id, role)
VALUES ('01a13180-7c18-750b-ab5b-4d327168cb6f', 'local_user', :'admin_uid', 'admin');

INSERT INTO waystone.servers (tenant_id, server_id, node_id, name, profile, runtime)
VALUES
  ('01a13180-7c18-750b-ab5b-4d327168cb6f', '122d4edb-ce38-49d3-a1c0-67efc42de5f5',
   '01a13180-7c18-760c-870d-164e46211f28', 'stage 3b server', 'minecraft-java', 'systemd'),
  ('01a13180-7c18-750b-ab5b-4d327168cb6f', '741dfc48-2be4-4a44-8437-2c7aba75d2d9',
   '01a13180-7c18-760c-870d-164e46211f28', 'built from nothing', 'minecraft-java', 'systemd');

COMMIT;
