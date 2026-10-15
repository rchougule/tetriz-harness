# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Pydantic request/response models for POST /api/v1/classify-themes."""

from __future__ import annotations

from pydantic import BaseModel, Field

from app.llm.types import TokenUsage

# The review-theme taxonomy size — the upper bound on themes returned per comment. Kept
# here (not imported from the SBERT module) so the request model does not pull in the
# optional sentence-transformers stack just to validate a bound.
THEME_COUNT = 13


class ClassifyThemesRequest(BaseModel):
    # Resolves the per-org classifier strategy (sbert vs gemini).
    org_id: str = Field(..., min_length=1, description="Organization ID; resolves the classifier strategy.")
    comments: list[str] = Field(
        ...,
        min_length=1,
        # Matches the Go aggregator's per-call chunk size: a busy repo's review bodies are
        # split into <=3000-comment batches, so this cap must not reject a full batch.
        max_length=3000,
        description="Review comment bodies to classify. Order-preserving; each output references its input by index.",
    )
    # Cap on themes returned per comment. 1 (default) = one best theme per comment; >1 lets a
    # single bulky comment map to several themes it genuinely spans. Fewer than the cap are
    # returned when only one/two clear the confidence gate. Upper bound is the theme count.
    max_themes_per_comment: int = Field(
        1,
        ge=1,
        le=THEME_COUNT,
        description="Max themes to return per comment (1 = single best; >1 = top-N for multi-topic comments).",
    )


class ThemeClassification(BaseModel):
    index: int = Field(..., ge=0, description="Index of the input comment in the request.")
    # Theme slug (one of THEME_DESCRIPTIONS keys) or the literal string "other".
    theme: str
    confidence: float = Field(..., ge=0.0, le=1.0, description="Cosine similarity (SBERT) or LLM score (Gemini).")


class ClassifyThemesResponse(BaseModel):
    classifications: list[ThemeClassification]
    # "sbert" | "gemini" — tells the caller which strategy actually ran.
    strategy_used: str
    # provider ∈ {gemini, ml}; model is the served Gemini model or the SBERT model name.
    # usage is populated only for the Gemini strategy; SBERT is local + token-free → null.
    provider: str | None = Field(None, description="gemini | ml — the tier that served this request.")
    model: str | None = Field(None, description="Served model (Gemini model id or SBERT model name).")
    usage: TokenUsage | None = Field(None, description="Aggregated token usage (Gemini strategy only).")
