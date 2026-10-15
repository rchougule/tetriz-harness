# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Task routing: a `task_type` string maps to a system prompt, model, and parameters.

The ``/api/v1/generate-content`` endpoint looks the task up in ``TASK_REGISTRY`` — a new
task is a config entry here, not an endpoint change. The review-theme classifier is NOT a
generate-content task; it has its own endpoint and classifier modules.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from app.llm.prompts.classify_harness_intent import CLASSIFY_HARNESS_INTENT_SYSTEM
from app.llm.prompts.derive_org_artifacts import DERIVE_ORG_ARTIFACTS_SYSTEM
from app.llm.prompts.generate_harness_fix import GENERATE_HARNESS_FIX_SYSTEM
from app.llm.schemas.classify_harness_intent import ClassifyHarnessIntentResponse
from app.llm.schemas.derive_org_artifacts import DeriveOrgArtifactsResponse
from app.llm.schemas.generate_harness_fix import GenerateHarnessFixResponse
from app.llm.types import ThinkingLevel

# Hard cap on request content length, measured the same way by every endpoint so two
# doors never disagree on what they accept.
MAX_CONTENT_CHARS = 3_000_000

# Task type constants — use these instead of raw strings to avoid silent typos.
DERIVE_ORG_ARTIFACTS_TASK_TYPE = "derive_org_artifacts"
CLASSIFY_HARNESS_INTENT_TASK_TYPE = "classify_harness_intent"
GENERATE_HARNESS_FIX_TASK_TYPE = "generate_harness_fix"


@dataclass(frozen=True)
class TaskConfig:
    """How one task runs: its system prompt, model, and per-call parameters.

    ``model=None`` defers to the service's configured default model so a single setting
    moves every task at once; a task that must pin a specific model sets it here.
    ``response_json_schema`` is the structured-output contract; when set, the provider
    asks the model to return JSON matching it.
    """

    system_prompt: str
    provider: str = "gemini"
    model: str | None = None
    temperature: float = 0.3
    thinking_level: ThinkingLevel | None = ThinkingLevel.MINIMAL
    thinking_budget: int | None = None
    # A default hard output bound sent on the request; None → clamp-only (the model's own
    # default cap governs a body whose size varies with the artifact).
    max_tokens: int | None = None
    # Upper clamp for a caller-supplied max_tokens.
    max_output_tokens: int | None = None
    # Per-task HTTP timeout override (seconds); None → the provider's configured default.
    request_timeout_seconds: int | None = None
    response_json_schema: dict[str, Any] = field(default_factory=dict)


TASK_REGISTRY: dict[str, TaskConfig] = {
    DERIVE_ORG_ARTIFACTS_TASK_TYPE: TaskConfig(
        system_prompt=DERIVE_ORG_ARTIFACTS_SYSTEM,
        temperature=0.3,
        thinking_level=ThinkingLevel.MINIMAL,
        response_json_schema=DeriveOrgArtifactsResponse.model_json_schema(),
        max_output_tokens=65_536,
    ),
    CLASSIFY_HARNESS_INTENT_TASK_TYPE: TaskConfig(
        system_prompt=CLASSIFY_HARNESS_INTENT_SYSTEM,
        # Classification — deterministic.
        temperature=0.2,
        thinking_level=ThinkingLevel.MINIMAL,
        # On a 2.x model thinking bills against the shared output ceiling, so a bounded
        # thinking_budget is the real reasoning cap; left unset it runs dynamic and can
        # exhaust the ceiling before the answer. 2_048 caps the runaway tail.
        thinking_budget=2_048,
        response_json_schema=ClassifyHarnessIntentResponse.model_json_schema(),
        # Fixed 3-field JSON with a one-sentence rationale — hard-bounded so the Gemini call
        # cannot run away. The ceiling is shared with thinking_budget, so both are kept equal.
        max_tokens=4_096,
        max_output_tokens=4_096,
    ),
    GENERATE_HARNESS_FIX_TASK_TYPE: TaskConfig(
        system_prompt=GENERATE_HARNESS_FIX_SYSTEM,
        # Content rewrite — near-deterministic.
        temperature=0.2,
        thinking_level=ThinkingLevel.MINIMAL,
        response_json_schema=GenerateHarnessFixResponse.model_json_schema(),
        # A full-body rewrite has a long generation tail; give the primary room to finish.
        request_timeout_seconds=285,
        # Clamp-only (no default max_tokens): this task returns a FULL rewritten body whose
        # size varies with the artifact. A body that overruns the model's own default surfaces
        # as a MAX_TOKENS LLMError (a retryable 502), never a silently truncated string.
        max_output_tokens=32_768,
    ),
}


def get_task(task_type: str) -> TaskConfig | None:
    """Return the config for a task type, or None when it is not registered."""
    return TASK_REGISTRY.get(task_type)
