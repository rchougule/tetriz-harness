# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""POST /api/v1/generate-content — task-based LLM content generation.

The caller sends a ``task_type``; the endpoint looks it up in ``TASK_REGISTRY`` for the
system prompt, model, and parameters, runs one provider call, and maps the LLM error
hierarchy onto HTTP status codes.
"""

from __future__ import annotations

import time

import structlog
from fastapi import APIRouter, Depends, HTTPException
from structlog.contextvars import bind_contextvars

from app.api.dependencies import require_model_key
from app.config.config import get_config
from app.llm.exceptions import (
    LLMAuthenticationError,
    LLMError,
    LLMInvalidRequestError,
    LLMRateLimitError,
)
from app.llm.providers.base import LLMProvider
from app.llm.providers.gemini import GEMINI_DEFAULT_MODEL
from app.llm.providers.registry import get_provider
from app.llm.router import get_task
from app.llm.types import CompletionRequest
from app.metrics import (
    GENERATE_CONTENT_TOTAL,
    LLM_CALL_DURATION,
    LLM_CALL_TOTAL,
    STATUS_ERROR,
    STATUS_SUCCESS,
    record_token_usage,
)
from app.models.generate_content import GenerateContentRequest, GenerateContentResponse

logger = structlog.get_logger(__name__)

router = APIRouter(prefix="/api/v1", tags=["generate"])


@router.post(
    "/generate-content",
    response_model=GenerateContentResponse,
    dependencies=[Depends(require_model_key)],
)
async def generate_content(
    request: GenerateContentRequest,
    provider: LLMProvider = Depends(get_provider),
) -> GenerateContentResponse:
    bind_contextvars(org_id=request.org_id, task_type=request.task_type, source_id=request.source_id)

    task = get_task(request.task_type)
    if task is None:
        # Label with the constant "unknown", never the raw client value — an unknown task_type is
        # unbounded caller input and would blow up the metric's cardinality. The 400 detail still
        # names the actual value so the caller can see what was rejected.
        GENERATE_CONTENT_TOTAL.labels("unknown", STATUS_ERROR).inc()
        raise HTTPException(status_code=400, detail=f"unknown task_type {request.task_type!r}")

    model = task.model or get_config().gemini_model or GEMINI_DEFAULT_MODEL

    # Caller overrides win over the task default, then clamp a caller-supplied max_tokens to
    # the task's ceiling so a request cannot ask for more than the task is sized for.
    temperature = request.temperature if request.temperature is not None else task.temperature
    max_tokens = request.max_tokens if request.max_tokens is not None else task.max_tokens
    if max_tokens is not None and task.max_output_tokens is not None:
        max_tokens = min(max_tokens, task.max_output_tokens)

    comp_req = CompletionRequest(
        model=model,
        system_prompt=task.system_prompt,
        user_content=request.content,
        temperature=temperature,
        max_tokens=max_tokens,
        response_json_schema=task.response_json_schema or None,
        thinking_level=task.thinking_level,
        thinking_budget=task.thinking_budget,
        timeout_seconds=task.request_timeout_seconds,
        task_type=request.task_type,
    )

    labels = (request.task_type, task.provider, model)
    start = time.perf_counter()
    try:
        response = await provider.complete(comp_req)
    except LLMRateLimitError as exc:
        _fail(labels, request.task_type)
        logger.warning("generate-content rate-limited; returning 429", error=str(exc))
        raise HTTPException(status_code=429, detail=exc.to_detail()) from exc
    except LLMAuthenticationError as exc:
        _fail(labels, request.task_type)
        logger.error("generate-content auth failed; returning 502")
        raise HTTPException(status_code=502, detail=exc.to_detail()) from exc
    except LLMInvalidRequestError as exc:
        _fail(labels, request.task_type)
        logger.error("generate-content rejected deterministically; returning 400", error=str(exc))
        raise HTTPException(status_code=400, detail=exc.to_detail()) from exc
    except LLMError as exc:
        # A billed-but-rejected response (truncation, safety block) still carries token usage —
        # record it so its cost is not lost — then surface as a retryable 502.
        record_token_usage((request.task_type, task.provider, exc.model or model), exc.usage)
        _fail(labels, request.task_type)
        logger.error("generate-content LLM call failed; returning 502", error=str(exc))
        raise HTTPException(status_code=502, detail=exc.to_detail()) from exc
    finally:
        LLM_CALL_DURATION.labels(*labels).observe(time.perf_counter() - start)

    LLM_CALL_TOTAL.labels(*labels, STATUS_SUCCESS).inc()
    GENERATE_CONTENT_TOTAL.labels(request.task_type, STATUS_SUCCESS).inc()
    record_token_usage((request.task_type, task.provider, response.model), response.usage)

    logger.info("generate-content completed", model=response.model)
    return GenerateContentResponse(
        result=response.content,
        task_type=request.task_type,
        model=response.model,
        provider=response.provider,
        usage=response.usage,
    )


def _fail(labels: tuple[str, str, str], task_type: str) -> None:
    LLM_CALL_TOTAL.labels(*labels, STATUS_ERROR).inc()
    GENERATE_CONTENT_TOTAL.labels(task_type, STATUS_ERROR).inc()
