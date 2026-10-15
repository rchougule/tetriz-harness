# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Pydantic schemas for the derive_org_artifacts task.

This is the structured-output contract enforced by Gemini's `response_schema`. The shape
is the unified `DerivedArtifact`: rules, templates, skills, hooks, and agents share one
artifact shape, so the Go quality gate and engine consume the same per-artifact fields —
notably `cited_paths`, `confidence`, `scope`/`scope_value`, and `priority`.

This engine emits project-scoped artifacts only; the LLM is instructed to set
`scope="project"`. The system prompt in app/llm/prompts/derive_org_artifacts.py must
emit exactly these fields — the registry test guards that they stay in sync.
"""

from __future__ import annotations

from typing import Literal

import structlog
from pydantic import BaseModel, Field, model_validator

logger = structlog.get_logger(__name__)

# Bounds for a template's trigger_keywords. Enforced by TRIMMING in _normalize_and_validate,
# never a raising Field constraint: an over-long LLM list must trim, not fail the whole derive
# batch. The Go install path re-normalizes and falls back to a static per-task-type map when
# the list is empty, so a modest cap here is safe.
MAX_TRIGGER_KEYWORDS = 20
MAX_TRIGGER_KEYWORD_LEN = 50


class DerivedArtifact(BaseModel):
    """A single derived artifact — rule, template, skill, hook, or agent.

    One shape for all types; the bucket it lands in tells the consumer its kind.
    """

    # Short, unique headline for the artifact (the card pairs it with `slug`).
    title: str = Field(..., min_length=1, description="Short, unique headline; unique within a response.")
    # Stable one-word identifier — the artifact's first-class identity and the dedup key the
    # Go gate matches on. Kebab-case for rules/templates (e.g. go-error-wrapping), '/'-prefixed
    # for skills (e.g. /spec-driven-dev). Deliberately NOT min_length- or required-constrained:
    # an omitted or empty slug must pass validation (defaulting to "") so the Go quality gate
    # hides only that one artifact (slug_missing). A Pydantic constraint here would raise on
    # model_validate_json and fail the whole derive response on a single bad slug.
    slug: str = Field(
        default="",
        description="Stable one-word id: kebab for rules/templates, '/'-prefixed for skills; unique per response.",
    )
    # The artifact body: rule text / template / skill markdown, or an AGENT's instruction prose.
    # A HOOK needs no body — Go synthesizes the hook config from the structured event/matcher/
    # command — so an empty body must NOT reject the batch. The Go gate enforces the per-type
    # body floor instead (rules/templates/skills/agents via body-min-length; hooks exempt).
    body: str = Field("", description="The artifact body in markdown (empty for a hook).")
    # PROJECT only; constrained to the single value so Gemini's structured output cannot emit
    # an org-scope artifact.
    scope: Literal["project"] = Field(default="project", description='Artifact scope; always "project".')
    # Set downstream by Go to the project's immutable repo id; the LLM leaves it blank.
    scope_value: str | None = Field(default=None, description="Set by Go to the repo id; LLM leaves blank.")
    # Templates only — the task type the template targets (debug, test, ...).
    task_type: str | None = Field(default=None, description="Task type (templates only).")
    # Templates only — the LLM's suggested prompt signals that mark a submitted prompt as this
    # template's task type. A list with an empty default (never a required/min_length constraint)
    # so an omitted value defaults to [] and never fails the batch.
    trigger_keywords: list[str] = Field(
        default_factory=list,
        description="Prompt signals that route to this template (templates only); bounded, template-owned.",
    )
    # Semantic level the LLM emits; the Go side maps it onto the INT priority column. Required
    # (not defaulted): a silent default could mis-rank a security/critical artifact whose
    # priority the model omitted. "low" is the weakest-evidence tier.
    priority: Literal["critical", "high", "medium", "low"] = Field(
        ..., description="Semantic priority: critical | high | medium | low."
    )
    confidence: float = Field(..., ge=0.0, le=1.0, description="LLM confidence 0.0-1.0 → confidence_score column.")
    evidence_summary: str = Field(
        ...,
        min_length=1,
        description="Org-specific evidence (session counts, gap %, severity, PR-outcome correlation).",
    )
    addresses_issues: list[str] = Field(
        default_factory=list,
        description="Taxonomy issue slugs this artifact mitigates.",
    )
    # Repo paths the body references (a file, or a directory for a location reference), returned
    # as STRUCTURED data (not parsed out of prose) so the Go gate can verify each against the
    # repo tree. Required (empty list allowed) so the model always emits the structured list.
    cited_paths: list[str] = Field(
        ...,
        description=(
            "Real repo paths the body cites — a file, or a directory for a location "
            "reference; checked against the project tree."
        ),
    )
    # Where a rule/skill lands (multi-IDE). target_file is the destination path the body is
    # written for — an existing rules file the repo tree shows, or a new file to create. Empty
    # is allowed: Go then resolves it to the nearest existing rule file. Empty on templates.
    target_file: str = Field(
        default="",
        description="Destination path for a rule/skill (e.g. CLAUDE.md, .cursor/rules/); empty on templates.",
    )
    # How target_file is applied: append the body inline to an existing rules file, or create a
    # new standalone file that reference_in_file imports.
    target_placement: Literal["append_to_file", "new_referenced_file"] = Field(
        default="append_to_file",
        description="append_to_file (inline) or new_referenced_file (separate file + import line).",
    )
    # The existing rules file that should import/link a new target_file; set ONLY when
    # target_placement is new_referenced_file. Empty otherwise.
    reference_in_file: str = Field(
        default="",
        description="Existing rules file that imports a new_referenced_file target; empty otherwise.",
    )
    # Hook-only extras. Nullable, and required-ness is enforced in the Go quality gate, NEVER
    # here: a Pydantic-required field would raise on model_validate_json and fail the whole
    # derive batch on a single malformed hook. _normalize_and_validate clears these on every
    # non-hook bucket so a stray value can't flow downstream.
    event: str | None = Field(default=None, description="Hook lifecycle event, e.g. PostToolUse (hooks only).")
    matcher: str | None = Field(default=None, description="Hook tool/command matcher (hooks only).")
    command: str | None = Field(default=None, description="Hook best-effort command (hooks only).")
    # Agent-only extras. Nullable / enforce-in-Go like the hook fields; cleared on every
    # non-agent bucket by _normalize_and_validate.
    agent_name: str | None = Field(default=None, description="Agent name, e.g. code-reviewer (agents only).")
    agent_description: str | None = Field(default=None, description="Agent when-to-delegate description (agents only).")
    # agent_tools, like the string extras above, is sanitized by _sanitize_new_type_extras (a
    # mode="before" validator) so a malformed value never raises and fails the batch.
    agent_tools: list[str] = Field(default_factory=list, description="Agent tool scope (agents only).")
    agent_model: str | None = Field(default=None, description="Agent model tier (agents only).")

    @model_validator(mode="before")
    @classmethod
    def _sanitize_new_type_extras(cls, data: object) -> object:
        # A malformed new-type extra must NEVER raise. model_validate_json parses every
        # artifact at once, so a single ValidationError fails the whole derive response and
        # drops all the good rules/templates/skills. Coerce each extra to a valid-but-empty
        # shape BEFORE per-field type validation (which would otherwise reject, e.g., a list
        # where a str is expected); required-ness / correctness is the Go quality gate's to
        # enforce, per artifact. Non-str string-extras degrade to None; a null / scalar /
        # odd-shaped agent_tools degrades to a list.
        if not isinstance(data, dict):
            return data
        # `body` is a non-nullable str (empty for a hook). A model that represents a bodyless
        # hook as `"body": null` would raise on the non-optional str and fail the whole derive
        # batch. Degrade a null or non-str body to "" and let the Go per-type gate enforce the
        # body floor.
        if "body" in data and not isinstance(data["body"], str):
            data["body"] = ""
        for key in ("event", "matcher", "command", "agent_name", "agent_description", "agent_model"):
            value = data.get(key)
            if value is not None and not isinstance(value, str):
                data[key] = None
        # Only touch agent_tools when the key is PRESENT — an absent key must fall through to the
        # default_factory=list. An explicit null (or scalar/odd shape) coerces to a list.
        if "agent_tools" in data:
            tools = data["agent_tools"]
            if isinstance(tools, str):
                data["agent_tools"] = [tools]
            elif isinstance(tools, list):
                data["agent_tools"] = [str(tool) for tool in tools]
            else:
                data["agent_tools"] = []
        # trigger_keywords is a list[str] like agent_tools: a null / scalar / odd shape must
        # coerce to a list, never raise. Only touch it when PRESENT.
        if "trigger_keywords" in data:
            keywords = data["trigger_keywords"]
            if isinstance(keywords, str):
                data["trigger_keywords"] = [keywords]
            elif isinstance(keywords, list):
                data["trigger_keywords"] = [str(keyword) for keyword in keywords]
            else:
                data["trigger_keywords"] = []
        return data


class DeriveOrgArtifactsResponse(BaseModel):
    """Structured LLM output for derive_org_artifacts. Buckets by artifact type."""

    rules: list[DerivedArtifact] = Field(default_factory=list)
    templates: list[DerivedArtifact] = Field(default_factory=list)
    skills: list[DerivedArtifact] = Field(default_factory=list)
    hooks: list[DerivedArtifact] = Field(default_factory=list)
    agents: list[DerivedArtifact] = Field(default_factory=list)

    @model_validator(mode="after")
    def _normalize_and_validate(self) -> DeriveOrgArtifactsResponse:
        # The shared DerivedArtifact shape carries type-specific fields the model can misplace
        # onto the wrong bucket; the structured output can't constrain that, so clear every
        # field from the buckets that do not own it before the contract reaches Go. Clearing
        # (not rejecting) keeps an LLM quirk from failing an otherwise-good batch.
        #
        # Ownership is an ALLOWLIST keyed by object identity: the owner bucket keeps the field,
        # every other bucket has it cleared. A new artifact bucket added to `all_artifacts` is
        # therefore scrubbed of every field it does not own BY DEFAULT — the safe direction.
        # Identity, not equality: two artifacts can compare equal, so `in`/`not in` would
        # mis-scrub — key on id().
        all_artifacts = (*self.rules, *self.templates, *self.skills, *self.hooks, *self.agents)
        template_ids = {id(a) for a in self.templates}
        hook_ids = {id(a) for a in self.hooks}
        agent_ids = {id(a) for a in self.agents}
        for artifact in all_artifacts:
            # scope_value is Go-owned (the immutable repo id) — no bucket authors it.
            artifact.scope_value = None
            # task_type + trigger_keywords describe which task a template targets and how a
            # prompt routes to it — templates own both.
            if id(artifact) not in template_ids:
                artifact.task_type = None
                artifact.trigger_keywords = []
            # event / matcher / command are the hook's lifecycle trigger — hooks own them.
            if id(artifact) not in hook_ids:
                artifact.event = artifact.matcher = artifact.command = None
            # agent_name / description / model / tools describe a subagent — agents own them.
            if id(artifact) not in agent_ids:
                artifact.agent_name = artifact.agent_description = artifact.agent_model = None
                artifact.agent_tools = []
        # Target fields are the one group owned by everyone EXCEPT templates (tool-agnostic
        # prompt scaffolds with no rules-file destination), so they stay a direct per-template
        # clear. rules/skills/hooks/agents keep target_file/target_placement.
        for artifact in self.templates:
            artifact.target_file = ""
            artifact.target_placement = "append_to_file"
            artifact.reference_in_file = ""
            # Bound the keyword list: cap the count and each keyword's length by TRIMMING.
            artifact.trigger_keywords = [
                keyword[:MAX_TRIGGER_KEYWORD_LEN] for keyword in artifact.trigger_keywords[:MAX_TRIGGER_KEYWORDS]
            ]
        # A template WITHOUT task_type is unusable downstream (the install path routes on it),
        # so DROP it — never raise. Raising here fails the whole derive and discards every VALID
        # artifact alongside the one bad template. Log the dropped titles so the LLM quirk stays
        # visible even though the batch survives.
        dropped = [t.title for t in self.templates if not t.task_type]
        if dropped:
            logger.warning("derive: dropping templates missing task_type", dropped=dropped)
            self.templates = [t for t in self.templates if t.task_type]
        return self
