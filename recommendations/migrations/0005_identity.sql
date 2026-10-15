-- Copyright 2026 Workloom Solutions Private Limited
-- SPDX-License-Identifier: MIT

-- The platform identity map that replaces a cross-service identity lookup. Populated by fixtures or
-- a dump import. No row-level security: single-tenant.

-- platform_identity_map joins a person to their per-platform identity (e.g. a GitHub account), which
-- the PR-correlation evidence uses to match session rows to pull-request rows. It stands in for a
-- cross-service identity lookup a host application would otherwise make over its own RPC.
CREATE TABLE IF NOT EXISTS platform_identity_map (
    organization_id       uuid NOT NULL,
    person_id             uuid NOT NULL,
    platform              text NOT NULL,   -- e.g. 'github'
    platform_identity_id  uuid NOT NULL,
    PRIMARY KEY (organization_id, person_id, platform)
);
CREATE INDEX IF NOT EXISTS idx_platform_identity_map_identity ON platform_identity_map (organization_id, platform_identity_id);
