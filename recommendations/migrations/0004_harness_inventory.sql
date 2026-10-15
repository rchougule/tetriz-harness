-- Copyright 2026 Workloom Solutions Private Limited
-- SPDX-License-Identifier: MIT

-- Committed-harness inventory the DETECTED detectors read. Populated by the on-disk harness
-- reader (zero external dependency) or a dump import. Enum columns hold the full proto value
-- name as text. No row-level security: single-tenant.

-- harness_artifacts is the observed-artifact inventory, discriminated by artifact_source
-- (VCS = org-canonical/person-less; DESKTOP = per-person). The detectors read the VCS rows.
-- files is the per-artifact file-reference JSONB (is_primary / truncated); current_file_id
-- points at the harness_files row holding the body.
CREATE TABLE IF NOT EXISTS harness_artifacts (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id    uuid NOT NULL,
    artifact_source    text NOT NULL DEFAULT 'HARNESS_ARTIFACT_SOURCE_DESKTOP',
    person_id          uuid,
    ide_type           text NOT NULL,
    scope              text NOT NULL,
    type               text NOT NULL,
    name               text NOT NULL,
    repository_id      uuid,
    source_path        text NOT NULL,
    current_file_id    uuid NOT NULL,
    operational_state  text NOT NULL DEFAULT 'HARNESS_OPERATIONAL_STATE_ENABLED',
    files              jsonb NOT NULL DEFAULT '{}'::jsonb,
    content_generation bigint NOT NULL DEFAULT 0,
    created_at         timestamptz NOT NULL DEFAULT now(),
    deleted_at         timestamptz,
    -- A VCS artifact is org-canonical and person-less; every other source belongs to one engineer.
    CONSTRAINT harness_artifacts_person_by_source_chk CHECK (
        ((artifact_source = 'HARNESS_ARTIFACT_SOURCE_VCS') AND (person_id IS NULL))
        OR ((artifact_source <> 'HARNESS_ARTIFACT_SOURCE_VCS') AND (person_id IS NOT NULL))),
    -- A user-global artifact has no repository; a repo-scoped one must name its repository.
    CONSTRAINT harness_artifacts_scope_repo_chk CHECK (
        ((scope = 'HARNESS_ARTIFACT_SCOPE_USER_GLOBAL') AND (repository_id IS NULL))
        OR ((scope IN ('HARNESS_ARTIFACT_SCOPE_REPO_LOCAL', 'HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED')) AND (repository_id IS NOT NULL)))
);
CREATE INDEX IF NOT EXISTS idx_harness_artifacts_org_source_repo ON harness_artifacts (organization_id, artifact_source, repository_id) WHERE deleted_at IS NULL;

-- harness_files is the content-addressed body store. content holds the artifact body inline.
CREATE TABLE IF NOT EXISTS harness_files (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  uuid NOT NULL,
    content_hash     text NOT NULL,
    byte_size        bigint,
    status           text NOT NULL,
    content          bytea,           -- artifact body, read in place
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_harness_files_org_hash ON harness_files (organization_id, content_hash);

-- harness_usage_records is per-invocation usage feeding the usage-informed detectors
-- (personal-never-used, MCP). One row per invocation event.
CREATE TABLE IF NOT EXISTS harness_usage_records (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  uuid NOT NULL,
    person_id        uuid NOT NULL,
    ide_type         text NOT NULL,
    user_global      boolean NOT NULL,
    repo_full_name   text,
    source_path      text NOT NULL,
    artifact_type    text NOT NULL,
    name             text NOT NULL,
    conversation_id  text NOT NULL,
    usage_date       date NOT NULL,
    invocation_count integer NOT NULL DEFAULT 1,
    repository_id    uuid,
    artifact_id      uuid,
    resolution_state text NOT NULL DEFAULT 'HARNESS_USAGE_RESOLUTION_STATE_PENDING',
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_harness_usage_records_org_person ON harness_usage_records (organization_id, person_id, usage_date);

-- harness_sync_scopes records each engineer's per-repo sync, used to pick a person's primary repo.
CREATE TABLE IF NOT EXISTS harness_sync_scopes (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id       uuid NOT NULL,
    person_id             uuid NOT NULL,
    repository_id         uuid,
    inventory_fingerprint text NOT NULL,
    last_synced_at        timestamptz NOT NULL DEFAULT now(),
    created_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_harness_sync_scopes_org_person ON harness_sync_scopes (organization_id, person_id);
