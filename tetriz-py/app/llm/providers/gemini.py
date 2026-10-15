# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Google Gemini provider via the google-genai SDK.

A single-shot prompt-to-text adapter: it maps a ``CompletionRequest`` onto one
``generate_content`` call, selects the right thinking control for the target model's
major version, and translates vendor errors and non-clean finish reasons into the
``LLMError`` hierarchy. The API key is supplied at construction from the environment and
is never logged.
"""

from __future__ import annotations

import re

import structlog
from google import genai
from google.genai import types
from google.genai.errors import ClientError, ServerError

from app.llm.exceptions import (
    LLMAuthenticationError,
    LLMError,
    LLMInvalidRequestError,
    LLMOutputTruncatedError,
    LLMRateLimitError,
    LLMSafetyBlockedError,
    LLMUnavailableError,
)
from app.llm.providers.base import LLMProvider
from app.llm.types import CompletionRequest, CompletionResponse, TokenUsage

logger = structlog.get_logger(__name__)

GEMINI_PROVIDER_NAME = "gemini"
GEMINI_DEFAULT_MODEL = "gemini-2.5-flash"

# Major version out of a Gemini model id (after lowercasing); a "models/" prefix or a
# suffix (flash, -preview, -latest) does not defeat it. Used to pick the thinking control:
# 3.x models take thinking_level, 2.x models take an integer thinking_budget.
_GEMINI_MAJOR_VERSION_RE = re.compile(r"gemini-(\d+)")

# Finish reasons that mean the model stopped because content was blocked (vs merely
# incomplete). These map to LLMSafetyBlockedError. Built via getattr so a minor SDK that
# drops or renames one member does not crash this module at import.
_SAFETY_FINISH_REASONS = frozenset(
    reason
    for reason in (
        getattr(types.FinishReason, name, None)
        for name in ("SAFETY", "PROHIBITED_CONTENT", "SPII", "BLOCKLIST", "IMAGE_SAFETY")
    )
    if reason is not None
)
# A clean completion ends on STOP (or the unset sentinel); MAX_TOKENS is a truncation, and
# any other reason is incomplete or blocked output.
_CLEAN_FINISH_REASONS = frozenset({None, types.FinishReason.FINISH_REASON_UNSPECIFIED, types.FinishReason.STOP})


def _uses_thinking_level(model: str) -> bool:
    """3.x+ Gemini models take thinking_level; 2.x models take an integer thinking_budget."""
    match = _GEMINI_MAJOR_VERSION_RE.search(model.lower())
    return bool(match) and int(match.group(1)) >= 3


class GeminiProvider(LLMProvider):
    def __init__(self, api_key: str, timeout: int = 120) -> None:
        self._client = genai.Client(api_key=api_key)
        self._default_timeout = timeout

    async def close(self) -> None:
        # The google-genai async client manages its own transport; nothing to release.
        return None

    def _build_config(self, request: CompletionRequest) -> types.GenerateContentConfig:
        config = types.GenerateContentConfig()
        if request.system_prompt:
            config.system_instruction = request.system_prompt
        if request.temperature is not None:
            config.temperature = request.temperature
        if request.max_tokens is not None:
            config.max_output_tokens = request.max_tokens
        if request.response_json_schema is not None:
            config.response_mime_type = "application/json"
            config.response_json_schema = request.response_json_schema

        # Thinking control is model-major-version specific.
        if _uses_thinking_level(request.model):
            if request.thinking_level is not None:
                config.thinking_config = types.ThinkingConfig(thinking_level=request.thinking_level.value)
        elif request.thinking_budget is not None:
            config.thinking_config = types.ThinkingConfig(thinking_budget=request.thinking_budget)

        timeout_s = request.timeout_seconds or self._default_timeout
        # google-genai http_options.timeout is milliseconds.
        config.http_options = types.HttpOptions(timeout=timeout_s * 1000)
        return config

    async def complete(self, request: CompletionRequest) -> CompletionResponse:
        config = self._build_config(request)
        try:
            response = await self._client.aio.models.generate_content(
                model=request.model,
                contents=request.user_content,
                config=config,
            )
        except ClientError as exc:
            self._raise_for_client_error(exc, request.model)
        except ServerError as exc:
            raise LLMUnavailableError(
                f"Gemini server error: {exc}", provider=GEMINI_PROVIDER_NAME, model=request.model
            ) from exc
        except Exception as exc:  # noqa: BLE001 - transport/timeout etc. are availability-class
            raise LLMUnavailableError(
                f"Gemini call failed: {exc}", provider=GEMINI_PROVIDER_NAME, model=request.model
            ) from exc

        usage = self._usage_from(response)
        self._guard_finish_reason(response, request.model, usage)

        # ``response.text`` raises when there is no text candidate; the finish-reason guard
        # above has already converted the blocked/truncated cases into typed errors.
        text = response.text or ""
        return CompletionResponse(
            content=text,
            model=request.model,
            provider=GEMINI_PROVIDER_NAME,
            usage=usage,
        )

    @staticmethod
    def _usage_from(response: object) -> TokenUsage:
        meta = getattr(response, "usage_metadata", None)
        if meta is None:
            return TokenUsage()
        return TokenUsage(
            prompt_tokens=getattr(meta, "prompt_token_count", 0) or 0,
            completion_tokens=getattr(meta, "candidates_token_count", 0) or 0,
            thoughts_tokens=getattr(meta, "thoughts_token_count", 0) or 0,
            total_tokens=getattr(meta, "total_token_count", 0) or 0,
        )

    @staticmethod
    def _guard_finish_reason(response: object, model: str, usage: TokenUsage) -> None:
        candidates = getattr(response, "candidates", None) or []
        if not candidates:
            # No candidate at all: a prompt-level block, surfaced as a safety stop.
            raise LLMSafetyBlockedError(
                "Gemini returned no candidate (prompt blocked)",
                provider=GEMINI_PROVIDER_NAME,
                usage=usage,
                model=model,
            )
        finish_reason = getattr(candidates[0], "finish_reason", None)
        if finish_reason in _CLEAN_FINISH_REASONS:
            return
        if finish_reason == types.FinishReason.MAX_TOKENS:
            raise LLMOutputTruncatedError(
                "Gemini response truncated at the output-token budget",
                provider=GEMINI_PROVIDER_NAME,
                usage=usage,
                model=model,
            )
        if finish_reason in _SAFETY_FINISH_REASONS:
            raise LLMSafetyBlockedError(
                f"Gemini response blocked ({finish_reason})",
                provider=GEMINI_PROVIDER_NAME,
                usage=usage,
                model=model,
            )
        raise LLMError(
            f"Gemini response did not complete ({finish_reason})",
            provider=GEMINI_PROVIDER_NAME,
            usage=usage,
            model=model,
        )

    @staticmethod
    def _raise_for_client_error(exc: ClientError, model: str) -> None:
        status = getattr(exc, "code", None) or getattr(exc, "status_code", None)
        message = str(exc)
        if status == 429:
            raise LLMRateLimitError(
                f"Gemini rate limit: {message}", provider=GEMINI_PROVIDER_NAME, model=model
            ) from exc
        if status in (401, 403):
            raise LLMAuthenticationError(
                f"Gemini authentication failed: {message}",
                provider=GEMINI_PROVIDER_NAME,
                model=model,
            ) from exc
        if status == 404:
            raise LLMUnavailableError(
                f"Gemini model not found: {message}", provider=GEMINI_PROVIDER_NAME, model=model
            ) from exc
        # 400 and other 4xx are deterministic request faults — non-retryable.
        raise LLMInvalidRequestError(
            f"Gemini rejected the request: {message}", provider=GEMINI_PROVIDER_NAME, model=model
        ) from exc
