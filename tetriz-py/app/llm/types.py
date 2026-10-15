# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Shared request/response types for the LLM module.

These stay SDK-free: a provider translates them to and from its own vendor types, so
the wire contract never leaks a vendor enum.
"""

from __future__ import annotations

from enum import StrEnum
from typing import Any

from pydantic import BaseModel


class ThinkingLevel(StrEnum):
    """Gemini thinking effort — the control for Gemini 3.x models. Values mirror the
    google-genai ThinkingLevel enum so they pass straight to the SDK."""

    MINIMAL = "MINIMAL"
    LOW = "LOW"
    MEDIUM = "MEDIUM"
    HIGH = "HIGH"


class TokenUsage(BaseModel):
    prompt_tokens: int = 0
    completion_tokens: int = 0
    total_tokens: int = 0
    # Gemini thinking-model reasoning tokens. Billed at the OUTPUT rate but NOT part of
    # completion_tokens (candidates only), so without this field thinking spend is invisible.
    # 0 for providers that do not report it.
    thoughts_tokens: int = 0


class CompletionRequest(BaseModel):
    """One single-shot prompt-to-text (usually prompt-to-JSON) call."""

    model: str
    system_prompt: str = ""
    user_content: str = ""
    # None → the provider omits temperature so the model uses its tuned default.
    temperature: float | None = 0.7
    max_tokens: int | None = None
    # JSON Schema for Gemini structured output. When set, the model must return JSON
    # matching this shape.
    response_json_schema: dict[str, Any] | None = None
    # Gemini 3.x thinking control; None → model default. Ignored on a 2.x model.
    thinking_level: ThinkingLevel | None = None
    # Gemini 2.x thinking budget (token count); None → the 2.x model's dynamic default.
    # Ignored on a 3.x model (which uses thinking_level).
    thinking_budget: int | None = None
    # Per-request HTTP timeout (seconds); None → the provider's configured default.
    timeout_seconds: int | None = None
    # Forwarded for provider-side log correlation; not sent to the LLM API.
    task_type: str | None = None


class CompletionResponse(BaseModel):
    content: str
    model: str
    provider: str
    usage: TokenUsage | None = None
