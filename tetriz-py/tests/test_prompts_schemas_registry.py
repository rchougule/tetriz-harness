# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Loads every prompt, schema, and the task registry, and validates the task configs.

This is the structural guard: the four carried LLM tasks (three generate-content tasks plus
the review-theme classifier's schema types) load, the registry is exactly the three
generate-content tasks (marketplace dropped), each config is well-formed, and the derive
prompt mentions every artifact field.
"""

from __future__ import annotations

import pytest
from pydantic import ValidationError

from app.llm.prompts.classify_harness_intent import CLASSIFY_HARNESS_INTENT_SYSTEM
from app.llm.prompts.derive_org_artifacts import DERIVE_ORG_ARTIFACTS_SYSTEM
from app.llm.prompts.generate_harness_fix import GENERATE_HARNESS_FIX_SYSTEM
from app.llm.router import (
    CLASSIFY_HARNESS_INTENT_TASK_TYPE,
    DERIVE_ORG_ARTIFACTS_TASK_TYPE,
    GENERATE_HARNESS_FIX_TASK_TYPE,
    TASK_REGISTRY,
)
from app.llm.schemas.classify_harness_intent import ClassifyHarnessIntentResponse, IntentClass
from app.llm.schemas.derive_org_artifacts import DerivedArtifact, DeriveOrgArtifactsResponse
from app.llm.schemas.generate_harness_fix import GenerateHarnessFixResponse


def test_registry_holds_exactly_the_three_generate_content_tasks() -> None:
    assert set(TASK_REGISTRY) == {
        DERIVE_ORG_ARTIFACTS_TASK_TYPE,
        CLASSIFY_HARNESS_INTENT_TASK_TYPE,
        GENERATE_HARNESS_FIX_TASK_TYPE,
    }


def test_marketplace_task_is_dropped() -> None:
    assert "recommend_marketplace_artifacts" not in TASK_REGISTRY


def test_every_task_config_is_well_formed() -> None:
    for task_type, config in TASK_REGISTRY.items():
        assert config.system_prompt.strip(), f"{task_type} has an empty system prompt"
        assert config.provider == "gemini"
        assert isinstance(config.response_json_schema, dict) and config.response_json_schema, (
            f"{task_type} has no structured-output schema"
        )
        assert 0.0 <= config.temperature <= 2.0


def test_prompts_are_wired_to_their_task_configs() -> None:
    assert TASK_REGISTRY[DERIVE_ORG_ARTIFACTS_TASK_TYPE].system_prompt == DERIVE_ORG_ARTIFACTS_SYSTEM
    assert TASK_REGISTRY[CLASSIFY_HARNESS_INTENT_TASK_TYPE].system_prompt == CLASSIFY_HARNESS_INTENT_SYSTEM
    assert TASK_REGISTRY[GENERATE_HARNESS_FIX_TASK_TYPE].system_prompt == GENERATE_HARNESS_FIX_SYSTEM


def test_derive_prompt_mentions_every_artifact_field() -> None:
    # The structured-output contract and the prompt must not drift: every DerivedArtifact field
    # the model must emit has to be named in the prompt text.
    prompt = DERIVE_ORG_ARTIFACTS_SYSTEM
    for field_name in DerivedArtifact.model_fields:
        assert field_name in prompt, f"derive prompt never mentions the field {field_name!r}"


def test_classify_harness_intent_schema_round_trips() -> None:
    parsed = ClassifyHarnessIntentResponse.model_validate_json(
        '{"intent_class": "orchestrator", "confidence": 0.8, "rationale": "drains a queue"}'
    )
    assert parsed.intent_class is IntentClass.ORCHESTRATOR
    # Off-enum class and out-of-range confidence are rejected.
    with pytest.raises(ValidationError):
        ClassifyHarnessIntentResponse.model_validate_json(
            '{"intent_class": "nonsense", "confidence": 0.5, "rationale": "x"}'
        )
    with pytest.raises(ValidationError):
        ClassifyHarnessIntentResponse.model_validate_json(
            '{"intent_class": "gate", "confidence": 1.5, "rationale": "x"}'
        )


def test_generate_harness_fix_schema_rejects_blank_body() -> None:
    ok = GenerateHarnessFixResponse.model_validate_json(
        '{"suggested_content": "new body", "change_summary": "fixed the path", "cited_paths": []}'
    )
    assert ok.cited_paths == []
    with pytest.raises(ValidationError):
        GenerateHarnessFixResponse.model_validate_json(
            '{"suggested_content": "   ", "change_summary": "x", "cited_paths": []}'
        )
    # cited_paths is required (an omission is not allowed), even though an empty list is valid.
    with pytest.raises(ValidationError):
        GenerateHarnessFixResponse.model_validate_json('{"suggested_content": "body", "change_summary": "x"}')


def test_derive_response_normalizes_misplaced_fields_and_drops_task_typeless_templates() -> None:
    # A rule carrying a stray hook field, and a template missing task_type, both survive without
    # failing the whole batch: the hook field is cleared, the bad template is dropped.
    response = DeriveOrgArtifactsResponse.model_validate(
        {
            "rules": [
                {
                    "title": "Wrap errors",
                    "slug": "go-error-wrapping",
                    "body": "rule body",
                    "priority": "high",
                    "confidence": 0.9,
                    "evidence_summary": "flagged in 12 reviews",
                    "cited_paths": ["pkg/x.go"],
                    "event": "PostToolUse",  # stray hook field on a rule
                }
            ],
            "templates": [
                {
                    "title": "No task type",
                    "slug": "debug-template",
                    "body": "t",
                    "priority": "low",
                    "confidence": 0.3,
                    "evidence_summary": "e",
                    "cited_paths": [],
                }
            ],
        }
    )
    assert response.rules[0].event is None  # cleared — rules do not own event
    assert response.templates == []  # the task_type-less template was dropped, not raised
