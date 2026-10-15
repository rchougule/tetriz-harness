# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Endpoint tests for POST /api/v1/classify-themes.

The SBERT path is exercised with an injected fake classifier (no sentence-transformers /
torch needed); the Gemini path uses the mocked provider. No test needs a live key or network.
"""

from __future__ import annotations

import json

import pytest
from httpx import AsyncClient

from app.models.classify_themes import ThemeClassification

pytestmark = pytest.mark.asyncio


class _FakeSBERT:
    """Stands in for SBERTThemeClassifier so the SBERT endpoint path is tested without the
    optional ML stack. Returns one best theme per comment, in input order."""

    def classify_batch(self, comments: list[str], max_themes: int = 1) -> list[ThemeClassification]:
        return [ThemeClassification(index=i, theme="testing", confidence=0.7) for i in range(len(comments))]


async def test_sbert_strategy_with_injected_classifier(client: AsyncClient) -> None:
    client.app.state.sbert_classifier = _FakeSBERT()  # type: ignore[attr-defined]
    resp = await client.post(
        "/api/v1/classify-themes",
        json={"org_id": "org-sbert", "comments": ["add a test for this", "nil check missing"]},
    )
    assert resp.status_code == 200
    body = resp.json()
    assert body["strategy_used"] == "sbert"
    assert body["provider"] == "ml"
    assert len(body["classifications"]) == 2
    assert body["usage"] is None


async def test_sbert_strategy_returns_503_when_classifier_absent(client: AsyncClient) -> None:
    # Default fixture leaves app.state.sbert_classifier = None (ml extra not installed).
    resp = await client.post(
        "/api/v1/classify-themes",
        json={"org_id": "org-sbert", "comments": ["anything"]},
    )
    assert resp.status_code == 503


async def test_gemini_strategy_with_mocked_provider(client: AsyncClient, fake_provider) -> None:
    fake_provider.content = json.dumps(
        {
            "classifications": [
                {"index": 0, "theme": "security", "confidence": 0.9},
                {"index": 1, "theme": "testing", "confidence": 0.8},
            ]
        }
    )
    resp = await client.post(
        "/api/v1/classify-themes",
        json={"org_id": "org-gemini", "comments": ["secret leaked here", "needs a unit test"]},
    )
    assert resp.status_code == 200
    body = resp.json()
    assert body["strategy_used"] == "gemini"
    assert body["provider"] == "gemini"
    themes = {c["theme"] for c in body["classifications"]}
    assert themes == {"security", "testing"}
    assert body["usage"]["total_tokens"] == 15


async def test_empty_comments_is_rejected_422(client: AsyncClient) -> None:
    resp = await client.post(
        "/api/v1/classify-themes",
        json={"org_id": "org-sbert", "comments": []},
    )
    assert resp.status_code == 422
