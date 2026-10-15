-- Copyright 2026 Workloom Solutions Private Limited
-- SPDX-License-Identifier: MIT

-- prompt_library holds the engine's output: one row per artifact, either a DERIVED recommendation
-- (recommendation_source NULL or DERIVED) or a DETECTED finding (recommendation_source DETECTED).
-- Enum columns hold the full proto value name as text (e.g. 'ARTIFACT_TYPE_RULE'); the JSONB columns
-- hold proto messages serialized as protojson. No row-level security: single-tenant, the org is bound
-- in every query. The marketplace discriminator (global_artifact_id) is dropped — marketplace is not
-- included — so the is_latest reconcile partition is the five-tuple
-- (organization_id, scope, scope_value, artifact_type, recommendation_source). Artifact bodies live in
-- a side content store keyed by (organization_id, artifact_id), not inline here.
CREATE TABLE IF NOT EXISTS prompt_library (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id       uuid NOT NULL,

    artifact_type         text NOT NULL,
    scope                 text NOT NULL,
    -- scope_value is the repository id for a PROJECT-scoped row, the person id for a PERSON-scoped
    -- row, empty for an ORG-scoped row. Stored as text so empty is valid.
    scope_value           text,
    -- task_type is a free-text template label ("debug", "test", …), not a closed enum.
    task_type             text,

    title                 text NOT NULL,
    slug                  text NOT NULL DEFAULT '',
    artifact_content      jsonb NOT NULL,
    evidence              jsonb NOT NULL DEFAULT '{}'::jsonb,

    priority              integer NOT NULL DEFAULT 0,
    confidence_score      double precision,

    -- is_latest is derived by the write path's reconcile, never caller-set. The column DEFAULTs true
    -- so a row inserted outside the reconcile (there is none today) would read as latest; the writer
    -- forces is_latest=false on every insert so the reconcile is the sole authority.
    is_latest             boolean NOT NULL DEFAULT true,
    client_status         text NOT NULL DEFAULT 'ARTIFACT_CLIENT_STATUS_UNSPECIFIED',
    surface_state         text NOT NULL DEFAULT 'ARTIFACT_SURFACE_STATE_UNSPECIFIED',
    admin_interaction     jsonb NOT NULL DEFAULT '{}'::jsonb,

    -- task_execution_id is the run that produced this row (app-level reference, no FK).
    task_execution_id     uuid,

    -- recommendation_source / dimension are the DETECTED-finding provenance + lens; NULL on a DERIVED
    -- row. harness_artifact_id is the committed artifact a modification finding is about (app-level
    -- ref, no FK). All three are nullable.
    recommendation_source text,
    dimension             text,
    harness_artifact_id   uuid,

    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    deleted_at            timestamptz
);

-- The reconcile and the dashboard list both narrow by (org, scope, scope_value) and filter on
-- is_latest / surface_state; these indexes serve those paths. All are partial on the live set.
CREATE INDEX IF NOT EXISTS idx_pl_org_latest ON prompt_library (organization_id, scope, scope_value, is_latest) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_pl_dedup ON prompt_library (organization_id, scope, scope_value, surface_state, created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_pl_org_client_status ON prompt_library (organization_id, client_status) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_pl_org_type ON prompt_library (organization_id, artifact_type) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_pl_task_execution ON prompt_library (task_execution_id) WHERE deleted_at IS NULL;
