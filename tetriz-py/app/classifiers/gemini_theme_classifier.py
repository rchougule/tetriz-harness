# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Gemini-backed theme classifier — the per-org opt-in alternative to SBERT.

Batches review comments into small groups and asks Gemini to return a structured JSON
classification per comment, through the shared ``LLMProvider`` interface. The theme set is
the same 13 themes the SBERT path uses, so the two strategies return comparable output.
"""

from __future__ import annotations

import asyncio
import json
import time
from typing import NamedTuple

import structlog

from app.classifiers.taxonomy import OTHER_THEME, THEME_DESCRIPTIONS
from app.llm.exceptions import LLMError
from app.llm.providers.base import LLMProvider
from app.llm.providers.gemini import GEMINI_DEFAULT_MODEL, GEMINI_PROVIDER_NAME
from app.llm.types import CompletionRequest, ThinkingLevel, TokenUsage
from app.metrics import (
    GEMINI_CHUNK_TOTAL,
    STATUS_DROPPED_INDICES,
    STATUS_PARSE_ERROR,
    STATUS_SUCCESS,
    record_token_usage,
)
from app.models.classify_themes import ThemeClassification

logger = structlog.get_logger(__name__)


# Batch size tuned for Gemini Flash — small enough to keep individual calls fast and the
# JSON response parseable, large enough to amortise per-call overhead.
BATCH_SIZE = 15

# Wave size: how many chunk calls hit Gemini at once. Without bounding, a full classify
# request would fan out to hundreds of simultaneous Gemini calls, tripping rate/connection
# limits. classify_batch runs chunks in waves of this size, which also means a failure in one
# wave stops the next wave from firing.
_MAX_CONCURRENT_CHUNKS = 8

# Allowed theme slugs for structured-output validation.
_ALLOWED_THEMES: list[str] = list(THEME_DESCRIPTIONS.keys()) + [OTHER_THEME]

# A secondary (rank-2+) theme is kept only at/above this model confidence, mirroring the SBERT
# path's threshold gate on secondaries: the top theme is always kept, but an additional theme
# must be clearly present to credit the comment with it.
_MIN_SECONDARY_THEME_CONFIDENCE = 0.5


def _is_usable_local_index(value: object, chunk_len: int) -> bool:
    """Whether a model-returned chunk-local index can be trusted to map a result back to its
    comment. The model is trusted for theme/confidence but NOT the index, so an index is usable
    only if it is a real int (bool is an int subclass — reject it so a JSON `true` can't pass as
    1) and in range [0, chunk_len)."""
    return isinstance(value, int) and not isinstance(value, bool) and 0 <= value < chunk_len


def _canonicalize_top_n(rows: list[tuple[str, float]], max_themes: int) -> list[tuple[str, float]]:
    """Reduce one comment's model-returned (theme, confidence) rows to the trustworthy top-N.

    The model is untrusted for the *set* it returns, so enforce the guarantees the SBERT path
    has structurally: distinct themes (dedup, keep the max confidence per theme), "other" only
    when NO real theme applies (never mixed with a real one), a confidence floor on secondaries,
    and at most max_themes rows, best-first.
    """
    best: dict[str, float] = {}
    for theme, confidence in rows:
        if theme == OTHER_THEME:
            continue
        if confidence > best.get(theme, -1.0):
            best[theme] = confidence
    if not best:
        # No real theme for this comment → a single "other" (highest confidence seen).
        return [(OTHER_THEME, max((c for _, c in rows), default=0.0))]
    # Stable sort by descending confidence: an exact tie keeps first-returned order.
    ranked = sorted(best.items(), key=lambda kv: -kv[1])
    kept = [ranked[0]]  # the top theme is always kept
    for theme, confidence in ranked[1:]:
        if len(kept) >= max_themes:
            break
        if confidence >= _MIN_SECONDARY_THEME_CONFIDENCE:
            kept.append((theme, confidence))
    return kept


def _system_prompt(max_themes: int) -> str:
    """System prompt, built per request from max_themes. The themes are listed inline so the
    model treats them as a closed set rather than free text."""
    if max_themes <= 1:
        return (
            "You classify short PR review comments into engineering themes.\n"
            "For each comment, pick the single best-matching theme from this list: "
            f"{', '.join(_ALLOWED_THEMES)}.\n"
            f"If no theme fits, use '{OTHER_THEME}'. confidence must be in [0.0, 1.0].\n"
            "Return a JSON object with a 'classifications' array; items MUST appear in the "
            "same order as the input, and the 'index' field MUST equal the comment's "
            "zero-based position."
        )
    return (
        "You classify short PR review comments into engineering themes.\n"
        f"For each comment, pick up to {max_themes} best-matching themes from this list, "
        f"most relevant first: {', '.join(_ALLOWED_THEMES)}.\n"
        "Emit a separate array item per (comment, theme) pair, all sharing that comment's "
        "index. Include a theme ONLY if it genuinely applies — a comment about a single topic "
        f"must get exactly one theme. Use '{OTHER_THEME}' only when NO theme fits, and never "
        f"pair '{OTHER_THEME}' with a real theme for the same comment. confidence must be in "
        "[0.0, 1.0].\n"
        "Return a JSON object with a 'classifications' array; every item's 'index' field MUST "
        "equal the referenced comment's zero-based position."
    )


# JSON schema for structured Gemini output. Fields match ThemeClassification (plus a top-level
# wrapper so Gemini can return an object, not a bare array).
_RESPONSE_SCHEMA: dict = {
    "type": "object",
    "properties": {
        "classifications": {
            "type": "array",
            "items": {
                "type": "object",
                "properties": {
                    "index": {"type": "integer"},
                    "theme": {"type": "string", "enum": _ALLOWED_THEMES},
                    "confidence": {"type": "number"},
                },
                "required": ["index", "theme", "confidence"],
            },
        }
    },
    "required": ["classifications"],
}


class ClassifyBatchResult(NamedTuple):
    """classify_batch output. `usage` is the SUM across all chunk calls and `model` the served
    model (the last chunk's — representative, since chunks share the configured model)."""

    classifications: list[ThemeClassification]
    usage: TokenUsage
    model: str


class GeminiThemeClassifier:
    """Classifies comments via an `LLMProvider` (typically the configured GeminiProvider)."""

    def __init__(self, provider: LLMProvider, model: str = GEMINI_DEFAULT_MODEL) -> None:
        self._provider = provider
        self._model = model

    async def classify_batch(self, comments: list[str], max_themes: int = 1) -> ClassifyBatchResult:
        if not comments:
            return ClassifyBatchResult([], TokenUsage(), self._model)

        # Chunk comments into BATCH_SIZE groups, then run them in bounded WAVES: at most
        # _MAX_CONCURRENT_CHUNKS Gemini calls in flight at once, and a chunk failure stops
        # scheduling the next wave so one error can't keep burning the rate-limit budget.
        chunks = [(offset, comments[offset : offset + BATCH_SIZE]) for offset in range(0, len(comments), BATCH_SIZE)]
        merged: list[ThemeClassification] = []
        agg = TokenUsage()
        served_model = self._model
        for wave_start in range(0, len(chunks), _MAX_CONCURRENT_CHUNKS):
            wave = chunks[wave_start : wave_start + _MAX_CONCURRENT_CHUNKS]
            wave_results = await asyncio.gather(
                *(self._classify_chunk(offset, chunk, max_themes) for offset, chunk in wave)
            )
            for results, usage, model in wave_results:
                merged.extend(results)
                served_model = model  # chunks share the configured model
                if usage is not None:
                    agg = TokenUsage(
                        prompt_tokens=agg.prompt_tokens + usage.prompt_tokens,
                        completion_tokens=agg.completion_tokens + usage.completion_tokens,
                        thoughts_tokens=agg.thoughts_tokens + usage.thoughts_tokens,
                        total_tokens=agg.total_tokens + usage.total_tokens,
                    )
        return ClassifyBatchResult(merged, agg, served_model)

    async def _classify_chunk(
        self, offset: int, chunk: list[str], max_themes: int = 1
    ) -> tuple[list[ThemeClassification], TokenUsage | None, str]:
        user_content = json.dumps({"comments": [{"index": i, "body": body} for i, body in enumerate(chunk)]})

        # temperature is intentionally left to the model default: Gemini 3.x ignores it and
        # 2.x classification is near-deterministic either way.
        request = CompletionRequest(
            model=self._model,
            system_prompt=_system_prompt(max_themes),
            user_content=user_content,
            temperature=None,
            response_json_schema=_RESPONSE_SCHEMA,
            thinking_level=ThinkingLevel.MINIMAL,
            task_type="classify_themes",
        )
        start = time.perf_counter()
        response = await self._provider.complete(request)
        record_token_usage(("classify_themes", GEMINI_PROVIDER_NAME, response.model), response.usage)
        latency_ms = round((time.perf_counter() - start) * 1000, 2)

        try:
            parsed = json.loads(response.content)
            raw_items = parsed.get("classifications", [])
        except (json.JSONDecodeError, AttributeError) as exc:
            GEMINI_CHUNK_TOTAL.labels(STATUS_PARSE_ERROR).inc()
            logger.error(
                "Gemini classifier chunk returned unparseable JSON; raising as LLMError",
                chunk_offset=offset,
                chunk_size=len(chunk),
                latency_ms=latency_ms,
                error=str(exc),
            )
            raise LLMError(
                f"Gemini classifier returned unparseable JSON: {exc}",
                provider=response.provider,
            ) from exc

        GEMINI_CHUNK_TOTAL.labels(STATUS_SUCCESS).inc()

        # Group each valid comment's rows by chunk-local index, then canonicalize to the
        # trustworthy top-N. The model is trusted for theme/confidence but NOT for the index: a
        # hallucinated, non-int, or out-of-range index would be offset onto the wrong comment.
        rows_by_index: dict[int, list[tuple[str, float]]] = {}
        dropped = 0
        for item in raw_items:
            local_idx = item.get("index")
            if not _is_usable_local_index(local_idx, len(chunk)):
                dropped += 1
                continue
            theme = item.get("theme", OTHER_THEME)
            theme = theme if theme in _ALLOWED_THEMES else OTHER_THEME
            confidence = max(0.0, min(1.0, float(item.get("confidence", 0.0))))  # clamp to [0, 1]
            rows_by_index.setdefault(local_idx, []).append((theme, confidence))

        results: list[ThemeClassification] = []
        for local_idx, rows in rows_by_index.items():
            for theme, confidence in _canonicalize_top_n(rows, max_themes):
                results.append(ThemeClassification(index=offset + local_idx, theme=theme, confidence=confidence))

        if dropped:
            # Sustained nonzero drops mean the model is systematically mis-indexing (themes get
            # under-attributed); a one-off is benign. Metered so the rate is trendable.
            GEMINI_CHUNK_TOTAL.labels(STATUS_DROPPED_INDICES).inc()
            logger.warning(
                "Gemini classifier chunk dropped invalid/duplicate indices",
                chunk_offset=offset,
                chunk_size=len(chunk),
                dropped=dropped,
            )

        logger.info(
            "Gemini classifier chunk completed",
            chunk_offset=offset,
            chunk_size=len(chunk),
            returned_count=len(results),
            latency_ms=latency_ms,
        )
        return results, response.usage, response.model
