-- Initial schema. Enum columns hold the full proto enum value name.

CREATE TABLE persons (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    display_name text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_persons_created ON persons (created_at, id);

CREATE TABLE teams (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT ux_teams_name UNIQUE (name)
);

CREATE TABLE team_members (
    team_id   uuid NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    person_id uuid NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
    PRIMARY KEY (team_id, person_id)
);
CREATE INDEX ix_team_members_person ON team_members (person_id);

CREATE TABLE device_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    person_id    uuid NOT NULL REFERENCES persons (id) ON DELETE CASCADE,
    label        text NOT NULL,
    -- sha256 of the bearer secret; the secret itself is never stored.
    token_hash   bytea NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz,
    CONSTRAINT ux_device_tokens_hash UNIQUE (token_hash)
);

CREATE TABLE blobs (
    content_hash text PRIMARY KEY CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    size_bytes   bigint NOT NULL CHECK (size_bytes >= 0),
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE org_artifacts (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ide_type      text NOT NULL,
    type          text NOT NULL,
    slug          text NOT NULL,
    name          text NOT NULL,
    primary_hash  text NOT NULL REFERENCES blobs (content_hash),
    hook_config   jsonb,
    enforce_until timestamptz NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz,
    CONSTRAINT org_artifacts_hook_config_chk CHECK (
        (type = 'ARTIFACT_TYPE_HOOK' AND hook_config IS NOT NULL)
        OR (type <> 'ARTIFACT_TYPE_HOOK' AND hook_config IS NULL))
);
-- One live definition per (tool, type, slug); a retracted one stays as history.
CREATE UNIQUE INDEX ux_org_artifacts_live ON org_artifacts (ide_type, type, slug) WHERE deleted_at IS NULL;
CREATE INDEX ix_org_artifacts_live_created ON org_artifacts (created_at, id) WHERE deleted_at IS NULL;

CREATE TABLE artifact_files (
    artifact_id   uuid NOT NULL REFERENCES org_artifacts (id),
    -- artifact-relative; '' is the primary.
    relative_path text NOT NULL,
    content_hash  text NOT NULL REFERENCES blobs (content_hash),
    is_primary    boolean NOT NULL,
    PRIMARY KEY (artifact_id, relative_path),
    CONSTRAINT artifact_files_primary_chk CHECK (is_primary = (relative_path = ''))
);

CREATE TABLE enforced_artifacts (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    artifact_id uuid NOT NULL REFERENCES org_artifacts (id),
    -- copied from the definition so the root-doc index can see it.
    type        text NOT NULL,
    scope       text NOT NULL,
    repository  text,
    person_id   uuid REFERENCES persons (id) ON DELETE CASCADE,
    team_id     uuid REFERENCES teams (id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz,
    CONSTRAINT enforced_artifacts_target_chk CHECK (NOT (person_id IS NOT NULL AND team_id IS NOT NULL)),
    CONSTRAINT enforced_artifacts_scope_repo_chk CHECK (
        (scope = 'ARTIFACT_SCOPE_USER_GLOBAL' AND repository IS NULL)
        OR (scope = 'ARTIFACT_SCOPE_REPO_LOCAL' AND repository IS NOT NULL))
);
CREATE UNIQUE INDEX ux_enforced_live ON enforced_artifacts (artifact_id, scope, repository, person_id, team_id)
    NULLS NOT DISTINCT WHERE deleted_at IS NULL;
-- Root docs derive one constant file per checkout or home, so two definitions
-- of one type may not share a target.
CREATE UNIQUE INDEX ux_enforced_root_doc_live ON enforced_artifacts (type, scope, repository, person_id, team_id)
    NULLS NOT DISTINCT
    WHERE type IN ('ARTIFACT_TYPE_CLAUDE_MD', 'ARTIFACT_TYPE_AGENTS_MD') AND deleted_at IS NULL;
CREATE INDEX ix_enforced_retracted ON enforced_artifacts (artifact_id, deleted_at) WHERE deleted_at IS NOT NULL;

-- agent_reports holds each person's latest report; a new report replaces it.
CREATE TABLE agent_reports (
    person_id          uuid PRIMARY KEY REFERENCES persons (id) ON DELETE CASCADE,
    device_id          text NOT NULL,
    agent_version      text NOT NULL,
    known_repositories text[] NOT NULL,
    reported_at        timestamptz NOT NULL
);

CREATE TABLE report_installed (
    person_id    uuid NOT NULL REFERENCES agent_reports (person_id) ON DELETE CASCADE,
    ide_type     text NOT NULL,
    -- '' for user-global.
    repository   text NOT NULL,
    source_path  text NOT NULL,
    content_hash text NOT NULL,
    PRIMARY KEY (person_id, ide_type, repository, source_path)
);

CREATE TABLE report_apply_failures (
    person_id    uuid NOT NULL REFERENCES agent_reports (person_id) ON DELETE CASCADE,
    artifact_id  uuid NOT NULL,
    repository   text NOT NULL,
    content_hash text NOT NULL,
    reason       text NOT NULL,
    PRIMARY KEY (person_id, artifact_id, repository)
);

CREATE TABLE settings (
    id         boolean PRIMARY KEY DEFAULT true CHECK (id),
    paused     boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO settings (id) VALUES (true);
