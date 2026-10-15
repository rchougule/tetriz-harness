GO ?= go
BUF ?= buf
PNPM ?= pnpm
DOCKER ?= docker
PG_IMAGE ?= postgres:17-alpine
PG_PORT ?= 0
GOVULNCHECK ?= govulncheck
AGENT := $(PNPM) --filter @harness-control-plane/agent
WEB := $(PNPM) --filter harness-control-plane-web
# Every tracked Go file, so formatting covers the whole module and never a worktree or a dependency.
GO_FILES = $(shell git ls-files '*.go')

.PHONY: check check-scripts proto proto-check vuln test test-integration lint fmt agent acceptance licenses web web-test py examples

proto:
	$(BUF) lint
	$(BUF) generate

test:
	$(GO) build ./...
	$(GO) test -race -count=1 ./...

# Uses either configured test DSN for both components, else a throwaway Postgres container.
test-integration:
	GO="$(GO)" DOCKER="$(DOCKER)" PG_IMAGE="$(PG_IMAGE)" PG_PORT="$(PG_PORT)" scripts/test-integration.sh

# Regenerates in a temporary directory; never writes to the checkout.
proto-check:
	BUF="$(BUF)" scripts/check-proto.sh

vuln:
	$(GOVULNCHECK) ./...

# Every check CONTRIBUTING.md lists that needs no container runtime. `make test-integration` and
# `make acceptance` start containers and run separately.
check: check-scripts lint proto-check vuln test agent web-test py examples licenses

check-scripts:
	python3 scripts/test-validation.py

lint:
	$(BUF) lint
	@unformatted=$$(gofmt -l $(GO_FILES)); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	$(GO) vet ./...
	golangci-lint run ./...
	$(AGENT) lint

fmt:
	gofmt -w $(GO_FILES)

agent:
	$(PNPM) install --frozen-lockfile
	$(PNPM) -r build
	$(AGENT) typecheck
	$(AGENT) lint
	$(AGENT) test

acceptance:
	scripts/acceptance.sh

licenses:
	scripts/check-licenses.sh
	scripts/go-module-licenses.sh check

web:
	$(PNPM) install --frozen-lockfile
	$(PNPM) --filter @harness-control-plane/gen build
	$(WEB) build

py:
	cd tetriz-py && uv sync --frozen --python 3.11 && uv run ruff check . && uv run ruff format --check . && uv run pytest

# The database examples run only when POSTGRES_TEST_DSN is set; see CONTRIBUTING.md.
examples:
	recommendations/scripts/run-examples.sh

web-test:
	$(PNPM) install --frozen-lockfile
	$(PNPM) --filter @harness-control-plane/gen build
	$(WEB) typecheck
	$(WEB) lint
	$(WEB) test
