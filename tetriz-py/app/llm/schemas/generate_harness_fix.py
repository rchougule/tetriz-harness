# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Pydantic schema for the generate_harness_fix task.

Structured output for the single LLM call that drafts a corrected body for a
harness-artifact content-rewrite finding. All three fields are required: a
whitespace-only suggested_content/change_summary or an omitted cited_paths is a
ValidationError, which the endpoint surfaces as a retryable 502 rather than a
successful-but-destructive fix. cited_paths may be an explicit empty list; omitting the
field entirely is not allowed, so the grounding list is never silently defaulted away.
"""

from __future__ import annotations

from pydantic import BaseModel, Field, field_validator


class GenerateHarnessFixResponse(BaseModel):
    """Top-level structured output for the generate_harness_fix task.

    suggested_content is the FULL modified body (not a diff); it must be non-blank, so a
    whitespace-only body can never reach the caller and blank the artifact. The caller
    grounds cited_paths as the real quality gate, so that field is required (an explicit
    empty list is valid; an omission is not) rather than silently defaulted.
    """

    suggested_content: str = Field(
        ...,
        min_length=1,
        description="The full modified artifact body, ready to replace the committed file.",
    )
    change_summary: str = Field(
        ...,
        min_length=1,
        description="One sentence describing what was changed and why.",
    )
    cited_paths: list[str] = Field(
        ...,
        description=(
            "Repo paths the new body references; each must exist in the provided repo path "
            "set. May be an empty list; must not be omitted."
        ),
    )

    @field_validator("suggested_content", "change_summary")
    @classmethod
    def _reject_blank(cls, value: str) -> str:
        # min_length=1 admits whitespace-only strings; a blank suggested_content would
        # blank the artifact if applied. Reject blank-after-strip without mutating the
        # value (the body's own leading/trailing whitespace is preserved).
        if not value.strip():
            raise ValueError("must not be blank")
        return value
