# Pairing with Agent Desk

[Agent Desk](https://github.com/tetriz-ai/agent-desk) can supply observed harness inventory and coding-session evidence for Harness Recommendations. Its packages collect locally; your ingestion adapter maps their output into the engine's input contracts.

There is no bundled Agent Desk importer in this repository. The on-disk `recommendations scan` command reads a checkout, while the full engine reads the DAO interfaces in [`recommendations/dao/dao.go`](../recommendations/dao/dao.go).

## Choose the input you need

| Input | Agent Desk source | Recommendations input |
|---|---|---|
| Observed skills, rules, agents, hooks and instruction files | `harness-discovery.scan()` returns assets with tool, scope, source path, hash and file locations | `IHarnessArtifactDAO` and `IHarnessFileDAO`, backed by `harness_artifacts` and `harness_files` |
| Observed invocations | `local-session-extractor` exports `Conversation.sessionEvents`, including skill/hook calls where the source records them | Your adapter resolves events to artifacts and writes `harness_usage_records`, read by `IHarnessUsageRecordsDAO` |
| Session-level behavior and quality | Exported conversations can be analyzed by [Session Intelligence](https://github.com/tetriz-ai/session-intelligence) | Your adapter maps the analysis into the session-signal tables read by `ISessionInsightDAO` |

The inventory package supports Claude Code, Codex, Cursor and Kiro on macOS and Linux. The session extractor supports six tools on macOS; event coverage varies by tool and source format. Follow each package's own prerequisites and supported-source limits.

## Map inventory identities and bodies

Use the contracts exported by [harness-discovery](https://github.com/tetriz-ai/agent-desk/tree/main/packages/harness-discovery), then normalize them to the [recommendations enums](../proto/recommendations/common/v1/harness_enums.proto) and [inventory models](../recommendations/model/model.go).

| Agent Desk value | Engine representation | Adapter responsibility |
|---|---|---|
| `ideType`, such as `claude-code` | Full enum name, such as `IDE_TYPE_CLAUDE_CODE` | Map supported values explicitly; reject unknown values |
| `type`, such as `skill` | Full enum name, such as `HARNESS_ARTIFACT_TYPE_SKILL` | Preserve the artifact kind rather than guessing from its title |
| `scope`: `user`, `repo-local`, `repo-committed` | `HARNESS_ARTIFACT_SCOPE_USER_GLOBAL`, `..._REPO_LOCAL`, `..._REPO_COMMITTED` | Resolve the repository UUID for repo scopes; user-global has none |
| `sourcePath` and `name` | `source_path` and `name` | Preserve the collector's relative path, including inline-hook identity suffixes |
| Asset `id` and `contentHash` | Artifact UUID and a content-addressed `harness_files` row | Maintain a stable ID mapping; the collector's SHA-256 asset ID is not a database UUID |
| `filePath`, `payload.files`, `payload.inline_body` | Body bytes and, for multi-file assets, a file-reference manifest | Read and hash-verify the actual bodies, or use the inline body; a hash alone cannot supply content |
| The observing installation and project | `organization_id`, `person_id`, `repository_id` | Supply these identities from your host; local paths and login metadata are not canonical organization IDs |

Desktop observations use `HARNESS_ARTIFACT_SOURCE_DESKTOP` and belong to a person, even if the observed file is classified as repo-committed. The canonical, person-less VCS inventory is a separate source: obtain it from a trusted repository checkout or VCS integration. Do not promote an arbitrary developer's observation to the organization's canonical baseline.

The [inventory migration](../recommendations/migrations/0004_harness_inventory.sql) defines the source/person and scope/repository constraints. Store confirmed body content and its hash together, and preserve each skill's primary/helper relationships.

## Resolve activity into usage records

The extractor's default export is a Session Intelligence `Conversation`, not a recommendation database row. Its session events include a kind, operation name, timestamp and call identity when observed; the export does not bind every event to a harness artifact UUID or source path.

For each invocation your adapter can identify:

1. Resolve the organization, person and repository using your host's identity mapping.
2. Match the observed event to the inventory using the tool, scope and source path where available. A name alone can be ambiguous when personal and committed artifacts share it.
3. Persist the session ID, event date, invocation count and artifact identity in `harness_usage_records`. Make retries idempotent so repeated exports do not count an invocation twice.
4. Record each person's repository sync scopes so the engine can choose a primary repository for personal findings.

A resolved usage row can reference `artifact_id`; a pending row still needs a reliable inline identity, including `ide_type`, `source_path` and `user_global`. Do not invent those values when an export lacks enough information. The engine's [usage DAO](../recommendations/dao/harness_usage_records_repo.go) and [personal detectors](../recommendations/personal.go) show how these identities and counts are consumed.

Treat missing events as unknown coverage. An installed skill is not necessarily invoked, and an absent invocation in an incomplete transcript does not establish that it was never used. Rules and instruction files may be loaded implicitly and are not judged by the personal never-used invocation check.

## Run the matching engine path

The personal usage-informed checks run through `HarnessImproveEngine.Run` alongside the repository's canonical harness. The engine also uses session aggregates for behavioral checks; DERIVED recommendations need richer coding signals, such as analyzed sessions, review themes and security incidents. Inventory and raw session events alone do not populate all those inputs.

Use the [full-engine CLI wiring](../recommendations/cmd/recommendations/main.go) as a reference for the public DAO constructors and engines. If your host uses its own storage, implement the same DAO interfaces instead of copying data into the supplied Postgres schema.

`ondisk.Read` and `ondisk.Scan` remain useful for the independent, config-only repository path. They do not accept Agent Desk export files and do not run the personal usage checks.

## Review and distribute the result

Recommendations persist to `prompt_library`. The control plane has separate artifact definitions and targeting APIs, so your host or an administrator chooses a recommendation, prepares its files for the desired IDE and calls `AdminService.UpsertArtifact` and `SetTargets`.

Target explicit people for a fixed test group, or teams for membership-based rollout. Keep outcome measurement in your analysis system; installation status confirms delivery rather than improvement in coding outcomes.

See [the control plane guide](control-plane.md) and [its API contract](../proto/harnessctl/v1/admin.proto).

## Integration status and data handling

The public contracts support this composition through a host adapter. This repository ships neither that adapter nor an end-to-end Agent Desk import example; the complete extraction → ingestion → engine → distribution path needs validation in your host.

Agent Desk performs no automatic upload to this engine. Inventory bodies can contain secrets, and session exports retain installation metadata such as account identifiers. Select and review the inputs you ingest, and keep the adapter's stable identity mapping consistent with any redaction applied to exports.
