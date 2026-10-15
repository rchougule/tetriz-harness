# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Shared test fixtures.

Every test runs OFFLINE: the LLM provider is a fake and the SBERT classifier is either
injected as a fake or deliberately absent. No test needs a Gemini key or network.
"""

from __future__ import annotations

from collections.abc import AsyncIterator, Iterator

import pytest
import pytest_asyncio
from httpx import ASGITransport, AsyncClient

from app.config.config import Settings, ThemeClassifierStrategy, reset_config, set_config
from app.llm.providers.base import LLMProvider
from app.llm.providers.registry import clear_provider, get_provider, set_provider
from app.llm.types import CompletionRequest, CompletionResponse, TokenUsage
from app.main import create_app


class FakeProvider(LLMProvider):
    """An LLMProvider that returns canned content, or raises a preset error, without a network
    call. Tests set ``content`` (the JSON/text the model would return) or ``to_raise``."""

    def __init__(self) -> None:
        self.content: str = "{}"
        self.to_raise: Exception | None = None
        self.model: str = "gemini-2.5-flash"
        self.last_request: CompletionRequest | None = None

    async def complete(self, request: CompletionRequest) -> CompletionResponse:
        self.last_request = request
        if self.to_raise is not None:
            raise self.to_raise
        return CompletionResponse(
            content=self.content,
            model=self.model,
            provider="gemini",
            usage=TokenUsage(prompt_tokens=10, completion_tokens=5, total_tokens=15),
        )

    async def close(self) -> None:
        return None


@pytest.fixture(autouse=True)
def _test_config() -> Iterator[None]:
    """Pin a known config so tests never read a real GEMINI_API_KEY from the environment.

    Org ``org-gemini`` is routed to the Gemini strategy; everything else defaults to SBERT.
    """
    set_config(
        Settings(
            gemini_api_key="test-key",
            theme_classifier_strategy=ThemeClassifierStrategy.SBERT,
            theme_classifier_overrides={"org-gemini": ThemeClassifierStrategy.GEMINI},
        )
    )
    yield
    reset_config()


@pytest.fixture
def fake_provider() -> FakeProvider:
    return FakeProvider()


@pytest_asyncio.fixture
async def client(fake_provider: FakeProvider) -> AsyncIterator[AsyncClient]:
    """An httpx client over the ASGI app with the LLM provider overridden by the fake.

    ASGITransport does not run the app lifespan, so no real GeminiProvider is built and no
    SBERT model is loaded; a test that needs a classifier sets ``app.state.sbert_classifier``.
    """
    app = create_app()
    # Cover both seams: generate-content resolves the provider via Depends(get_provider), while
    # the Gemini classify path reads the registry directly — register the fake in both.
    app.dependency_overrides[get_provider] = lambda: fake_provider
    set_provider(fake_provider)
    app.state.sbert_classifier = None
    transport = ASGITransport(app=app)
    try:
        async with AsyncClient(transport=transport, base_url="http://test") as async_client:
            # Expose the app so a test can set app.state (e.g. inject a fake SBERT classifier).
            async_client.app = app  # type: ignore[attr-defined]
            yield async_client
    finally:
        clear_provider()
