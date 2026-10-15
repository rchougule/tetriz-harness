# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""structlog configuration: JSON in deploy/prod, human-readable console in development."""

from __future__ import annotations

import logging
import sys

import structlog

from app.config.config import Environment, get_config


def setup_logging() -> None:
    config = get_config()
    level = getattr(logging, config.log_level.upper(), logging.INFO)

    logging.basicConfig(format="%(message)s", stream=sys.stdout, level=level)

    processors: list[structlog.types.Processor] = [
        structlog.contextvars.merge_contextvars,
        structlog.processors.add_log_level,
        structlog.processors.TimeStamper(fmt="iso"),
        structlog.processors.StackInfoRenderer(),
        structlog.processors.format_exc_info,
    ]
    if config.environment == Environment.DEVELOPMENT:
        processors.append(structlog.dev.ConsoleRenderer())
    else:
        processors.append(structlog.processors.JSONRenderer())

    structlog.configure(
        processors=processors,
        wrapper_class=structlog.make_filtering_bound_logger(level),
        logger_factory=structlog.PrintLoggerFactory(),
        cache_logger_on_first_use=True,
    )
