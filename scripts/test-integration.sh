#!/usr/bin/env bash
# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT
# Both database-backed components run with an explicit DSN so missing configuration cannot skip them.
set -euo pipefail
cd "$(dirname "$0")/.."
go_cmd="${GO:-go}"
docker_cmd="${DOCKER:-docker}"
container=""
cleanup() {
    if [ -n "$container" ]; then "$docker_cmd" rm -f "$container" >/dev/null; fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [ -n "${HARNESS_TEST_DATABASE_URL:-}${POSTGRES_TEST_DSN:-}" ]; then
    export HARNESS_TEST_DATABASE_URL="${HARNESS_TEST_DATABASE_URL:-$POSTGRES_TEST_DSN}"
    export POSTGRES_TEST_DSN="${POSTGRES_TEST_DSN:-$HARNESS_TEST_DATABASE_URL}"
else
    container="tetriz-test-pg-$$"
    "$docker_cmd" run -d --rm --name "$container" -e POSTGRES_PASSWORD=test \
        -p "127.0.0.1:${PG_PORT:-0}:5432" "${PG_IMAGE:-postgres:17-alpine}" >/dev/null
    ready=false
    for ((attempt=0; attempt<60; attempt++)); do
        if "$docker_cmd" exec "$container" pg_isready -h 127.0.0.1 -U postgres >/dev/null 2>&1; then
            ready=true
            break
        fi
        sleep 1
    done
    if [ "$ready" != true ]; then
        echo "test-integration: Postgres did not become ready within 60 seconds" >&2
        exit 1
    fi
    port="$("$docker_cmd" port "$container" 5432/tcp)"
    port="${port##*:}"
    export HARNESS_TEST_DATABASE_URL="postgres://postgres:test@127.0.0.1:$port/postgres?sslmode=disable"
    export POSTGRES_TEST_DSN="$HARNESS_TEST_DATABASE_URL"
fi
"$go_cmd" test -race -count=1 ./server/... ./recommendations/...
