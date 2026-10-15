-- Copyright 2026 Workloom Solutions Private Limited
-- SPDX-License-Identifier: MIT

-- Session-quality signals the recommendations engine reads. These are a read-only
-- mirror of a session-analysis producer's output; this engine never writes them (they
-- are populated by fixtures, a dump import, or an upstream producer). Enum columns hold
-- the full proto enum value name as text. No row-level security: single-tenant.

-- session_analyses is the per-session judged-quality row. Columns are the flat read-subset
-- the 14 reader methods SELECT; the two generated columns reproduce the producer's wrapped
-- JSONB shape so the aggregates read them unchanged.
CREATE TABLE IF NOT EXISTS session_analyses (
    id                               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id                  uuid NOT NULL,
    person_id                        uuid NOT NULL,
    raw_record_id                    uuid NOT NULL,
    conversation_started_at          timestamptz NOT NULL,
    ide_type                         text,
    -- scores (flat typed columns)
    score_overall                    smallint,
    score_clarity                    smallint,
    score_specificity                smallint,
    score_context_provision          smallint,
    score_actionability              smallint,
    score_completeness               smallint,
    score_efficiency                 smallint,
    weighted_score                   numeric,
    -- volume / cost / efficiency
    total_prompts                    integer,
    total_tokens                     bigint,
    wasted_tokens                    bigint,
    estimated_cost_usd               numeric,
    iteration_count                  integer,
    effective_context_usage_percent  numeric,
    effective_ai_code_percent        numeric,
    effective_active_duration_s      integer,
    cache_hit_rate_percent           numeric,
    compaction_count                 integer,
    files_edited_count               integer,
    is_session_effective             boolean,
    repo_name                        text,
    project_name                     text,
    task_type                        text,
    -- typed-proto JSONB (wrapped {"values":[...]} / object) and plain JSONB
    subagent_usage                   jsonb,
    languages                        jsonb,   -- {"values":[...]}
    friction_counts                  jsonb,   -- object: {type: count}
    models_used                      jsonb,   -- model-bucket computed from here at query time
    invoked_skills                   jsonb,   -- [{name, ...}]
    taxonomy_issues                  jsonb,   -- [{categorySlug, ...}]
    session_meta_raw                 jsonb,   -- backs the two generated columns below
    mcp_server_names                 jsonb GENERATED ALWAYS AS (
        CASE WHEN jsonb_typeof(session_meta_raw -> 'mcpServerNames') = 'array'
             THEN jsonb_set('{"values": []}'::jsonb, '{values}', session_meta_raw -> 'mcpServerNames')
             ELSE NULL END) STORED,
    tool_errors                      text GENERATED ALWAYS AS (session_meta_raw ->> 'toolErrors') STORED,
    deleted_at                       timestamptz
);
-- The readers' keyset and group-by paths.
CREATE INDEX IF NOT EXISTS session_analyses_org_started ON session_analyses (organization_id, conversation_started_at) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS session_analyses_org_person  ON session_analyses (organization_id, person_id)               WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS session_analyses_org_project ON session_analyses (organization_id, project_name)            WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS session_analyses_org_ide     ON session_analyses (organization_id, ide_type)                WHERE deleted_at IS NULL;

-- security_flags is one open/resolved security event per detection. CountByFlagType reads
-- the open flags in a date window grouped by flag_type (a kebab-case category slug).
CREATE TABLE IF NOT EXISTS security_flags (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  uuid NOT NULL,
    person_id        uuid NOT NULL,
    raw_record_id    uuid NOT NULL,
    flag_type        text NOT NULL,                       -- category slug, e.g. 'api-key-leak'
    status           text NOT NULL DEFAULT 'open',
    flag_date        date NOT NULL,
    deleted_at       timestamptz
);
CREATE INDEX IF NOT EXISTS idx_security_flags_org_flag_type ON security_flags (organization_id, flag_type, id) WHERE deleted_at IS NULL;
