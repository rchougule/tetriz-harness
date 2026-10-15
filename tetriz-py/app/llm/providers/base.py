# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Abstract base class for LLM provider adapters."""

from __future__ import annotations

from abc import ABC, abstractmethod

from app.llm.types import CompletionRequest, CompletionResponse


class LLMProvider(ABC):
    """Interface that every LLM provider must implement."""

    @abstractmethod
    async def complete(self, request: CompletionRequest) -> CompletionResponse:
        """Execute a completion call."""
        ...

    @abstractmethod
    async def close(self) -> None:
        """Release resources (HTTP clients, etc.)."""
        ...
