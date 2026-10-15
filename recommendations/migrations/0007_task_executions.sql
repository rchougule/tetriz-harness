-- Copyright 2026 Workloom Solutions Private Limited
-- SPDX-License-Identifier: MIT

-- task_executions is the run-lifecycle audit row: one row per scheduled / manually-triggered run,
-- discriminated by task_type, carrying the single-in-flight guarantee and the terminal compare-and-set.
-- Enum columns hold the full proto value name as text; input_payload / output_payload hold the ContentRef
-- proto as protojson. No row-level security: single-tenant, the org is bound in every query.
CREATE TABLE IF NOT EXISTS task_executions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    task_type       text NOT NULL,
    status          text NOT NULL,
    trigger_type    text NOT NULL,
    triggered_by    uuid,
    -- dedup_key is the single-in-flight identity (the repository id for a per-repo run, the org id for
    -- an org-relational run); nullable for runs that do not serialize on one.
    dedup_key       text,
    input_payload   jsonb NOT NULL DEFAULT '{}'::jsonb,
    output_payload  jsonb,
    error_message   text,
    started_at      timestamptz NOT NULL,
    completed_at    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz,
    substatus       text NOT NULL DEFAULT 'TASK_EXECUTION_SUB_STATUS_UNSPECIFIED'
);

-- The single-in-flight guarantee: at most one RUNNING run per (org, task_type, dedup_key). The partial
-- predicate's status literal must match CreateIfNoneRunning's ON CONFLICT TargetWhere exactly so Postgres
-- uses this index as the conflict arbiter.
CREATE UNIQUE INDEX IF NOT EXISTS idx_te_single_in_flight ON task_executions (organization_id, task_type, dedup_key)
    WHERE status = 'TASK_EXECUTION_STATUS_RUNNING' AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_te_org_task_type_status ON task_executions (organization_id, task_type, status) WHERE deleted_at IS NULL;
