# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""No-key behaviour: with no model key configured the LLM endpoints fail fast with 503 and make
no outbound call, and readiness reports not-ready while liveness stays up. All offline."""

from __future__ import annotations

import pytest
from httpx import AsyncClient

from app.config.config import Settings, ThemeClassifierStrategy, set_config

pytestmark = pytest.mark.asyncio


async def test_llm_endpoints_503_without_key_and_never_call_provider(client: AsyncClient, fake_provider) -> None:
    # Reconfigure to an empty key after the app is built; the guard and readiness read the config
    # at request time.
    set_config(Settings(gemini_api_key=""))

    generate = await client.post(
        "/api/v1/generate-content",
        json={"content": "x", "task_type": "derive_org_artifacts", "org_id": "o", "source_id": "s"},
    )
    assert generate.status_code == 503
    assert "no model key configured" in generate.json()["detail"]

    classify = await client.post(
        "/api/v1/classify-themes",
        json={"org_id": "org-gemini", "comments": ["needs a test"]},
    )
    assert classify.status_code == 503
    assert "no model key configured" in classify.json()["detail"]

    # The guard short-circuits before the endpoint reaches the provider, so no outbound call is made.
    assert fake_provider.last_request is None


async def test_classify_themes_sbert_strategy_not_blocked_by_missing_key(client: AsyncClient) -> None:
    # The sbert strategy is token-free, so a missing key must NOT short-circuit classify-themes.
    # With no key but the sbert strategy, the request reaches the sbert path (which here fails only
    # because no classifier is loaded) — never the "no model key configured" guard.
    set_config(Settings(gemini_api_key="", theme_classifier_strategy=ThemeClassifierStrategy.SBERT))

    resp = await client.post(
        "/api/v1/classify-themes",
        json={"org_id": "some-sbert-org", "comments": ["needs a test"]},
    )
    assert resp.status_code == 503
    detail = resp.json()["detail"]
    assert "no model key configured" not in detail
    assert "SBERT" in detail


async def test_readiness_503_without_key_but_liveness_ok(client: AsyncClient) -> None:
    set_config(Settings(gemini_api_key=""))

    # Liveness is independent of the key — the process is up.
    assert (await client.get("/_health")).status_code == 200

    readiness = await client.get("/readyz")
    assert readiness.status_code == 503
    body = readiness.json()
    assert body["status"] == "degraded"
    assert body["checks"]["llm_provider"] != "ok"
