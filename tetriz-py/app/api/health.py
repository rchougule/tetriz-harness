# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Health endpoints.

- /_health : liveness (always 200 while the process is up)
- /readyz  : readiness (the LLM provider is registered AND a model key is configured)
"""

from fastapi import APIRouter, Response

from app.config.config import get_config
from app.llm.providers.registry import is_provider_registered
from app.models.health import HealthResponse, ReadinessResponse

router = APIRouter(tags=["health"])


@router.get("/_health", response_model=HealthResponse)
async def liveness() -> HealthResponse:
    return HealthResponse(status="ok")


@router.get("/readyz", response_model=ReadinessResponse)
async def readiness(response: Response) -> ReadinessResponse:
    # The service boots with an empty key (so liveness still passes), but it cannot serve an LLM
    # request without one, so readiness is not ready until a key is configured — distinct from the
    # provider never having been registered.
    provider_registered = is_provider_registered()
    has_model_key = bool(get_config().gemini_api_key)
    if not provider_registered:
        llm_status = "not_registered"
    elif not has_model_key:
        llm_status = "no_model_key"
    else:
        llm_status = "ok"
    ready = provider_registered and has_model_key
    if not ready:
        response.status_code = 503
    return ReadinessResponse(status="ok" if ready else "degraded", checks={"llm_provider": llm_status})
