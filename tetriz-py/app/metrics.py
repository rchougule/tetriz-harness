# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Prometheus collectors for tetriz-py.

Metric names are prefixed ``tetriz_py_``. Per the cardinality budget, ``org_id`` is NEVER
a label — it stays in structlog context only.
"""

from __future__ import annotations

from prometheus_client import Counter, Histogram

from app.llm.types import TokenUsage

# Status label values, shared across collectors.
STATUS_SUCCESS = "success"
STATUS_ERROR = "error"
STATUS_PARSE_ERROR = "parse_error"
STATUS_DROPPED_INDICES = "dropped_indices"

# The local (token-free) inference tier, surfaced alongside the LLM provider tiers.
ML_PROVIDER_NAME = "ml"


GENERATE_CONTENT_TOTAL = Counter(
    "tetriz_py_generate_content_total",
    "generate-content requests by task type and outcome.",
    ["task_type", "status"],
)

LLM_CALL_TOTAL = Counter(
    "tetriz_py_llm_call_total",
    "LLM/model calls by task, provider, model and outcome.",
    ["task_type", "provider", "model", "status"],
)

LLM_CALL_DURATION = Histogram(
    "tetriz_py_llm_call_duration_seconds",
    "LLM/model call latency by task, provider and model.",
    ["task_type", "provider", "model"],
)

LLM_TOKENS_TOTAL = Counter(
    "tetriz_py_llm_tokens_total",
    "LLM tokens consumed by task, provider, model and token kind.",
    ["task_type", "provider", "model", "kind"],
)

CLASSIFIER_TOTAL = Counter(
    "tetriz_py_theme_classifier_total",
    "classify-themes requests by strategy and outcome.",
    ["strategy", "status"],
)

CLASSIFY_DURATION = Histogram(
    "tetriz_py_theme_classify_duration_seconds",
    "classify-themes latency by strategy.",
    ["strategy"],
)

GEMINI_CHUNK_TOTAL = Counter(
    "tetriz_py_theme_gemini_chunk_total",
    "Gemini theme-classifier chunk calls by outcome.",
    ["status"],
)


def record_token_usage(labels: tuple[str, str, str], usage: TokenUsage | None) -> None:
    """Add one call's token usage to LLM_TOKENS_TOTAL under (task_type, provider, model).

    A no-op when usage is absent — a transport failure that never billed a response.
    """
    if usage is None:
        return
    task_type, provider, model = labels
    LLM_TOKENS_TOTAL.labels(task_type, provider, model, "prompt").inc(usage.prompt_tokens)
    LLM_TOKENS_TOTAL.labels(task_type, provider, model, "completion").inc(usage.completion_tokens)
    LLM_TOKENS_TOTAL.labels(task_type, provider, model, "thoughts").inc(usage.thoughts_tokens)
