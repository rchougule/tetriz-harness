# Agent guide

## What this is

Two independent components in one Go module, `github.com/tetriz-ai/tetriz-harness`:

- **Harness Control Plane** (`server/`, `cmd/harnessctl-server/`, `pkg/`, `web/`, `agent/`): a Go server library and reference host that enforce harness files on developer machines, an admin UI compiled into the host, and `harness-agent`, the TypeScript agent that writes the files and reports back. Guide: `docs/control-plane.md`.
- **Harness Recommendations** (`recommendations/`, `tetriz-py/`): a Go engine that finds problems in a committed harness and recommends new artifacts, and the Python LLM service it calls. `recommendations/CLAUDE.md` is the guide for that subtree.

`proto/` holds the API contracts. `gen/` is generated from them. `examples/` holds runnable programs.

## Commands

Run from the repository root. There is no CI service: run the checks for the area you change before a pull request. `CONTRIBUTING.md` lists them all.

```sh
gofmt -l .                       # must print nothing
go build ./... && go vet ./...
go test -race -count=1 ./...     # Postgres suites skip without HARNESS_TEST_DATABASE_URL / POSTGRES_TEST_DSN
golangci-lint run ./...          # golangci-lint v2.11.x, under Go 1.26
make test-integration            # server + recommendations suites against throwaway Postgres
make agent                       # agent: install, build, typecheck, lint, test
make web-test                    # admin UI: typecheck, lint, test
make proto-check                 # isolated generation and drift check; needs Buf registry access
make vuln                        # govulncheck ./...
make proto                       # buf lint + buf generate; commit the result under gen/
scripts/acceptance.sh            # end to end: compose stack, built agent, temporary HOME
recommendations/scripts/run-examples.sh
(cd tetriz-py && uv sync --frozen && uv run ruff check . && uv run pytest)
```

Containers run on Docker with Compose v2. The Go floor is 1.26.0; Node is 22 or later; pnpm is 10.

## Contract to keep

- **The server library authenticates nothing.** The host authenticates every call and binds each agent call to a person with `server.WithPerson`. Do not add a code path that serves a request the host has not checked.
- **The agent manages only the files it wrote.** It does not adopt files that people wrote themselves.
- **Install paths, proto fields and CLI flags are compatibility surfaces.** `testdata/install-paths.json` pins the install paths; a change there is breaking.

## Making a change

- Follow `CONTRIBUTING.md`: every behaviour change needs a test that fails before it and passes after, and a user-visible change gets a `CHANGELOG.md` entry.
- Do not hand-edit `gen/`. Edit `proto/` and run `make proto`.
- Docs describe what exists now, in the present tense, with one way to do each task. Do not hint at later releases or packages.

## Do not

- Do not run `harness-agent` against your real home directory. Use `HOME="$(mktemp -d)"` and `--no-discover`, as the quick start does; the agent writes into `~/.claude`, `~/.codex`, `~/.cursor` and `~/.kiro`.
- Do not run `harness-agent install-service` outside a temporary `HOME`; it installs a background service for the account.
- Do not commit a real token. The tokens in `cmd/harnessctl-server/directory.local.json` are local-only samples.
