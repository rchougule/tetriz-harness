<picture>
  <source media="(prefers-color-scheme: dark)" srcset=".github/assets/banner-dark.svg">
  <img alt="Tetriz Harness: recommend harness changes from your codebase and coding signals, then roll them out to the people you choose" src=".github/assets/banner-light.svg" width="880">
</picture>

Recommend improvements to the harness that guides your AI coding agents, using the repository it works in and your team's coding signals. Review the recommendations, then use the control plane to distribute selected changes to developers, teams or a test group.

A harness is the set of rules, skills, agents, commands, hooks and MCP config that steers a coding agent such as Claude Code, Codex, Cursor or Kiro.

[![Go](https://img.shields.io/badge/go-%E2%89%A5%201.26-00ADD8?logo=go)](go.mod)
[![Node](https://img.shields.io/badge/node-%E2%89%A5%2022-339933?logo=node.js)](agent/package.json)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

<a href="https://www.producthunt.com/products/tetriz-harness?embed=true&amp;utm_source=badge-featured&amp;utm_medium=badge&amp;utm_campaign=badge-tetriz-harness" target="_blank" rel="noopener noreferrer"><img alt="Tetriz Harness - Turn what your team learns into everyone’s agent setup | Product Hunt" width="250" height="54" src="https://api.producthunt.com/widgets/embed-image/v1/featured.svg?post_id=1281396&amp;theme=light&amp;t=1792082193173"></a>

<img alt="The recommendations scan finds six problems in a sample repository's harness and reports which detector families ran" src="recommendations/.github/assets/demo.gif" width="1000">

The [recommendations quick start](recommendations/README.md#quick-start) has the commands and a text version of the scan's output.

## Recommend changes in the context of your codebase

**Harness Recommendations** helps you decide what to change in the instructions and tools your coding agents use.

- **Find problems in the existing harness.** The DETECTED scan checks committed harness files against the repository's file paths and configuration. It reports conflicts, references to missing files, patterns matching no files and over-broad permissions, with evidence and detector coverage.
- **Recommend missing artifacts.** The DERIVED engine uses your organization's session analyses, code-review themes and security signals to propose new rules, agents and hooks. This path needs populated signal tables and the bundled `tetriz-py` service with your model key.
- **Use harness activity as evidence.** The full engine also consumes observed personal harness inventory and invocation records for usage-informed checks. These inputs come from your ingestion layer; the on-disk scan cannot infer them from a checkout.

The engine persists findings and recommendations. You choose which to adopt; publishing a recommendation to the control plane is an explicit integration step.

Start with the [recommendations guide](recommendations/README.md), [runnable examples](recommendations/examples/README.md) or the Go entry points `ondisk.Read`, `ondisk.Scan`, `HarnessImproveEngine` and `Engine`.

## Pair it with Agent Desk

[Agent Desk](https://github.com/tetriz-ai/agent-desk) provides two complementary inputs through its standalone packages:

- **[harness-discovery](https://github.com/tetriz-ai/agent-desk/tree/main/packages/harness-discovery)** inventories the agent files on a machine and in its repositories, including their tool, scope, source path and content hash.
- **[local-session-extractor](https://github.com/tetriz-ai/agent-desk/tree/main/packages/local-session-extractor)** extracts local coding sessions and observed session events. Where the source records them, events identify skill and hook calls; exported conversations can also be analyzed with [Session Intelligence](https://github.com/tetriz-ai/session-intelligence) to produce richer coding signals.

You can use these inputs to populate the inventory, usage and session-signal data that Harness Recommendations reads. **The integration needs an adapter:** this repository does not import Agent Desk's JSON or SQLite store directly, and an inventory entry does not prove that an artifact was invoked.

[Pairing with Agent Desk](docs/agent-desk.md) describes the input contracts, identity mappings and coverage limits.

## Roll out selected changes with the control plane

**Harness Control Plane** distributes the artifacts you approve and reports what each targeted person has installed. It includes a Go server library, a reference host with an admin UI, and the `harness-agent` CLI and Node library.

Target an artifact at named people, teams or the whole organization. Place it in a tool's user configuration or the known checkouts of a repository, update the content, and inspect per-person installation status.

<img alt="An admin targets a skill at one person, the agent installs it in a temporary home, and the control plane reports INSTALLED" src=".github/assets/control-plane.gif" width="1000">

The [control plane quick start](docs/control-plane.md#quick-start) reproduces this task with a temporary home directory and includes its text output.

### Run a test/control rollout

Use audience targeting to deliver a change to a test group while leaving a separate control group untargeted. You select the people or teams and measure the outcomes in your own analysis system.

1. Establish the baseline harness and choose disjoint test and control groups. For fixed cohorts, target explicit person IDs; team membership is resolved dynamically.
2. Publish the selected artifact and target only the test group, using the admin UI or `AdminService.SetTargets`.
3. Check installation status before comparing coding outcomes over the same observation window. The control plane reports delivery state; your session and review analysis supplies the outcome measures.
4. Expand the audience if you adopt the change, or remove its targets to withdraw it. Agents need successful syncs to observe withdrawal; a completely empty desired set must be observed twice before removal. An enforcement deadline freezes files rather than uninstalling them.

This supports a manually managed test/control rollout. It does not randomize cohorts, calculate experiment results or restore an earlier harness version automatically. An untargeted person can still have equivalent files from another source, so verify the baseline rather than assuming an empty control.

See the [control plane guide](docs/control-plane.md), [targeting API](proto/harnessctl/v1/admin.proto) and [Go host example](examples/README.md).

## Getting started

| To | You need | Start at |
|---|---|---|
| Inspect the harness inventory without a database or model | Go 1.26+ | [Read the sample harness](recommendations/examples/read-harness-inventory/README.md) |
| Scan a repository's harness for problems | Go 1.26+, Postgres; Docker can provide a throwaway database | [Recommendations quick start](recommendations/README.md#quick-start) |
| Recommend new artifacts from your team's signals | Populated signal tables, `tetriz-py`, uv and a Gemini API key | [DERIVED recommendations](recommendations/README.md#recommend-net-new-artifacts-derived) |
| Feed local inventory and session evidence into the engine | Agent Desk's packages and your ingestion adapter | [Pairing with Agent Desk](docs/agent-desk.md) |
| Try distribution and audience targeting | git, Go toolchain selection, Node 22+, pnpm 10, Docker with Compose, curl, jq | [Control plane quick start](docs/control-plane.md#quick-start) |
| Mount the control plane in your own Go server | Go 1.26+, Postgres 15+ | [Using it as a Go library](docs/go-library.md) |

The on-disk recommendations scan makes no model call, but persists its findings to Postgres. The recommendations engine and the control plane are independent components; you can use either on its own.

## What is in the repository

| Path | Language | What it is |
|---|---|---|
| `recommendations/` | Go | The recommendations engine, on-disk scanner and CLI |
| `tetriz-py/` | Python | The HTTP service backing the recommendation passes that call an LLM |
| `server/`, `cmd/harnessctl-server/`, `pkg/` | Go | The control plane library and reference host |
| `web/` | TypeScript (React) | The admin UI, compiled into the reference host |
| `agent/` | TypeScript (Node) | The distribution agent, as a CLI and an embeddable library |
| `proto/`, `gen/` | Protobuf | API contracts and generated Go and TypeScript bindings |
| `examples/` | Go | A runnable host that mounts the control plane library |

## Project

### Versioning and releases

Everything ships as source. There are no prebuilt binaries and no published packages. Build each component from a clone with the commands in its guide; Go programs import `github.com/tetriz-ai/tetriz-harness`.

This is [v0.1.0](https://github.com/tetriz-ai/tetriz-harness/releases/tag/v0.1.0), a pre-1.0 source release. [CHANGELOG.md](CHANGELOG.md) records the changes. Any minor version can change the API, the agent library and the CLI flags. Background-service installation supports macOS and Linux; check the component guides for setup and operating limits.

### Validation

Maintainers run the checks in [CONTRIBUTING.md](CONTRIBUTING.md) locally and create source releases manually. There is no hosted CI workflow in this repository. Database suites, examples and container acceptance are explicit release checks.

The repository is private. Clone and release links require repository access; public module documentation is not indexed. From a clone, use `go doc ./recommendations` and `go doc ./server` for the Go APIs.

## What it reads, writes and sends

- **`recommendations scan`** reads the directory you provide and writes findings to your Postgres. It calls no model.
- **`tetriz-py`** sends prompts to Gemini with your API key. It is the only component that calls a model.
- **`harnessctl-server`** keeps artifact definitions in Postgres and file bodies in a directory or an S3 bucket. It calls no other service.
- **`harness-agent`** writes the targeted harness files under `~/.claude`, `~/.codex`, `~/.agents`, `~/.cursor`, `~/.kiro` and known repository checkouts. It adds hook entries to tool settings and lists repo-local files in each checkout's Git exclude file. It keeps its state in `~/.config/harness-agent`.

To discover checkouts, the agent reads working directories recorded in Claude Code and Codex session files; `--no-discover` turns this off. Each pass sends the server known `owner/repo` names and the paths and SHA-256 hashes of managed files, without their contents. The agent talks to the server and, when configured, the S3 body store.

Agent Desk does not upload its inventory or sessions to this engine automatically. Your adapter controls what leaves a machine, and session exports can contain personal metadata; review them before ingestion.

Each component guide has a "Before you rely on it" section with its limits.

## Contributing, security and license

- We accept issues and pull requests. See [CONTRIBUTING.md](CONTRIBUTING.md); [CONTRIBUTORS.md](CONTRIBUTORS.md) lists the people who have contributed.
- Report a security vulnerability privately; see [SECURITY.md](SECURITY.md). Do not report it in a public issue.
- The project uses the [MIT License](LICENSE). Copyright 2026 Workloom Solutions Private Limited.
