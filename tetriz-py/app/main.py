# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""FastAPI application entry point for tetriz-py.

Startup: load config, configure logging, build and register the Gemini provider, and load
the SBERT theme classifier when its optional dependency is installed. Shutdown: close the
provider. The Gemini key arrives from the environment (``TETRIZ_GEMINI_API_KEY`` or
``GEMINI_API_KEY``) at run time only — never baked in.
"""

from __future__ import annotations

from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

import structlog
from fastapi import FastAPI
from fastapi.exceptions import RequestValidationError
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest
from starlette.requests import Request
from starlette.responses import JSONResponse, Response

from app.api.classify_themes import router as classify_themes_router
from app.api.generate_content import router as generate_content_router
from app.api.health import router as health_router
from app.config.config import Environment, ThemeClassifierStrategy, get_config
from app.config.logging import setup_logging
from app.llm.providers.gemini import GeminiProvider
from app.llm.providers.registry import clear_provider, close_provider, set_provider

logger = structlog.get_logger(__name__)


def _load_sbert_classifier() -> object | None:
    """Load the SBERT theme classifier, or None when its optional dependency is absent.

    The base install omits sentence-transformers (the ``ml`` extra). When it is missing the
    service still boots and every other endpoint works; /classify-themes on the SBERT strategy
    then returns 503 until the extra is installed.
    """
    try:
        from app.classifiers.review_theme_classifier import SBERTThemeClassifier
    except ImportError:
        logger.info(
            "sentence-transformers not installed; SBERT theme classifier disabled",
            hint="install the 'ml' extra (uv sync --extra ml) to enable the SBERT strategy",
        )
        return None
    return SBERTThemeClassifier()


@asynccontextmanager
async def lifespan(app: FastAPI) -> AsyncIterator[None]:
    config = get_config()
    setup_logging()

    # Build the Gemini provider from the configured key. An empty key still builds the provider
    # (with a placeholder) so the service boots and health checks pass; LLM calls then fail until
    # the key is set.
    api_key = config.gemini_api_key
    if not api_key:
        api_key = "not-configured"
        logger.warning(
            "Gemini API key not configured; LLM calls will fail until a key is set",
            hint="set TETRIZ_GEMINI_API_KEY or GEMINI_API_KEY",
        )
    set_provider(GeminiProvider(api_key=api_key, timeout=config.request_timeout))

    # Load the SBERT classifier only when it could be used (default strategy or any override is
    # sbert) and its dependency is present; the Gemini strategy needs no local model.
    uses_sbert = config.theme_classifier_strategy == ThemeClassifierStrategy.SBERT or any(
        s == ThemeClassifierStrategy.SBERT for s in config.theme_classifier_overrides.values()
    )
    app.state.sbert_classifier = _load_sbert_classifier() if uses_sbert else None

    logger.info(
        "tetriz-py starting",
        environment=config.environment.value,
        port=config.port,
        sbert_loaded=app.state.sbert_classifier is not None,
    )
    yield

    await close_provider()
    clear_provider()
    logger.info("tetriz-py shutting down")


def create_app() -> FastAPI:
    config = get_config()
    application = FastAPI(
        title="tetriz-py",
        description="LLM service for the Tetriz Harness recommendations engine.",
        version="0.1.0",
        docs_url="/docs" if config.environment == Environment.DEVELOPMENT else None,
        redoc_url=None,
        lifespan=lifespan,
    )

    @application.exception_handler(RequestValidationError)
    async def validation_error_handler(_request: Request, exc: RequestValidationError) -> JSONResponse:
        """Strip user input from validation errors so request bodies are not echoed back."""
        errors = [{k: v for k, v in err.items() if k not in ("input", "ctx")} for err in exc.errors()]
        return JSONResponse(status_code=422, content={"detail": errors})

    application.include_router(health_router)
    application.include_router(generate_content_router)
    application.include_router(classify_themes_router)

    # Serve the exposition at the bare /metrics path directly. A sub-app mount on "/metrics"
    # redirects GET /metrics → /metrics/ (307), which some scrapers do not follow; an explicit
    # route returns the metrics on the exact path with no redirect.
    @application.get("/metrics")
    async def metrics() -> Response:
        return Response(content=generate_latest(), media_type=CONTENT_TYPE_LATEST)

    return application


app = create_app()


def main() -> None:
    import uvicorn

    config = get_config()
    uvicorn.run(
        "app.main:app",
        host=config.host,
        port=config.port,
        workers=config.workers,
        log_level=config.log_level.lower(),
    )


if __name__ == "__main__":
    main()
