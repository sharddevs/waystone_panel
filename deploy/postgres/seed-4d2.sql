-- SPDX-License-Identifier: AGPL-3.0-only
--
-- Stage 4d-2 development seed: the game port of the dev VM's two servers,
-- so the agent can probe their readiness. Run it once, as the owner, after
-- "controller migrate":
--
--   psql "host=/var/run/postgresql dbname=waystone user=waystone_owner" \
--     -v ON_ERROR_STOP=1 -f deploy/postgres/seed-4d2.sql
--
-- The ports are the ones the servers already listen on (each game logs
-- "Starting Minecraft server on *:<port>" at start). The
-- agent gets them at its next hello, so restart it afterwards. Nothing on
-- the host changes. Development only: never part of a release (the stage
-- 6 install writes the port for real servers).

BEGIN;

UPDATE waystone.servers SET config = config || '{"port": 25566}'
WHERE server_id = '122d4edb-ce38-49d3-a1c0-67efc42de5f5';

UPDATE waystone.servers SET config = config || '{"port": 25567}'
WHERE server_id = '741dfc48-2be4-4a44-8437-2c7aba75d2d9';

COMMIT;
