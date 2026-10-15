# Contributing

This repository holds several components, each with its own toolchain. There is no CI service:
before you open a pull request, run the checks below for each component you change.

## Go (the recommendations engine and the control plane)

The toolchain floor is Go 1.26.0. Install the vulnerability scanner with
`go install golang.org/x/vuln/cmd/govulncheck@v1.9.0` and put `$(go env GOPATH)/bin` on
your `PATH` (or pass `GOVULNCHECK=/path/to/govulncheck` to Make). From a fresh clone:

```sh
gofmt -l .                              # must print nothing
go build ./...
go vet ./...
go test -race -count=1 ./...            # DAO + integration tests skip without a test database
golangci-lint run ./...                 # golangci-lint v2.11.x, under Go 1.26
govulncheck ./...                       # golang.org/x/vuln/cmd/govulncheck
```

The `harnessctl-server` binary and its image redistribute the Go modules it links. After a
`go.mod` change, or a change to what `cmd/harnessctl-server` imports, run
`scripts/go-module-licenses.sh update`, review the diff under `third_party/licenses/go/`, and
commit it. `make licenses` fails until those files match the modules the binary links.

`make test-integration` runs both the control plane and recommendations suites with the race
detector against a throwaway Postgres container, and removes it on success, failure or interruption.
Docker assigns a free localhost port by default; set `PG_PORT` to request a particular port.
The database must allow scratch database creation. Set `HARNESS_TEST_DATABASE_URL` or
`POSTGRES_TEST_DSN` to use an existing test database instead of Docker: a single configured DSN
is supplied to both suites; when both are set, each component uses its own. An unreachable
configured database fails the target rather than skipping the database tests.

For the database examples below, `POSTGRES_TEST_DSN` must use libpq keyword form with a `dbname=`:

```sh
export POSTGRES_TEST_DSN="host=localhost port=55432 user=postgres password=postgres dbname=postgres sslmode=disable"
make test-integration
```

Run the recommendations examples, with `POSTGRES_TEST_DSN` set so the database examples run too:

```sh
recommendations/scripts/run-examples.sh
```

## tetriz-py (the LLM service)

From `tetriz-py/`, with `uv`:

```sh
uv sync --frozen --python 3.11          # the SBERT 'ml' extra is optional; the theme tests mock it
uv run ruff check .
uv run ruff format --check .
uv run pytest                           # offline: no Gemini key, the provider is mocked
```

## Node workspaces and Protobuf

The `agent` and `web` workspaces use pnpm. `make agent` and `make web-test` install the
workspace and run its `typecheck`, `lint` and `test` scripts; after a dependency change run
`pnpm audit --prod`, and `make licenses`, which fails when the admin UI bundles a package that
`NOTICE` and `third_party/licenses/` do not attribute, or whose licence file lacks the text of a
licence its `package.json` declares. Protobuf is generated with Buf; after
editing anything under `proto/`, run `make proto`, and commit the result under `gen/`. On a
clean checkout, `make proto-check` regenerates in a temporary directory and fails on drift,
without changing checkout files. This uses the pinned remote plugins and needs Buf registry access.

`make check-scripts` exercises the validation scripts’ DSN forwarding, cleanup, failure propagation
and drift detection without external services.

Run `pnpm install --frozen-lockfile` before `make check`; its early lint step needs the Node workspace dependencies.

`make check` runs the non-container checks above, including `govulncheck` (`make vuln`),
isolated Protobuf drift verification, Go build/race tests, Node and Python checks, offline examples,
and license validation. Install the listed tools first. Vulnerability checks require advisory database
access and can fail when new advisories appear. Database suites/examples and container acceptance
remain separate: run `make test-integration`, the examples with `POSTGRES_TEST_DSN` set, and
`make acceptance` for release validation.

## End to end

`scripts/acceptance.sh` runs the compose stack, the built agent and a temporary home directory
through the whole enforcement lifecycle. It needs Docker with Compose v2. Run it after a change to
the server, the agent or the compose stack.

To verify the notices actually distributed in the two images, build them from the same checkout
and inspect their contents (Docker, Go and Python 3.11+ are required):

```sh
docker build -t tetriz-readiness-server:local .
docker build -t tetriz-readiness-python:local tetriz-py
scripts/image-licenses.py --server tetriz-readiness-server:local --python tetriz-readiness-python:local
python3 -B scripts/test-image-licenses.py # negative checks against deliberately incomplete images
```

The verifier checks notice files against the checkout, linked Go versions, installed Python wheel
inventories and license files, bundled native notices, and base-image copyright files. It removes
temporary containers. Run it for each architecture distributed; it checks redistribution evidence,
not complete source provenance or legal compliance.

## Pull requests

- **Every pull request needs a test.** A behaviour change needs a test that fails before the
  change and passes after; new cases go beside the code.
- **Keep commits focused**, with messages in the imperative mood.
- **Add a `CHANGELOG.md` entry for any user-visible change.** A pure refactor or test needs none.
- **Do not silence a lint finding inline.** Fix it, or change the linter config with a comment
  explaining why.
- **New public-API demonstrations go under `recommendations/examples/`** and are linked from
  [`recommendations/examples/README.md`](recommendations/examples/README.md).
  `recommendations/scripts/run-examples.sh` diffs each example's output against its README, so
  keep that output deterministic.

## Licensing of contributions

No sign-off line and no separately signed agreement are needed. By opening a pull request you
agree that your contribution is released under the project's [LICENSE](LICENSE).

## Source releases

Releases use one repository tag, `vMAJOR.MINOR.PATCH`, for the source tree. Before a release, run
`make check`, `make test-integration`, the examples with `POSTGRES_TEST_DSN` set, Compose acceptance,
and the built-image notice checks above. A default test run without a database is not a database pass.

Move the release entries from `Unreleased` into a dated version section in `CHANGELOG.md`, leave an
empty `Unreleased` heading, and update its compare link and the version's release link. Keep the
agent and Python versions consistent with the release when their versions change. Tag the checked
revision and create a GitHub release with the changelog entries and source-build instructions.
There are no registry packages or prebuilt binaries; GitHub provides the source archives.
