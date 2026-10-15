-- Copyright 2026 Workloom Solutions Private Limited
-- SPDX-License-Identifier: MIT

-- prompt_library_content is the storage-agnostic side content store for artifact bodies/evidence, keyed by
-- (organization_id, artifact_id). The artifactStore seam writes it BEFORE the prompt_library row is
-- inserted (mirroring a content-addressed write-before-row ordering — no FK, no write-ordering problem);
-- prompt_library references it by its own id, and the serve path reads it back by (org, artifact_id).
-- Run input/output blobs are NOT stored (audit-only, dropped). No row-level security: single-tenant.
CREATE TABLE IF NOT EXISTS prompt_library_content (
    organization_id uuid NOT NULL,
    artifact_id     uuid NOT NULL,
    content         bytea,
    evidence        bytea,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, artifact_id)
);
