-- SPDX-License-Identifier: AGPL-3.0-only

-- Stage 4b-2 (notes/stage4b2-scope.md, D1): the audit log learns a second
-- outcome. "flagged" marks something suspicious that wasn't refused, such
-- as a node whose agent keeps taking over its own session (C4).
--
-- Only a node may write a flagged row, and the CHECK says so. Nothing else
-- changes: waystone_app keeps exactly SELECT and INSERT, and the existing
-- tenant_rows policy (tenant AND membership) still decides which rows it
-- may add. A node is a member of its own tenant, so it already passes.

-- +goose Up

ALTER TABLE waystone.audit_events DROP CONSTRAINT audit_events_outcome_check;
ALTER TABLE waystone.audit_events ADD CONSTRAINT audit_events_outcome_check
    CHECK (outcome = 'denied' OR (outcome = 'flagged' AND actor_type = 'node'));
