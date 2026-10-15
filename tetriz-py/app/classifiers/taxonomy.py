# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""The review-theme taxonomy, shared by both classifier strategies.

Kept dependency-free (no sentence-transformers / torch) so the Gemini strategy and the
request models can import the theme set without pulling in the optional ``ml`` stack.
"""

from __future__ import annotations

SBERT_MODEL_NAME = "all-MiniLM-L6-v2"

# The literal label emitted when no theme clears the bar. The Go aggregator drops it (it is
# not one of the 13 themes and produces no artifact).
OTHER_THEME = "other"

# Natural-language descriptions — NOT bare labels — for each theme. Descriptions give the
# embedding model more semantic signal than a bare label, and the Gemini path lists them as a
# closed set.
THEME_DESCRIPTIONS: dict[str, str] = {
    "error-handling": "error handling, try catch, nil checks, error propagation, panic recovery",
    "testing": "test coverage, unit tests, assertions, test frameworks, missing tests",
    "logging": "structured logging, observability, debug output, log levels",
    "concurrency": "locks, race conditions, goroutine safety, shared state, deadlocks, mutex",
    "security": "credentials, authentication, input sanitization, secrets, vulnerabilities",
    "code-reuse": "DRY violations, duplicated logic, extract shared utility, copy paste code",
    "type-safety": "type assertions, interface{}, type casts, generic types, any type",
    "documentation": "comments, godoc, README, explanation, missing docs",
    "dead-code": "unused variables, unreachable branches, commented out code, unused imports",
    "input-validation": "validate input, sanitize data, boundary checks, parameter validation",
    "naming-conventions": "variable names, function names, naming consistency, misleading names",
    "performance": "caching, latency, query optimization, N+1 queries, slow operations",
    "pr-scope": "PR too large, break down, single responsibility, scope creep",
}
