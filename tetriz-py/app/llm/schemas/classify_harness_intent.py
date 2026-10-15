# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Pydantic schema for the classify_harness_intent task.

Structured output for the single LLM call that assigns one intent class to a harness
artifact (skill / subagent / command). All three fields are required on every call
(`ambiguous` is a valid VALUE of intent_class, never an absent field). An off-enum
intent_class or an out-of-range confidence is a ValidationError, which the endpoint
surfaces as a retryable 502.
"""

from __future__ import annotations

from enum import StrEnum

from pydantic import BaseModel, Field


class IntentClass(StrEnum):
    """The fixed intent taxonomy: five behavioral classes plus the `ambiguous` abstain
    value. No other values are legal."""

    MECHANICAL = "mechanical"
    EXPLORATORY = "exploratory"
    REVIEW = "review"
    ORCHESTRATOR = "orchestrator"
    GATE = "gate"
    AMBIGUOUS = "ambiguous"


class ClassifyHarnessIntentResponse(BaseModel):
    """Top-level structured output for the classify_harness_intent task.

    All three fields are required — a low-confidence guess still emits all three.
    """

    intent_class: IntentClass = Field(
        ...,
        description="One of mechanical | exploratory | review | orchestrator | gate | ambiguous.",
    )
    confidence: float = Field(
        ...,
        ge=0.0,
        le=1.0,
        description="The classifier's confidence in the assigned class, 0.0-1.0.",
    )
    rationale: str = Field(
        ...,
        min_length=1,
        description=("One sentence naming the specific signal in stated_purpose/content that drove the class."),
    )
