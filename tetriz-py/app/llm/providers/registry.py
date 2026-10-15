# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Provider lifecycle: a single registered LLMProvider the service serves requests with.

The registry indirection keeps every endpoint resolving its provider through one
injectable seam (FastAPI ``Depends``), which the tests override with a fake provider so
no test needs a live key.
"""

from __future__ import annotations

import structlog

from app.llm.providers.base import LLMProvider

logger = structlog.get_logger(__name__)

_provider: LLMProvider | None = None


def set_provider(provider: LLMProvider) -> None:
    """Register the process-wide provider. Called from the FastAPI lifespan."""
    global _provider
    _provider = provider


def clear_provider() -> None:
    """Clear the registered provider. Called on shutdown."""
    global _provider
    _provider = None


def is_provider_registered() -> bool:
    """Whether a provider has been registered (used by the readiness probe)."""
    return _provider is not None


def get_provider() -> LLMProvider:
    """FastAPI ``Depends()``-compatible getter for the registered provider."""
    if _provider is None:
        raise RuntimeError("LLM provider not registered. Is the app lifespan running?")
    return _provider


async def close_provider() -> None:
    """Close the registered provider, swallowing a close failure so shutdown never raises."""
    if _provider is None:
        return
    try:
        await _provider.close()
    except Exception as exc:  # noqa: BLE001 - shutdown must not raise
        logger.error("failed to close LLM provider during shutdown", error=str(exc))
