-- Copyright 2026 Workloom Solutions Private Limited
-- SPDX-License-Identifier: MIT

-- Project-management signals the DERIVED flow reads for bug-volume / priority grounding.
-- Read-only mirrors of an external PM connector (not part of this engine); populated by
-- fixtures or a dump import. Enum columns hold the full proto value name as text. No
-- row-level security: single-tenant.

-- pm_issues is the per-issue registry; the engine reads bug volume and priority over a window.
CREATE TABLE IF NOT EXISTS pm_issues (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id            uuid NOT NULL,
    -- connector_id is the connector identifier string (e.g. 'github'); the bug-signal's label check
    -- is GitHub-only, so this is text, not a uuid.
    connector_id      text,
    issue_type        text,
    priority          text,
    labels            jsonb,
    issue_created_at   timestamptz,
    deleted_at        timestamptz
);
CREATE INDEX IF NOT EXISTS idx_pm_issues_org_created ON pm_issues (org_id, issue_created_at) WHERE deleted_at IS NULL;

-- pr_issue_mapping_candidates is the normalized child table linking a PR to the issue it
-- closes (the read source, not the parent mapping JSONB), feeding the repo-attributed bug signal.
CREATE TABLE IF NOT EXISTS pr_issue_mapping_candidates (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id           uuid NOT NULL,
    entity_type      text,
    entity_id        uuid,
    other_entity_id  uuid,
    closes_issue     boolean NOT NULL DEFAULT false,
    deleted_at       timestamptz
);
CREATE INDEX IF NOT EXISTS idx_pr_issue_mapping_candidates_org_entity ON pr_issue_mapping_candidates (org_id, entity_id) WHERE deleted_at IS NULL;
