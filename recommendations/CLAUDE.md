# Agent guide

## What this is

A Go engine that recommends changes to an AI-coding harness. The DETECTED path scans a committed
harness (rules, skills, agents, commands, settings, MCP config) and persists evidence-backed
findings; the DERIVED path recommends net-new artifacts from an organization's own coding signals.
It is single-tenant and persists to Postgres. The Go module is `github.com/tetriz-ai/tetriz-harness`;
this engine is the `recommendations/...` subtree of it.

The zero-setup entry point is `ondisk.Scan` (and `recommendations scan`): point it at a repository
on disk and it seeds a local Postgres, runs the config-only detectors, and returns the findings. The
DERIVED path and the generative DETECTED legs call an LLM through `tetriz-py`; with no `tetriz-py`
URL they degrade to the config-only detectors.

## Layout

- `*.go` (root package `recommendations`) — the two engines (`Engine`/`Run` for DERIVED,
  `HarnessImproveEngine`/`Run`/`RunOrgScan` for DETECTED), the detectors, the aggregator, the quality
  gate, the result models, and the `LLMClient`/`NewNoopLLMClient` seam. One package by design.
- `ondisk/` — the zero-setup on-disk entry point: `Read` (pure disk walk → `Inventory`, no DB) and
  `Scan` (seed + run + project findings, needs Postgres). Tests beside it.
- `dao/` — the Postgres repositories and the lean DAO interfaces the engines consume.
- `model/` — the table models and the aggregate scan structs.
- `migrations/` — the idempotent schema (every statement guarded with `IF NOT EXISTS`); `RunMigrations` (root `migrate.go`) applies the embedded files, so re-running a scan on the same database is a no-op.
- `cmd/recommendations/` — the CLI: the default org/repos run and the `scan` subcommand.
- `internal/` — vendored support (logger, config, the `pyclient`/`llmanalytics` LLM transport, the
  settings parser, slug helpers, test support). Not an import surface; may change without notice.
- `examples/` — runnable examples; `scripts/run-examples.sh` runs them and diffs each against its README.
- `observability.go` — `RegisterMetrics` and `SetLogger`, the package's only global opt-ins.

## Commands

Toolchain floor is Go 1.26.0. Run from the repository root.

```sh
gofmt -l recommendations/                        # must print nothing
go build ./recommendations/...
go vet ./recommendations/...
go test -race -count=1 ./recommendations/...     # DAO + on-disk tests skip without POSTGRES_TEST_DSN
recommendations/scripts/run-examples.sh          # runs every example, compares output to its README
```

Build the CLI with `go build -o <path> ./recommendations/cmd/recommendations` (or `go run ./recommendations/cmd/recommendations ...`); a bare `go build` there fails because the default binary name collides with the `recommendations/` directory.

The DAO and on-disk tests need Postgres; set `POSTGRES_TEST_DSN` as [CONTRIBUTING.md](../CONTRIBUTING.md)
shows. Importing the package has no side effect: it logs through a no-op until `SetLogger`, and registers
no Prometheus metric until `RegisterMetrics`.

## Contract to keep

- **The terminal run stamp and the `is_latest` reconcile commit in one transaction, never half-applied.** A superseded run writes nothing. Pinned by `guarantee_test.go` (`TestEngineRun_TerminalWriteIsSingleTransaction`, `TestEngineRun_SupersededAbortsBeforePersist`).
- **One run per partition is in flight.** A second run on the same `(org, task_type, dedup_key)` is skipped, not duplicated. Pinned by `guarantee_test.go` (`TestEngineRun_SingleInFlightGuardBlocksNonRunningRow`).
- **DETECTED and DERIVED findings coexist as `is_latest`; a reconcile demotes only its own provenance.** Pinned by `dao/prompt_library_repo_test.go` (`TestPromptLibraryRepo_DetectedDerivedIsolation`).
- **An on-disk scan reports detector coverage, never a silent zero.** The families it cannot run on a bare repo walk are named in `Result.Coverage`. Pinned by `ondisk/scan_test.go` (`TestScan_PersistsDetectedFindingsFromDisk`).
- **The security taxonomy stays in lockstep with the tetriz-py derive prompt.** Pinned by `slug_contract_test.go` (`TestSecuritySlugConstsMatchPythonTaxonomy`).
- **Single-tenant: every record for the organization is processed.** Pinned by the DAO tests, which read every matching record for the org.

## Making a change

- Follow [CONTRIBUTING.md](../CONTRIBUTING.md): every behaviour change needs a test (failing before, passing after), user-visible changes get a `CHANGELOG.md` entry, and lint findings are fixed, not silenced.
- New DAO or engine cases go beside the code; new public-API demonstrations go under `examples/` and are linked from `examples/README.md` as an index row. Copy an existing `examples/*/main.go` for the API shape.
- Detectors live in named files in the root `recommendations` package: the conflict sub-detectors in `conflict.go`, instruction-intent in `instruction_intent.go`, security in `security.go`. Most detector files have a matching `<name>_test.go`; some behaviors are covered by the broader engine suites (e.g. `harness_improve_behavioral_test.go`). A new sub-detector goes in the relevant detector file with a test case beside the code.
- `scripts/run-examples.sh` runs the examples and README blocks. It extracts each `examples/<name>/README.md`'s first ` ```sh ` block, runs it from the repository root, and diffs stdout against the last ` ```text ` block. Keep example output deterministic.

## Do not

- Do not add access scoping that narrows a read below the whole organization; this engine is single-tenant by design.
- Do not hand-edit the `gen/` stubs; regenerate from `proto/` with `buf generate`.
- Do not lower the Go floor below 1.26.0.
- The `API_KEY`/`SECRET` strings in the detector test fixtures are sample content, not real secrets.
- Do not call a live LLM or read a real credential store in tests; the suite uses the no-op client.
