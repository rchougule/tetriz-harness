# Reference

This page gives the exact rules for each part of Harness Control Plane. The [README](../README.md) has the quick start and the known limits.

- [Server configuration](#server-configuration)
- [Directory file](#directory-file)
- [Enforcement status](#enforcement-status)
- [Install paths](#install-paths)
- [`harness-agent` commands](#harness-agent-commands)
- [API](#api)

## Server configuration

These are the settings of the reference host. A program that uses the library gives the same choices to `server.New` and to the stores. The reference host reads only environment variables. [`.env.example`](../.env.example) lists them with local defaults.

| Variable | Default | Meaning |
|---|---|---|
| `HARNESS_DATABASE_URL` | required | Postgres connection string. |
| `HARNESS_DIRECTORY_FILE` | required | JSON file of people, teams and tokens. See [Directory file](#directory-file). |
| `HARNESS_LISTEN_ADDR` | `:8080` | HTTP listen address. |
| `HARNESS_BLOB_BACKEND` | `fs` | The store for file bodies: `fs` or `s3`. |
| `HARNESS_BLOB_DIR` | `./data/blobs` | Directory for the `fs` backend. The image sets `/data/blobs`. |
| `HARNESS_S3_ENDPOINT` | none | `host[:port]` of an S3-compatible endpoint. Required for `s3`. |
| `HARNESS_S3_BUCKET` | none | Bucket. The server creates it if it does not exist. Required for `s3`. |
| `HARNESS_S3_REGION` | none | Bucket region. |
| `HARNESS_S3_ACCESS_KEY` | none | Static access key. If the keys are empty, the server uses the instance role. |
| `HARNESS_S3_SECRET_KEY` | none | Static secret key. |
| `HARNESS_S3_USE_SSL` | `true` | Use HTTPS to the endpoint. |
| `HARNESS_S3_PREFIX` | none | Prefix for each object key. |
| `HARNESS_ALLOW_HOOKS` | `false` | Accept hook artifacts. A hook runs code on each targeted machine. |

- **Health:** `GET /healthz` returns `ok` when Postgres answers. `harnessctl-server healthcheck` sends this request. Use it in images that have no shell.
- **S3 on your machine:** `docker compose --profile s3 up --build --wait` adds MinIO and a second server on `127.0.0.1:8081`.
- **Version:** set the version with `go build -ldflags "-X main.version=v0.1.0"`. `GetSettings` returns it.
- **Fixed limits:** 100 live definitions, 100 files for each skill and 1 MiB for each file. The default window is 15 days and the maximum window is 90 days. These limits are constants. You cannot configure them.

## Directory file

The reference host reads its people, teams and tokens from one JSON file when it starts. Restart the host to apply a change to the file.

```json
{
  "admin_token": "sha256:<64 lowercase hex digits>",
  "persons": [
    {"id": "dev-1", "display_name": "Dev One", "device_tokens": ["sha256:<64 lowercase hex digits>"]}
  ],
  "teams": [
    {"id": "platform", "name": "Platform", "members": ["dev-1"]}
  ]
}
```

| Field | Meaning |
|---|---|
| `admin_token` | The bearer token for each `AdminService` call and for the admin UI. Required. |
| `persons[].id` | The id of the person, 1 to 200 bytes. Targets and reports keep the id as it is. |
| `persons[].display_name` | The name that the UI shows. The default is the id. |
| `persons[].device_tokens` | Bearer tokens that an agent sends to act as this person. Optional. |
| `teams[].id` | The id of the team, 1 to 200 bytes. |
| `teams[].name` | The name that the UI shows. The default is the id. |
| `teams[].members` | Ids of persons in the file. |

- **Tokens:** a token is the secret itself, or `sha256:` followed by the lowercase hex digest of the secret. Only the digest keeps the secret out of the file.
- **Errors:** the host does not start if the file has one of these problems. The host shows each problem.
  - An unknown field.
  - An empty id, or the same id two times.
  - A device token that two persons use, or a device token that is the same as the admin token.
  - A team member who is not a person in the file.
- **Requests:** an `AdminService` call must have the admin token. An `AgentService` call must have a device token from the file, and acts as the person of that token. The host refuses all other calls as `unauthenticated`. `/healthz` and the files of the UI do not need a token.

This command shows the digest of a secret:

```sh
printf '%s' 'example-secret' | shasum -a 256 | cut -d' ' -f1
```

```text
7fccb1e7c6b606c58525851cc1bfe1bdeed2251a07fefc0e269e1382d3c97406
```

Use a random secret in place of `example-secret`, for example the output of `openssl rand -hex 32`. Give the secret to the agent. Write `sha256:` and the digest in the file. On Linux, `sha256sum` does the same as `shasum -a 256`.

## Enforcement status

Each targeted person gets one status for each placement. The server reads the table from top to bottom and uses the first row that matches. `PENDING` is in the table two times because two different conditions cause it.

| Status | When |
|---|---|
| `EXPIRED` | The enforcement window is closed. Agents do not change the file, and the server does not read reports. |
| `PENDING` | The server cannot find the path for this tool and type. The detail tells you this. |
| `EXCLUDED` | No agent has reported for the person. For a repo-local placement, this status also shows when no agent knows a checkout of the repository. |
| `INSTALLED` | The latest report of the person has the enforced content hash at the install path. |
| `FAILED` | The file is not installed, and the agent reported an apply failure for the enforced hash. The detail gives the reason. |
| `PENDING` | The person is targeted and the agent can reach the path, but the file is not correct yet. |
| `REMOVAL_PENDING` | The artifact is retracted, but the latest report still shows a file at the path. |
| `REMOVED` | The artifact is retracted, and the latest report does not show the file. |

- **Retraction:** after `RemoveArtifact`, the agent deletes each file that still has the bytes it wrote. It deletes these files on the second pass, not the first. This delay prevents one bad or empty response from the server from deleting files on all machines. If a person changed the file, the agent keeps it, and the status is `REMOVAL_PENDING` with the reason.
- **Files removed from a skill:** if an update drops a file from a skill, the agent deletes that file only while it still has the bytes that the agent wrote. If a person changed the file, the agent keeps it and reports it, as for a retraction.
- **Hook updates:** if the event, matcher, command or install path of a hook changes, the agent removes its old entry before it adds the new one. It removes only entries that it added itself; an identical entry that a person wrote stays.
- **Removing a person from targets:** the person is not in the status list after this change. The agent of that person also deletes the files, as for a retraction, on the second pass. Only a retraction gives the status `REMOVED`.
- **Pause:** `PauseEnforcement` stops all writes and deletes by all agents. The server continues to read reports and to calculate status.
- **New database:** if you delete the database, an installed agent gets an empty set. The agent then deletes the files that it wrote, on the second pass. If you create the artifact again, the new artifact has a new id.

## Install paths

The paths come from `pkg/spec`. The Go tests and the TypeScript tests compare them with [`testdata/install-paths.json`](../testdata/install-paths.json). `<slug>` must match `^[a-z0-9][a-z0-9-]*$` and have a maximum of 100 characters. A skill can have a maximum of 100 files with its `SKILL.md`.

The agent reads and writes regular files up to 16 MiB, including co-owned hook settings. Larger or non-regular files fail safely and are left unchanged. Artifact files accepted by the reference server have the narrower 1 MiB server limit.

### Claude Code

| Type | User-global | Repo-local |
|---|---|---|
| Skill | `~/.claude/skills/<slug>/SKILL.md` | `<checkout>/.claude/skills/<slug>/SKILL.md` |
| Agent | `~/.claude/agents/<slug>.md` | `<checkout>/.claude/agents/<slug>.md` |
| Rule | `~/.claude/rules/<slug>.md` | `<checkout>/.claude/rules/<slug>.md` |
| Hook | `~/.claude/hooks/<slug>.sh` | `<checkout>/.claude/hooks/<slug>.sh` |
| CLAUDE.md | `~/.claude/CLAUDE.md` | `<checkout>/CLAUDE.md` |
| AGENTS.md | not supported | `<checkout>/AGENTS.md` |

### Codex

| Type | User-global | Repo-local |
|---|---|---|
| Skill | `~/.agents/skills/<slug>/SKILL.md` | `<checkout>/.agents/skills/<slug>/SKILL.md` |
| Agent | not supported | `<checkout>/.codex/agents/<slug>.md` |
| Rule | not supported | `<checkout>/.codex/rules/<slug>.md` |
| Hook | `~/.codex/hooks/<slug>.sh` | `<checkout>/.codex/hooks/<slug>.sh` |
| CLAUDE.md | `~/.claude/CLAUDE.md` | `<checkout>/CLAUDE.md` |
| AGENTS.md | not supported | `<checkout>/AGENTS.md` |

If `$CODEX_HOME` is set, a user-global Codex hook goes in `$CODEX_HOME`. If it is not set, the hook goes in `~/.codex`.

### Cursor

| Type | User-global | Repo-local |
|---|---|---|
| Skill | `~/.cursor/skills/<slug>/SKILL.md` | `<checkout>/.cursor/skills/<slug>/SKILL.md` |
| Agent | `~/.cursor/agents/<slug>.md` | `<checkout>/.cursor/agents/<slug>.md` |
| Rule | `~/.cursor/rules/<slug>.mdc` | `<checkout>/.cursor/rules/<slug>.mdc` |
| Hook | `~/.cursor/hooks/<slug>.sh` | `<checkout>/.cursor/hooks/<slug>.sh` |
| CLAUDE.md | `~/.claude/CLAUDE.md` | `<checkout>/CLAUDE.md` |
| AGENTS.md | not supported | `<checkout>/AGENTS.md` |

### Kiro

| Type | User-global | Repo-local |
|---|---|---|
| Skill | `~/.kiro/skills/<slug>/SKILL.md` | `<checkout>/.kiro/skills/<slug>/SKILL.md` |
| Agent | `~/.kiro/agents/<slug>.md` | `<checkout>/.kiro/agents/<slug>.md` |
| Rule | `~/.kiro/steering/<slug>.md` | `<checkout>/.kiro/steering/<slug>.md` |
| Hook | not supported | `<checkout>/.kiro/hooks/<slug>.kiro.hook` |
| CLAUDE.md | `~/.claude/CLAUDE.md` | `<checkout>/CLAUDE.md` |
| AGENTS.md | not supported | `<checkout>/AGENTS.md` |

### Hook commands

The server makes the command that a hook entry runs. Thus the entry always runs the placed script. The API has no field for a different shell command.

| Tool | User-global command | Repo-local command |
|---|---|---|
| Claude Code | `"$HOME"/.claude/hooks/<slug>.sh` | `"$CLAUDE_PROJECT_DIR"/.claude/hooks/<slug>.sh` |
| Codex | `"${CODEX_HOME:-$HOME/.codex}"/hooks/<slug>.sh` | `"$(git rev-parse --show-toplevel)"/.codex/hooks/<slug>.sh` |
| Cursor | `"$HOME"/.cursor/hooks/<slug>.sh` | `.cursor/hooks/<slug>.sh` |
| Kiro | not supported | none: the `.kiro.hook` file starts the hook itself |

- **Settings entry:** for Claude Code, the entry goes in `settings.json` at user scope and in `settings.local.json` in a checkout. For Codex and Cursor, the entry goes in `hooks.json`. Each file is next to the `hooks/` folder.
- **Events:** each tool accepts only its own event names. See `hookEvents` in [`pkg/spec/spec.go`](../pkg/spec/spec.go) for the list.

## `harness-agent` commands

| Command | What it does |
|---|---|
| `sync` | Runs one pass. It gets the set, writes and deletes files, and sends a report. |
| `watch` | Runs a pass at each `--interval` until you stop it. |
| `status` | Shows the last stored state. It does not call the server. |
| `install-service` | Installs a launchd service (macOS) or a systemd user service (Linux) that runs `watch`. |
| `uninstall-service` | Stops and removes the service and its launcher. Use the same `--state-dir` that you used for the installation. |

| Flag | Meaning |
|---|---|
| `--server <url>` | Server URL. If not set, the agent uses `HARNESS_AGENT_SERVER`. |
| `--token <token>` | Device token. If not set, the agent uses `HARNESS_AGENT_TOKEN`, then `<state-dir>/token`. |
| `--state-dir <dir>` | State directory. If not set, the agent uses `HARNESS_AGENT_STATE_DIR`, then `$XDG_CONFIG_HOME/harness-agent` or `~/.config/harness-agent`. |
| `--interval <dur>` | Interval for `watch`, for example `30s`, `5m` or `1h`. The default is `5m`. The minimum is one second. |
| `--project <dir>` | Adds a checkout for repo-local artifacts. You can use this flag more than one time. |
| `--no-discover` | Does not look for checkouts in the session history of Claude Code and Codex. |
| `--insecure-http` | Allows an `http://` server URL for a host other than this machine. The device token then crosses the network without encryption. Without this flag, such a URL is a usage error. `install-service` keeps the flag in the service. |
| `--print` | Makes `install-service` show the launcher and the unit. It does not install them. |
| `--json` | Shows the output in a format for programs. |
| `--verbose` | Writes each event to stderr as JSON. |

| Exit code | Meaning |
|---|---|
| `0` | The pass is complete with no apply failures. |
| `1` | One or more files did not apply. |
| `2` | The agent could not reach the server, the pass stopped, or the report failed. |
| `64` | The command line is not correct. |

- **Live sessions:** if a tool wrote to its session store in the last five minutes, the agent does not change or delete files for that tool. It tries again on the next pass.
- **Local edits:** if a person changes a managed file, the agent writes the enforced content again on the next pass.
- **Cursor:** on a machine without `~/.cursor`, the agent does not apply Cursor artifacts.
- **Sync summary:** during the first pass after a retraction, the summary line counts the retracted artifact as `in_sync`. The agent deletes its files on the next pass.

### Installing the command

Build on the target macOS or Linux machine with Go toolchain selection enabled, Node 22+ and pnpm 10. `pnpm -r build` compiles `agent/dist/native/fs-helper` and bundles its Go notices; keep the executable and adjacent notices when copying or packaging the agent.

Do these steps in a clone where `pnpm install --frozen-lockfile && pnpm -r build` is complete. This links the CLI onto your PATH. It uses `npm link`, because `npm` comes with Node and `pnpm link --global` first needs `pnpm setup`:

```sh
cd agent && npm link
harness-agent --help | head -1
```

```text
usage: harness-agent <command> [options]
```

- **Updates:** the link points to `agent/dist`. After each pull, run `pnpm -r build` again.
- **nvm:** the link is for the active Node version only. If you change the version, the command is not available.
- **Removal:** run `npm rm -g @harness-control-plane/agent`.

## API

The two services are in [`proto/harnessctl/v1`](../proto/harnessctl/v1). The handler does not authenticate requests. The host must do this. In the reference host, `AdminService` needs the admin token and `AgentService` needs a device token from the directory file.

| `AdminService` RPC | What it does |
|---|---|
| `UpsertArtifact` | Creates or updates a definition. If a live definition has the same tool, type and slug, the call updates it. |
| `ListArtifacts` | Lists live definitions with their placements and status counts. It can also list retracted definitions. |
| `GetArtifact` | Returns one definition with its bodies. |
| `RemoveArtifact` | Retracts a definition. Agents then delete its files that nobody changed. |
| `SetTargets` | Replaces the audience of one placement. An empty audience removes the placement. |
| `GetEnforcementStatus` | Calculates the status of each targeted person at each placement. |
| `ListEligibleEngineers` | Lists people and shows if enforcement can reach each person. |
| `GetArtifactFiles` | Returns the files of a repo-local definition at their repository paths. Use them to commit the files yourself. |
| `GetInstallPreview` | Calculates the install paths for an artifact. The artifact does not have to exist. |
| `ListPersons`, `ListTeams` | Show the directory of the host, for the target pickers. You cannot change the directory here. The server reads team targets when it needs them, so a new team member gets them. |
| `PauseEnforcement`, `ResumeEnforcement` | Turn the kill switch on and off. |
| `GetSettings` | Returns the pause state, the hook state, the server version and the window limits. |

On `UpsertArtifact`, a SKILL carries its file set in `files`, with exactly one entry named
`SKILL.md`. Every other type is a single file and carries its body in `inline_content`; `files`
on one of those is rejected. A HOOK also requires `hook_config`, which every other type rejects.

| `AgentService` RPC | What it does |
|---|---|
| `GetEnforcedSet` | Returns the artifacts of the caller with paths and hashes. It does not return bodies. |
| `GetArtifactBodies` | Returns a maximum of 100 bodies by content hash. It returns only bodies from the set of the caller. |
| `ReportState` | Replaces the report of the caller: the installed files and the apply failures. |
