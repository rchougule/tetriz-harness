# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""LLM error hierarchy.

Provider implementations catch SDK-specific exceptions and re-raise as these; the API
handlers map them to HTTP status codes.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from app.llm.types import TokenUsage


class LLMError(Exception):
    """Base exception for LLM operations."""

    # Overridden per subclass; travels in to_detail() as the wire contract.
    error_code = "LLM_ERROR"

    def __init__(
        self,
        message: str,
        provider: str,
        usage: TokenUsage | None = None,
        model: str | None = None,
    ) -> None:
        self.provider = provider
        # Token usage from a response that was received but rejected (e.g. a MAX_TOKENS
        # truncation or safety block). Those calls are still billed, so the handler can
        # still emit token metrics. None when no response was received (transport error).
        self.usage = usage
        # The model that produced `usage`, set alongside it on billed rejections.
        self.model = model
        super().__init__(message)

    def to_detail(self) -> dict[str, Any]:
        """Structured HTTPException detail preserving billed-call metadata."""
        detail: dict[str, Any] = {
            "error": str(self),
            # A stable identifier for the failure class, so a caller can branch on the KIND
            # of failure without matching on prose.
            "code": type(self).error_code,
            "provider": self.provider,
            "model": self.model,
        }
        if self.usage is not None:
            detail["usage"] = self.usage.model_dump()
        return detail


class LLMRateLimitError(LLMError):
    """Provider rate limit exceeded."""

    error_code = "RATE_LIMIT"


class LLMAuthenticationError(LLMError):
    """Invalid API key or authentication failure."""

    error_code = "AUTHENTICATION"


class LLMInvalidRequestError(LLMError):
    """A deterministic INVALID_ARGUMENT fault in the request or its response config — the
    same request fails identically on a retry, so the handler returns a non-retryable 400
    rather than a 502 the caller would re-drive in a loop."""

    error_code = "INVALID_REQUEST"


class LLMUnavailableError(LLMError):
    """Availability-class failure: transport/connection error, request timeout, HTTP 5xx,
    or model-not-found (404 / NOT_FOUND)."""

    error_code = "UNAVAILABLE"


class LLMSafetyBlockedError(LLMError):
    """The provider's safety filter blocked the response."""

    error_code = "SAFETY_BLOCKED"


class LLMOutputTruncatedError(LLMError):
    """The model filled its output budget before finishing the response. Deterministic —
    the same input truncates on every attempt, so a caller must bound what the response
    needs to contain rather than re-driving it."""

    error_code = "OUTPUT_TRUNCATED"
