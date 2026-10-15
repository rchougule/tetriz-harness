# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

from pydantic import BaseModel


class HealthResponse(BaseModel):
    status: str


class ReadinessResponse(BaseModel):
    status: str
    checks: dict[str, str]
