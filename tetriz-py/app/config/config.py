# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Configuration via Pydantic settings.

Everything is read from the environment, prefixed ``TETRIZ_`` (e.g. ``TETRIZ_PORT``),
with sensible defaults so the service boots with no configuration at all. The one
exception is the Gemini key, which is accepted as either ``TETRIZ_GEMINI_API_KEY`` or
the plain ``GEMINI_API_KEY`` — bring your own key and the service runs. The key is
NEVER read from a file or baked into the image; it arrives at run time only.
"""

from __future__ import annotations

from enum import StrEnum

from pydantic import AliasChoices, Field
from pydantic_settings import BaseSettings, SettingsConfigDict


class Environment(StrEnum):
    DEVELOPMENT = "development"
    DEPLOY = "deploy"
    PROD = "prod"


class ThemeClassifierStrategy(StrEnum):
    """How the review-theme classifier runs.

    ``gemini`` is the default: it uses the Gemini key the service already needs, so the
    DERIVED pipeline works with no extra dependency. ``sbert`` is the token-free local
    opt-in (an all-MiniLM-L6-v2 sentence encoder) and requires the ``ml`` extra
    (``uv sync --extra ml``). The strategy is resolved per org inside the service —
    callers never pass it, so a third option later is a Python-only change.
    """

    SBERT = "sbert"
    GEMINI = "gemini"


class Settings(BaseSettings):
    """Service settings, overridable from ``TETRIZ_``-prefixed environment variables."""

    model_config = SettingsConfigDict(
        env_prefix="TETRIZ_",
        case_sensitive=False,
        extra="ignore",
    )

    environment: Environment = Environment.PROD

    # Bind to loopback by default: the service is unauthenticated and, once a key is set,
    # holds the operator's Gemini key — it must not be reachable from other hosts.
    host: str = "127.0.0.1"
    port: int = 8000
    workers: int = 1
    log_level: str = "info"

    # Accepted as TETRIZ_GEMINI_API_KEY or the plain GEMINI_API_KEY; empty is valid — the
    # service boots and health checks pass, and LLM calls fail until the key is supplied.
    gemini_api_key: str = Field(
        default="",
        validation_alias=AliasChoices("TETRIZ_GEMINI_API_KEY", "GEMINI_API_KEY"),
    )
    gemini_model: str = "gemini-2.5-flash"
    # Per-call Gemini HTTP timeout, seconds. A task may set a shorter per-task budget.
    request_timeout: int = 120

    # Default review-theme classifier strategy, and optional per-org overrides
    # (TETRIZ_THEME_CLASSIFIER_OVERRIDES='{"org-abc":"gemini"}'). Gemini is the default so
    # DERIVED works with only the Gemini key; set this to sbert for the token-free local
    # classifier, which needs the ml extra.
    theme_classifier_strategy: ThemeClassifierStrategy = ThemeClassifierStrategy.GEMINI
    theme_classifier_overrides: dict[str, ThemeClassifierStrategy] = Field(default_factory=dict)


_settings: Settings | None = None


def get_config() -> Settings:
    """Return the process-wide settings singleton, building it on first use."""
    global _settings
    if _settings is None:
        _settings = Settings()
    return _settings


def set_config(settings: Settings) -> None:
    """Replace the settings singleton (tests use this to inject a fixed config)."""
    global _settings
    _settings = settings


def reset_config() -> None:
    """Clear the singleton so the next get_config() rebuilds from the environment."""
    global _settings
    _settings = None


def get_org_theme_classifier_strategy(org_id: str) -> ThemeClassifierStrategy:
    """Resolve the review-theme classifier strategy for one org: a per-org override if
    present, otherwise the service default."""
    config = get_config()
    return config.theme_classifier_overrides.get(org_id, config.theme_classifier_strategy)
