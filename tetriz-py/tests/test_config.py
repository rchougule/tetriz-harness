# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""Config loading: defaults, the TETRIZ_ env prefix, the GEMINI_API_KEY alias, and per-org
classifier strategy resolution."""

from __future__ import annotations

import pytest

from app.config.config import (
    Settings,
    ThemeClassifierStrategy,
    get_org_theme_classifier_strategy,
    reset_config,
    set_config,
)


def test_defaults_bind_to_loopback_and_gemini() -> None:
    settings = Settings(_env_file=None)
    assert settings.host == "127.0.0.1"
    assert settings.port == 8000
    assert settings.theme_classifier_strategy is ThemeClassifierStrategy.GEMINI


def test_plain_gemini_api_key_alias_is_accepted(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("TETRIZ_GEMINI_API_KEY", raising=False)
    monkeypatch.setenv("GEMINI_API_KEY", "plain-key")
    assert Settings(_env_file=None).gemini_api_key == "plain-key"


def test_tetriz_prefixed_key_wins_over_plain(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("GEMINI_API_KEY", "plain-key")
    monkeypatch.setenv("TETRIZ_GEMINI_API_KEY", "prefixed-key")
    assert Settings(_env_file=None).gemini_api_key == "prefixed-key"


def test_tetriz_prefix_drives_server_settings(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("TETRIZ_PORT", "9123")
    monkeypatch.setenv("TETRIZ_HOST", "0.0.0.0")
    settings = Settings(_env_file=None)
    assert settings.port == 9123
    assert settings.host == "0.0.0.0"


def test_per_org_strategy_override() -> None:
    set_config(
        Settings(
            theme_classifier_strategy=ThemeClassifierStrategy.SBERT,
            theme_classifier_overrides={"org-x": ThemeClassifierStrategy.GEMINI},
        )
    )
    try:
        assert get_org_theme_classifier_strategy("org-x") is ThemeClassifierStrategy.GEMINI
        assert get_org_theme_classifier_strategy("org-y") is ThemeClassifierStrategy.SBERT
    finally:
        reset_config()
