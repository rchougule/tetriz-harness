# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Shared FastAPI dependencies for the API routes."""

from __future__ import annotations

from fastapi import HTTPException

from app.config.config import get_config


def model_key_configured() -> bool:
    """Whether a model (Gemini) key is configured. Endpoints that only sometimes need the key
    (classify-themes, whose sbert strategy is token-free) check this directly instead of using
    require_model_key as an unconditional dependency."""
    return bool(get_config().gemini_api_key)


def require_model_key() -> None:
    """Short-circuit an LLM endpoint with 503 when no model key is configured.

    The service boots with an empty key so liveness and the non-LLM flows still work, but an
    endpoint whose every path calls the provider (generate-content) has no credential to use.
    Failing here — before the endpoint builds a completion request or reaches the provider — means
    a keyless deployment returns a clear "not ready" rather than making a doomed outbound call that
    would surface as a provider 4xx/5xx.
    """
    if not model_key_configured():
        raise HTTPException(status_code=503, detail="no model key configured")
