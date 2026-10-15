-- Copyright 2026 Workloom Solutions Private Limited
-- SPDX-License-Identifier: MIT

-- VCS and raw-connector signals the DERIVED flow reads, plus the committed-repo tree the
-- DETECTED detectors ground paths against. Read-only mirrors of an external VCS/connector
-- producer (not part of this engine); populated by fixtures or a dump import. Enum columns
-- hold the full proto value name as text. No row-level security: single-tenant.

-- pull_requests carries the per-PR outcome the PR-correlation evidence and PR signals read.
CREATE TABLE IF NOT EXISTS pull_requests (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id       uuid NOT NULL,
    repository_id         uuid NOT NULL,
    -- connector_id + platform_repo_id identify the physical repo; the grounding reads filter the
    -- per-repo PR signals on this pair (rename/connector-safe), so both are carried.
    connector_id          text,
    platform_repo_id      text,
    platform_identity_id  uuid,
    state                 text,
    opened_at             timestamptz,
    ready_for_review_at   timestamptz,
    first_review_at       timestamptz,
    merged_at             timestamptz,
    lines_added           integer,
    lines_deleted         integer,
    -- review_cycles is the per-PR review-loop count (loops-per-PR = review_cycles + 1).
    review_cycles         integer,
    reviewers             jsonb,
    files_line_stats      jsonb,
    deleted_at            timestamptz
);
CREATE INDEX IF NOT EXISTS idx_pull_requests_org_repo_merged ON pull_requests (organization_id, repository_id, merged_at) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_pull_requests_org_identity     ON pull_requests (organization_id, platform_identity_id)     WHERE deleted_at IS NULL;

-- raw_records is the landing table for vendor-faithful connector payloads; the engine reads
-- review/PR-comment body text from the payload JSONB (via a per-vendor COALESCE on known keys).
CREATE TABLE IF NOT EXISTS raw_records (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id   uuid NOT NULL,
    connector_id      uuid,
    operation_id      text,
    data_date         date,
    metadata          jsonb,     -- e.g. metadata->>'repo'
    payload           jsonb,     -- vendor-faithful body; read via COALESCE on known keys
    deleted_at        timestamptz
);
CREATE INDEX IF NOT EXISTS idx_raw_records_org_date ON raw_records (organization_id, data_date) WHERE deleted_at IS NULL;

-- vcs_repositories is the per-repo registry. tree_content holds the paths-only repo tree the
-- on-disk reader or a dump fills; tree_key is the legacy tree reference, read by the DERIVED
-- in-scope gate to tell whether a repo has a fetched tree to ground against.
CREATE TABLE IF NOT EXISTS vcs_repositories (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id   uuid NOT NULL,
    connector_id      uuid,
    platform_repo_id  text,
    full_name         text,
    name              text,
    primary_language  text,
    archived          boolean NOT NULL DEFAULT false,
    tree_key          text,      -- legacy tree reference; read by the DERIVED in-scope gate
    tree_content      text,      -- paths-only repo tree body, filled by the reader
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    deleted_at        timestamptz
);
CREATE INDEX IF NOT EXISTS idx_vcs_repositories_org ON vcs_repositories (organization_id) WHERE deleted_at IS NULL;

-- vcs_repo_profile carries per-repo policy context (merge enforcement). A missing row is
-- non-fatal to the engine.
CREATE TABLE IF NOT EXISTS vcs_repo_profile (
    repo_id           uuid NOT NULL,
    organization_id   uuid NOT NULL,
    merge_enforcement text,
    PRIMARY KEY (organization_id, repo_id)
);
