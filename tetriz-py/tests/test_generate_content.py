# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Endpoint tests for POST /api/v1/generate-content with a MOCKED provider (no live Gemini)."""

from __future__ import annotations

import json

import pytest
from httpx import AsyncClient

from app.llm.exceptions import LLMAuthenticationError, LLMRateLimitError

pytestmark = pytest.mark.asyncio


async def test_health_and_readiness(client: AsyncClient) -> None:
    assert (await client.get("/_health")).status_code == 200
    # The fake provider is registered, so readiness reports ok.
    readiness = await client.get("/readyz")
    assert readiness.status_code == 200
    assert readiness.json()["status"] == "ok"


async def test_classify_harness_intent_happy_path(client: AsyncClient, fake_provider) -> None:
    fake_provider.content = json.dumps(
        {"intent_class": "orchestrator", "confidence": 0.82, "rationale": "drains a queue"}
    )
    resp = await client.post(
        "/api/v1/generate-content",
        json={
            "content": "name: ship-queue\ndescription: drain a labeled queue of issues",
            "task_type": "classify_harness_intent",
            "org_id": "org-1",
            "source_id": "artifact-7",
        },
    )
    assert resp.status_code == 200
    body = resp.json()
    assert body["task_type"] == "classify_harness_intent"
    assert body["provider"] == "gemini"
    assert json.loads(body["result"])["intent_class"] == "orchestrator"
    # The task's system prompt and structured-output schema reached the provider.
    assert fake_provider.last_request is not None
    assert fake_provider.last_request.response_json_schema is not None
    assert "Harness Artifact Intent Classifier" in fake_provider.last_request.system_prompt


async def test_unknown_task_type_is_400(client: AsyncClient) -> None:
    resp = await client.post(
        "/api/v1/generate-content",
        json={
            "content": "x",
            "task_type": "recommend_marketplace_artifacts",
            "org_id": "org-1",
            "source_id": "s-1",
        },
    )
    assert resp.status_code == 400


async def test_unknown_task_type_metric_uses_unknown_label(client: AsyncClient) -> None:
    """The unknown-task_type error metric is labelled with the constant "unknown", never the raw
    client value — otherwise an attacker-controlled task_type blows up the metric's cardinality.
    The 400 detail still names the actual value for the caller."""
    from prometheus_client import REGISTRY

    raw_task_type = "totally_made_up_task_type_xyzzy"
    metric = "tetriz_py_generate_content_total"
    before = REGISTRY.get_sample_value(metric, {"task_type": "unknown", "status": "error"}) or 0.0

    resp = await client.post(
        "/api/v1/generate-content",
        json={"content": "x", "task_type": raw_task_type, "org_id": "o", "source_id": "s"},
    )
    assert resp.status_code == 400
    assert raw_task_type in resp.json()["detail"]  # the detail still names the rejected value

    after = REGISTRY.get_sample_value(metric, {"task_type": "unknown", "status": "error"}) or 0.0
    assert after == before + 1
    # The raw client value must NOT appear as a metric label.
    assert REGISTRY.get_sample_value(metric, {"task_type": raw_task_type, "status": "error"}) is None


async def test_metrics_endpoint_served_without_redirect(client: AsyncClient) -> None:
    """GET /metrics serves the Prometheus exposition on the bare path with no 307 redirect."""
    resp = await client.get("/metrics")  # httpx does not follow redirects by default
    assert resp.status_code == 200
    assert resp.headers["content-type"].startswith("text/plain")
    assert "tetriz_py_" in resp.text


async def test_rate_limit_maps_to_429(client: AsyncClient, fake_provider) -> None:
    fake_provider.to_raise = LLMRateLimitError("slow down", provider="gemini", model="gemini-2.5-flash")
    resp = await client.post(
        "/api/v1/generate-content",
        json={"content": "x", "task_type": "derive_org_artifacts", "org_id": "o", "source_id": "s"},
    )
    assert resp.status_code == 429


async def test_auth_failure_maps_to_502(client: AsyncClient, fake_provider) -> None:
    fake_provider.to_raise = LLMAuthenticationError("bad key", provider="gemini", model="gemini-2.5-flash")
    resp = await client.post(
        "/api/v1/generate-content",
        json={"content": "x", "task_type": "generate_harness_fix", "org_id": "o", "source_id": "s"},
    )
    assert resp.status_code == 502


async def test_empty_content_is_rejected_422(client: AsyncClient) -> None:
    resp = await client.post(
        "/api/v1/generate-content",
        json={"content": "", "task_type": "derive_org_artifacts", "org_id": "o", "source_id": "s"},
    )
    assert resp.status_code == 422
