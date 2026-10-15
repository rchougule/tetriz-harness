# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""POST /api/v1/classify-themes — PR review-theme classifier.

The strategy (SBERT vs Gemini) is resolved inside this module from ``org_id`` — callers
never pass it, so a later third option is a Python-only change. The SBERT path is local and
token-free; the Gemini path uses the shared ``LLMProvider``.
"""

from __future__ import annotations

import structlog
from fastapi import APIRouter, HTTPException, Request
from fastapi.concurrency import run_in_threadpool
from structlog.contextvars import bind_contextvars

from app.api.dependencies import model_key_configured
from app.classifiers.taxonomy import SBERT_MODEL_NAME
from app.config.config import ThemeClassifierStrategy, get_org_theme_classifier_strategy
from app.llm.exceptions import LLMAuthenticationError, LLMError, LLMRateLimitError
from app.llm.providers.gemini import GEMINI_PROVIDER_NAME
from app.llm.providers.registry import get_provider
from app.metrics import (
    CLASSIFIER_TOTAL,
    CLASSIFY_DURATION,
    ML_PROVIDER_NAME,
    STATUS_ERROR,
    STATUS_SUCCESS,
)
from app.models.classify_themes import ClassifyThemesRequest, ClassifyThemesResponse, ThemeClassification

logger = structlog.get_logger(__name__)

router = APIRouter(prefix="/api/v1", tags=["classify"])


async def _classify_gemini(request: ClassifyThemesRequest) -> ClassifyThemesResponse:
    from app.classifiers.gemini_theme_classifier import GeminiThemeClassifier

    provider = get_provider()
    classifier = GeminiThemeClassifier(provider)
    batch = await classifier.classify_batch(request.comments, request.max_themes_per_comment)
    return ClassifyThemesResponse(
        classifications=batch.classifications,
        strategy_used=ThemeClassifierStrategy.GEMINI.value,
        provider=GEMINI_PROVIDER_NAME,
        model=batch.model,
        usage=batch.usage,
    )


async def _classify_sbert(request: ClassifyThemesRequest, req: Request) -> ClassifyThemesResponse:
    classifier = getattr(req.app.state, "sbert_classifier", None)
    if classifier is None:
        CLASSIFIER_TOTAL.labels(ThemeClassifierStrategy.SBERT.value, STATUS_ERROR).inc()
        logger.error("SBERT classifier not loaded (ml extra not installed or still warming up); returning 503")
        raise HTTPException(
            status_code=503,
            detail="SBERT classifier not available; install the 'ml' extra or use the gemini strategy.",
        )
    # SBERT encode is CPU-bound — run in a thread so the event loop stays responsive.
    classifications: list[ThemeClassification] = await run_in_threadpool(
        classifier.classify_batch, request.comments, request.max_themes_per_comment
    )
    return ClassifyThemesResponse(
        classifications=classifications,
        strategy_used=ThemeClassifierStrategy.SBERT.value,
        provider=ML_PROVIDER_NAME,
        model=SBERT_MODEL_NAME,
        usage=None,
    )


@router.post("/classify-themes", response_model=ClassifyThemesResponse)
async def classify_themes(request: ClassifyThemesRequest, req: Request) -> ClassifyThemesResponse:
    bind_contextvars(org_id=request.org_id, task_type="classify_themes", comment_count=len(request.comments))

    strategy = get_org_theme_classifier_strategy(request.org_id)
    bind_contextvars(strategy=strategy.value)

    # Only the Gemini strategy calls the provider, so only it needs a key; the sbert strategy is
    # token-free and must stay usable with no key configured.
    if strategy == ThemeClassifierStrategy.GEMINI and not model_key_configured():
        CLASSIFIER_TOTAL.labels(strategy.value, STATUS_ERROR).inc()
        logger.error("classify-themes gemini strategy needs a model key; returning 503")
        raise HTTPException(status_code=503, detail="no model key configured")

    with CLASSIFY_DURATION.labels(strategy.value).time():
        try:
            if strategy == ThemeClassifierStrategy.GEMINI:
                result = await _classify_gemini(request)
            else:
                result = await _classify_sbert(request, req)
        except LLMRateLimitError as exc:
            CLASSIFIER_TOTAL.labels(strategy.value, STATUS_ERROR).inc()
            logger.warning("theme classifier rate-limited; returning 429")
            raise HTTPException(status_code=429, detail=exc.to_detail()) from exc
        except LLMAuthenticationError as exc:
            CLASSIFIER_TOTAL.labels(strategy.value, STATUS_ERROR).inc()
            logger.error("theme classifier auth failed; returning 502")
            raise HTTPException(status_code=502, detail=exc.to_detail()) from exc
        except LLMError as exc:
            CLASSIFIER_TOTAL.labels(strategy.value, STATUS_ERROR).inc()
            logger.error("theme classifier call failed; returning 502", error=str(exc))
            raise HTTPException(status_code=502, detail=exc.to_detail()) from exc

    CLASSIFIER_TOTAL.labels(strategy.value, STATUS_SUCCESS).inc()
    logger.info("classify-themes completed", result_count=len(result.classifications))
    return result
