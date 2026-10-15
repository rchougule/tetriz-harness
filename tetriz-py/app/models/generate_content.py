# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

from pydantic import BaseModel, Field

from app.llm.router import MAX_CONTENT_CHARS
from app.llm.types import TokenUsage


class GenerateContentRequest(BaseModel):
    content: str = Field(..., min_length=1, max_length=MAX_CONTENT_CHARS, description="User content / question.")
    task_type: str = Field(..., min_length=1, description="Task type for prompt routing (a TASK_REGISTRY key).")
    org_id: str = Field(..., min_length=1, description="Organization ID for per-tenant log/trace correlation.")
    source_id: str = Field(
        ...,
        min_length=1,
        description="Identifier of the source entity this LLM call is made for; bound into log context.",
    )
    temperature: float | None = Field(None, ge=0.0, le=2.0, description="Override the task's default temperature.")
    max_tokens: int | None = Field(None, ge=1, le=65_536, description="Override the task's default max_tokens.")


class GenerateContentResponse(BaseModel):
    result: str = Field(..., description="Generated content.")
    task_type: str = Field(..., description="Task type used.")
    model: str = Field(..., description="Model used.")
    provider: str = Field(..., description="Provider used.")
    usage: TokenUsage | None = Field(None, description="Token usage statistics.")
