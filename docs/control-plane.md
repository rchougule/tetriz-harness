# Harness Control Plane

Harness Control Plane is a Go library and a self-hosted server. It puts the coding-agent files that you select on the machine of each developer. It also shows, for each person, if each file is installed. It manages skills, hooks, rules, agents, `CLAUDE.md` and `AGENTS.md` for Claude Code, Codex, Cursor and Kiro.

[![Go](https://img.shields.io/badge/go-%E2%89%A5%201.26-00ADD8?logo=go)](../go.mod)
[![Node](https://img.shields.io/badge/node-%E2%89%A5%2022-339933?logo=node.js)](../agent/package.json)
[![License](https://img.shields.io/badge/license-MIT-blue)](../LICENSE)

Versions are `0.x`. Any minor version can change the API, the agent library and the CLI flags.

<img alt="The control plane quick start: an admin enforces a skill for one person, the agent syncs it into a temporary home directory, and the status reads INSTALLED" src="../.github/assets/control-plane.gif" width="1000">

This is one of the two components in the [Tetriz Harness](../README.md) repository. Its companion, [Harness Recommendations](../recommendations/README.md), reads a committed harness and recommends what to change.

## Contents

- [What it does](#what-it-does) gives each entry point, and the tasks that the project does not do.
- [Quick start](#quick-start) runs the reference host on your machine and installs one skill into a temporary home directory.
- [Choosing an entry point](#choosing-an-entry-point) helps you find the correct entry point.
- [Using it as a Go library](#using-it-as-a-go-library) puts the server in your own program, with your people, teams and authentication.
- [Usage](#usage) gives one short example for each of the other entry points.
- [Troubleshooting](#troubleshooting) tells you how to test without a change to your own setup.
- [Reference](#reference) links to the exact rules.
- [Before you rely on it](#before-you-rely-on-it) gives the known limits.
- [Project](#project) gives versions, tests, UI work, contributions and the license.

## What it does

An admin selects the files for each machine. The server keeps this selection. The agent on each machine writes the files and reports the result.

The server is a library. A host program mounts the library. The host tells the server who the people and teams are, and the host authenticates each request. The server calculates the status from the reports of the agents when you ask for it. The server does not store the status.

```mermaid
flowchart LR
  admin["Admin UI or script"] -- "AdminService" --> host["Host program<br/>authenticates every call"]
  agent["harness-agent<br/>on each machine"] -- "AgentService" --> host
  host -- "people and teams,<br/>the caller's person" --> server["server library<br/>Postgres + blob store"]
  agent -- "writes files,<br/>merges hook settings" --> dirs["~/.claude, ~/.codex, ~/.agents,<br/>~/.cursor, ~/.kiro, checkouts"]
```

- **`server.New(server.Options)`** returns an `http.Handler` for your own Go server. The handler has the admin API, the agent API and the admin UI. You supply the people and teams as a `server.Directory`, and you authenticate each request. The server keeps definitions in Postgres. It keeps file bodies in a directory or in an S3 bucket.
- **`harnessctl-server`** is a complete reference host in one binary. It reads people, teams and bearer tokens from a JSON file. The compose stack runs it with a sample file. Use the sample file only on your own machine, to try the project.
- **Admin UI** lets you write artifacts and target them at people, teams or repositories. You can also see the status and pause all enforcement.
- **`AdminService` over Connect** does all the tasks of the UI, for scripts and CI.
- **`harness-agent sync`** gets the set of the person and writes each file. It adds hook entries to the settings of the tool, then sends a report. `watch` does this again and again. `install-service` runs `watch` as a launchd or systemd user service.
- **`createAgent()`** is the same engine as a Node library. Use it in an app that already runs on each machine.

### What it does not do

- It does not find or adopt files that people wrote themselves. It manages only the files that it wrote. If a different file is already at an install path, the agent leaves it and reports a collision. If the file already holds the enforced content, the status reads `INSTALLED`, but the agent never removes it.
- It confines managed file operations below the configured home, Codex home or checkout using an anchored native filesystem helper. Relative symlinks within a root work; absolute symlinks below a root are refused, including ones pointing back inside it. The configured root and its ancestors are trusted. Git-selected metadata directories are separate trusted roots, including a worktree’s shared Git directory outside the checkout. Privileged mount changes, hard links and hostile Git metadata are outside this boundary.
- It does not run on Windows. The agent runs on macOS and Linux.
- It does not commit files. It writes repo-local files into each checkout, and it adds them to the git exclude file of that checkout.
- It does not keep user accounts, sign people in or keep tenants separate. The host program owns people, teams and authentication.
- It does not enforce plugins, slash commands or prompt templates.

## Quick start

You must have git, Go with automatic toolchain selection (the patched version in `go.mod`), Node 22 or later, pnpm 10, curl, jq, and Docker with Compose v2. You do not need an account or an external service.

The stack runs the reference host on `127.0.0.1:8080`, with passwords and tokens for your machine only. This one port serves the API and the admin UI, so you do not start a separate frontend. Start the server first, because the agent is a client of the server.

```sh
git clone https://github.com/tetriz-ai/tetriz-harness
cd tetriz-harness
pnpm install --frozen-lockfile && pnpm -r build
docker compose up --build --wait
alias harness-agent="node $PWD/agent/dist/cli/main.js"
```

`pnpm -r build` builds the agent CLI and its native filesystem helper for this machine. Go is needed at build time; running the built agent needs Node and the bundled helper, not a Go installation. The image builds the server and admin UI itself.

The `alias` is only for this terminal. To keep the `harness-agent` command, see [Installing the command](reference.md#installing-the-command).

The people, teams and tokens of the stack come from [`cmd/harnessctl-server/directory.local.json`](../cmd/harnessctl-server/directory.local.json). The person `dev-1` is a sample person. The server does not create persons. The host supplies them.

| In the sample file | Value |
|---|---|
| Person | `dev-1`, in team `platform` |
| Admin token | `local-only-admin-token` |
| Device token for `dev-1` | `local-only-device-token` |

Enforce a Claude Code skill for `dev-1`:

```sh
admin() {
  curl -sS -X POST "http://127.0.0.1:8080/harnessctl.v1.AdminService/$1" \
    -H 'Content-Type: application/json' \
    -H 'Authorization: Bearer local-only-admin-token' -d "$2"
}
SKILL=$(admin UpsertArtifact '{"ideType":"IDE_TYPE_CLAUDE_CODE","type":"ARTIFACT_TYPE_SKILL","slug":"review","name":"Review",
  "files":[{"relativePath":"SKILL.md","content":"---\nname: review\ndescription: Review the current diff\n---\nList risky changes.\n"}]}' | jq -r .artifactId)
admin SetTargets "{\"artifactId\":\"$SKILL\",\"scope\":\"ARTIFACT_SCOPE_USER_GLOBAL\",\"audience\":{\"personIds\":[\"dev-1\"]}}"
```

```json
{"installPath":".claude/skills/review/SKILL.md", "enforceUntil":"2026-10-15T17:14:24.604958Z"}
```

Run the agent one time with a temporary home directory. Thus the agent does not change your own home directory:

```sh
DEV_HOME=$(mktemp -d /tmp/dev-home.XXXXXX)
HOME="$DEV_HOME" harness-agent sync --server http://127.0.0.1:8080 --token local-only-device-token --no-discover
find "$DEV_HOME/.claude" -type f
admin GetEnforcementStatus "{\"artifactId\":\"$SKILL\"}" | jq -r '.statuses[].status'
```

```text
sync: 1 artifact(s), 1 target(s); in_sync 1, pending 0, failed 0, ended 0; 0 apply failure(s); reported
/tmp/dev-home.pGq4J7/.claude/skills/review/SKILL.md
ENFORCEMENT_STATUS_INSTALLED
```

Before the sync, the same query shows `ENFORCEMENT_STATUS_EXCLUDED`. This is because no agent has reported for that person yet.

To remove what the agent installed, retract the artifact while the server runs. Then run two passes: the first pass only notes the change, and the second pass deletes the files. Then stop the stack and delete its data:

```sh
admin RemoveArtifact "{\"artifactId\":\"$SKILL\"}"
HOME="$DEV_HOME" harness-agent sync --server http://127.0.0.1:8080 --token local-only-device-token --no-discover
HOME="$DEV_HOME" harness-agent sync --server http://127.0.0.1:8080 --token local-only-device-token --no-discover
find "$DEV_HOME/.claude" -type f
docker compose down -v
```

The `find` command prints nothing. If you stop the server first, the files stay: an agent that cannot reach the server changes nothing. See [Enforcement status](reference.md#enforcement-status).

## Choosing an entry point

| You have | Use | Example |
|---|---|---|
| A Go server with its own people and sign-in | [Go library](#using-it-as-a-go-library) | `server.New(server.Options{...})` |
| A browser and the admin token | [Admin UI](#admin-ui) | open `http://127.0.0.1:8080` |
| A script or CI job | [Admin API over Connect](#admin-api-over-connect) | `curl .../AdminService/GetSettings` |
| A developer machine | [`harness-agent` CLI](#harness-agent-cli) | `harness-agent install-service` |
| A Node app that already runs on each machine | [Agent library](#agent-library) | `createAgent({ transport, store })` |
| A desktop app, such as an Electron app, on each machine | [Agent in a desktop app](#agent-in-a-desktop-app) | `agent.sync()` on an interval in the main process |
| Postgres and Go, but no container runtime | [Reference host without Docker](#reference-host-without-docker) | `go build ./cmd/harnessctl-server` |

All entry points use the same handler. A host is in front of the handler and authenticates each call. The host decides what an admin credential is. The host also maps the credential of each agent to one person. In the reference host, both credentials are bearer tokens from the directory file.

## Using it as a Go library

**When:** you already have people, teams and sign-in, and you want the control plane in your own Go program.

You supply three items to `server.New`:

```go
h, err := server.New(server.Options{Store: st, Directory: dir, Blobs: blobs})
```

- **`Store`:** the Postgres store from `server/store`. `store.Open` applies the migrations.
- **`Directory`:** your people and teams, as a `server.Directory`.
- **`Blobs`:** the body store from `server/blob`, in a directory or in S3.

Your middleware must authenticate each request. For each agent call, it binds the request to a person with `server.WithPerson`. The handler does not authenticate requests. It trusts each request that it gets.

[Using it as a Go library](go-library.md) has a complete example host, the commands to try it, and the rules for ids and the directory.

## Usage

### Admin UI

**When:** you want to write artifacts and see the status yourself.

```sh
docker compose up --build --wait
open http://127.0.0.1:8080   # xdg-open on Linux
```

Sign in with the admin token. In the compose stack, the admin token is `local-only-admin-token`.

| Screen | What you do there |
|---|---|
| Artifacts | Write, target and retract artifacts. See the status of each person. |
| Settings | Pause enforcement. See if hooks are on, and see the window limits. |

Notes:

- The browser tab keeps the token in its session storage. The UI sends the token as a bearer header.
- People and teams come from the directory of the host. The UI shows them in the target picker. You cannot change them in the UI.

### Admin API over Connect

**When:** a script, a CI job or a different service manages artifacts.

Each RPC is an HTTP `POST` to `/harnessctl.v1.AdminService/<Method>` with a JSON body. It uses the [Connect protocol](https://connectrpc.com/docs/protocol). Field names are the proto names in lower camel case. Enums are their full value names.

```sh
curl -sS -X POST http://127.0.0.1:8080/harnessctl.v1.AdminService/GetSettings \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer local-only-admin-token' -d '{}'
```

```json
{"serverVersion":"(devel)", "enforcementWindow":"1296000s", "maxEnforcementWindow":"7776000s"}
```

Notes:

- Errors are Connect JSON, for example `{"code":"unauthenticated","message":"missing or invalid bearer token"}`.
- Each `UpsertArtifact` and each `SetTargets` starts the 15-day enforcement window again. If nobody saves an artifact again, its status is `EXPIRED` after the window.

The contract is [`proto/harnessctl/v1/admin.proto`](../proto/harnessctl/v1/admin.proto). The generated Go clients are in `gen/go/harnessctl/v1/harnessctlv1connect`. [API](reference.md#api) lists each RPC.

### `harness-agent` CLI

**When:** a developer machine must keep its files the same as the server.

```sh
HOME="$DEV_HOME" harness-agent install-service --server http://127.0.0.1:8080 --token local-only-device-token --print
```

```text
# /tmp/dev-home.pGq4J7/.config/harness-agent/bin/harness-control-plane-agent
#!/bin/sh
exec '/bin/zsh' -lc 'exec node "$0" "$@"' '/path/to/tetriz-harness/agent/dist/cli/main.js' "$@"

# /tmp/dev-home.pGq4J7/Library/LaunchAgents/harness-control-plane-agent.plist
<?xml version="1.0" encoding="UTF-8"?>
...
  <key>Label</key>
  <string>harness-control-plane-agent</string>
  <key>Program</key>
  <string>/tmp/dev-home.pGq4J7/.config/harness-agent/bin/harness-control-plane-agent</string>
  <key>ProgramArguments</key>
  <array>
    <string>/tmp/dev-home.pGq4J7/.config/harness-agent/bin/harness-control-plane-agent</string>
    <string>watch</string>
    <string>--server</string>
    <string>http://127.0.0.1:8080</string>
    <string>--interval</string>
    <string>5m</string>
...
```

Remove the `HOME` override and `--print` to install the service for your own account. The command keeps the token in `<state-dir>/token`, and only the owner can read that file. Thus the unit file does not contain a secret. On Linux, the command writes a systemd user unit, `harness-control-plane-agent.service`.

Notes:

- The service starts `node` through your login shell. Thus `node` must be on the PATH that your login profile sets.
- The service runs the launcher `<state-dir>/bin/harness-control-plane-agent`. macOS Login Items shows the service with that name. It also shows "Item from unidentified developer", because the launcher does not have a Developer ID signature.
- If you run `install-service` again, it replaces a service that has the old name `harness-agent`.
- Repo-local artifacts go only to checkouts that the agent knows. The agent finds checkouts in the session history of Claude Code and Codex. To add a checkout yourself, use `--project <dir>`.
- The agent sends its device token with each call. Serve the control plane over HTTPS when agents reach it over a network, for example behind a reverse proxy that terminates TLS. The agent refuses `http://` to any host other than this machine, unless you add `--insecure-http`.

[`harness-agent` commands](reference.md#harness-agent-commands) lists each command, flag and exit code.

### Agent library

**When:** an app that already runs on each machine must apply the set itself. An example is a desktop app.

Build it from a clone, as in the quick start. Then link it into your app with `pnpm add link:../tetriz-harness/agent`.

```ts
import { ConnectTransport, JsonFileStore, createAgent } from "@harness-control-plane/agent";

const agent = createAgent({
  transport: new ConnectTransport({ server: "http://127.0.0.1:8080", token: process.env.HARNESS_AGENT_TOKEN ?? "" }),
  store: new JsonFileStore(),
  roots: { discoverProjects: false },
});
agent.on("updated", (state) => {
  for (const item of state.items) console.log(item.status, item.installPath);
});
const result = await agent.sync();
console.log(`fetched=${result.fetched} failures=${result.applyFailures.length}`);
```

```text
in_sync .claude/skills/review/SKILL.md
fetched=true failures=0
```

Notes:

- If the agent cannot reach the server, it does not throw an error. Examine `fetched`, `passError`, `reportError` and `applyFailures` in the result.
- `JsonFileStore` locks its directory. Thus the CLI and your app can use the same state directory. `Transport` and `Store` are interfaces. You can replace them.

### Agent in a desktop app

**When:** your company has a desktop app on each machine, and the app must apply the set. Then you do not have to install `harness-agent` as a service.

The library is for Node. Use it in Electron or in a different app that runs Node 22 or later. Use it only in the main process, because it reads and writes files and runs `git`. Do not use it in a renderer.

1. Build the agent from a clone, as in the quick start.
2. Add the package to your app:

   ```sh
   pnpm add link:../tetriz-harness/agent
   ```

3. Include `agent/dist/native/fs-helper` and its adjacent `licenses/` directory as executable files outside an Electron ASAR archive. Build the agent on each target platform and architecture. Include the package and its dependencies: `@harness-control-plane/gen`, `@bufbuild/protobuf`, `@connectrpc/connect` and `@connectrpc/connect-node`.
4. Start the agent in the main process. Run a pass when the app starts, and then at an interval:

```ts
import { app, ipcMain, type BrowserWindow } from "electron";
import { ConnectTransport, JsonFileStore, createAgent } from "@harness-control-plane/agent";

const SYNC_INTERVAL_MS = 5 * 60 * 1000; // the same default as `harness-agent watch`

export function startHarnessAgent(server: string, token: string, win: BrowserWindow) {
  const agent = createAgent({
    transport: new ConnectTransport({ server, token }),
    store: new JsonFileStore(), // ~/.config/harness-agent, the same directory as the CLI
  });

  // Send each new state to the UI, and let the UI ask for the state or a pass now.
  agent.on("updated", (state) => win.webContents.send("harness:state", state));
  ipcMain.handle("harness:status", () => agent.status());
  ipcMain.handle("harness:sync", () => agent.sync());

  const run = () => agent.sync().catch((err) => console.error("harness sync failed", err));
  void run();
  const timer = setInterval(run, SYNC_INTERVAL_MS);
  app.on("before-quit", () => clearInterval(timer));
}
```

Notes:

- **ES module:** the package is an ES module only. Use `import`. From a CommonJS main process, use `await import("@harness-control-plane/agent")`.
- **Token:** your app must get the device token for the person who is signed in. The host decides how to issue the token. Keep the token in secure storage, for example the Electron `safeStorage` API. Do not put the token in the renderer.
- **Errors:** if the agent cannot reach the server, `sync()` does not throw an error. Examine `fetched`, `passError`, `reportError` and `applyFailures` in the result. `sync()` can throw an error for a disk or store problem. Catch it, as in the example, so that the next pass can run.
- **One scheduler:** do not also install `harness-agent install-service` for the same user. If you use the CLI for a manual `sync`, the lock in `JsonFileStore` keeps the two processes from writing at the same time.
- **Passes at the same time:** if you call `sync()` while a pass runs, you get the result of the pass that runs. The agent does not start a second pass.
- **`git` on the PATH:** the agent runs `git` for repo-local artifacts. A macOS app that starts from Finder or the Dock gets a short PATH. Make sure that `git` is on that PATH.
- **Checkouts:** the agent finds checkouts in the session history of Claude Code and Codex. To add checkouts, set `roots: { projects: [...] }`. To stop the search, set `roots: { discoverProjects: false }`.
- **Live sessions:** if a tool has a session that is active, the agent waits to change the files of that tool. It tries again on the next pass. Thus a change can take more than one interval.
- **Logs:** to get the events of the agent, give a `logger` with `debug`, `info`, `warn` and `error` functions.

### Reference host without Docker

**When:** you run Postgres yourself, and you want the reference host as one static binary.

You must have Go 1.26 or later and Postgres 15 or later. The host applies the migrations when it starts. Use the sample directory file only on your own machine. For all other uses, write [your own file](reference.md#directory-file).

```sh
make web
go build -o bin/harnessctl-server ./cmd/harnessctl-server
HARNESS_DATABASE_URL='postgres://harness:local-only-password@127.0.0.1:5432/harness?sslmode=disable' \
HARNESS_DIRECTORY_FILE=cmd/harnessctl-server/directory.local.json \
  ./bin/harnessctl-server
```

```json
{"time":"2026-10-01T12:13:06.63647+05:30","level":"INFO","msg":"listening","addr":":8080","blob_backend":"fs","allow_hooks":false,"web_ui":true,"persons":1,"teams":1}
```

Notes:

- Run `make web` before `go build`. The binary includes `web/dist` when you build it. The `web_ui` field in the log line shows if the UI is included. `persons` and `teams` count the entries in the directory file.
- The host keeps file bodies in `./data/blobs`, relative to the working directory. To change this, set `HARNESS_BLOB_DIR`.
- The Postgres of the compose stack is not available outside Docker. Use your own Postgres.

## Troubleshooting

### Try it safely

While you test, use the `HOME` override on each agent command. Run `watch` in a terminal. Do not install the service.

```sh
HOME="$DEV_HOME" harness-agent watch --interval 30s --server http://127.0.0.1:8080 --token local-only-device-token --no-discover
```

```text
sync: 1 artifact(s), 1 target(s); in_sync 1, pending 0, failed 0, ended 0; 0 apply failure(s); reported
```

If you run `install-service` without the `HOME` override, it installs a background service for your own account. The service writes to your real `~/.claude`, `~/.codex` and `~/.cursor`. Thus an enforced skill loads in each Claude Code session.

- To delete the files, retract the artifact in the UI.
- To stop the service, run `harness-agent uninstall-service`. The files stay on disk.

### Symptoms and fixes

[Troubleshooting](troubleshooting.md) gives each known symptom and its fix.

## Reference

[Reference](reference.md) gives the exact rules:

- [Server configuration](reference.md#server-configuration): environment variables, health check and fixed limits.
- [Directory file](reference.md#directory-file): people, teams and tokens for the reference host.
- [Enforcement status](reference.md#enforcement-status): each status, retraction, pause and a new database.
- [Install paths](reference.md#install-paths): the path of each artifact type for each tool, and hook commands.
- [`harness-agent` commands](reference.md#harness-agent-commands): commands, flags, exit codes and how to install the command.
- [API](reference.md#api): each RPC of `AdminService` and `AgentService`.

## Before you rely on it

Read these limits before you use the project on machines with an important setup. In each group, the harmful limits are first.

### Authentication

- **Trusts each request (harmful).** The library does not authenticate requests. If a host sends a request without a check, the sender gets full admin rights. Or the sender can act as the person that the host bound.
- **Static tokens in the reference host.** The tokens are fixed values in the directory file. They do not expire. The host does not record when a token was last used. To change or cancel a token, edit the file and restart the host.
- **Plain secrets in the file.** If a token is the secret itself, each person who can read the file can read the secret. Use the `sha256:` form to prevent this.

### Hooks and code

- **Runs code on each targeted machine (harmful).** A hook is a script. The agent puts the script on the machine and registers it in the settings of the tool. Each caller that the host lets call `AdminService` can send a hook to all developers. The server refuses hooks unless `HARNESS_ALLOW_HOOKS=true`.
- **Removes hooks when hooks are off.** If you restart the server with hooks off, the server removes hooks from all sets. Agents then delete installed hooks and their settings entries. The status is `PENDING`.

### Status

- **Can show `INSTALLED` for a machine that does not have the file (harmful).** The latest report for a person replaces the previous report, from any device. If a person has two machines, the status shows the machine that reported last. For a repo-local placement, one correct checkout is sufficient.
- **Trusts reports.** The status is what the agent says it found. Each caller that the host binds to a person can report anything for that person.
- **Can show a status that is worse than the real state.** Cursor artifacts stay `PENDING` on machines without `~/.cursor`. Repo-local placements stay `EXCLUDED` until the agent knows a checkout. The agent finds checkouts only in the history of Claude Code and Codex.
- **Ends without a warning.** Enforcement ends 15 days after the last save of a definition or its targets, unless a later `enforce_until` is set. The files stay, and the status is `EXPIRED`.

### Files on disk

- **Overwrites (harmful).** If git already tracks a repo-local path, and its working copy has no uncommitted changes, the agent writes the file in place. Git keeps the old content, but the change shows in `git status`, and a person can commit it. If the working copy has uncommitted changes, the agent reports a collision and does not write. If a person changes a file that the agent wrote, the agent changes it back on the next pass.
- **Forgets its files if you delete its state.** The agent knows its files from its state directory. If you delete that directory, the files that it wrote earlier read as other people's files: the agent reports a collision for each one. Delete those files to let the agent install them again.
- **Deletes files when you remove a person from the targets.** The agent of that person deletes the unchanged files on the second pass, as for a retraction. The same occurs for all agents if you delete the database.
- **Leaves files on disk.** These files stay: expired artifacts, files written during a pause, and retracted files that a person changed after installation. A retraction takes two passes.

### Operations

- **Single tenant:** one database has one set of artifacts. The server does not record who changed what. The reference host has one admin token and no accounts for each admin.
- **Directory changes:** if the host does not list a person, that person is not in any status. The stored report and targets of the person stay. If the host lists the same id again, the person comes back.
- **Storage:** the server does not delete old bodies from the blob store after an edit. The server lists retracted definitions without pages.
- **Limits:** the server checks the limit of 100 definitions without a lock. Thus two creates at the same time can go a small amount over the limit.
- **Agent state lock:** if an agent stops with a lock held, the next agent tries to take the lock, but this is not guaranteed. Two agents that wait can both take the lock at the same time.

## Project

### Versioning and compatibility

- **Semver:** [v0.1.0](https://github.com/tetriz-ai/tetriz-harness/releases/tag/v0.1.0) is a pre-1.0 source release. [CHANGELOG.md](../CHANGELOG.md) records the changes. Any minor version can break the API, the agent library or the CLI.
- **What counts as breaking:** removing or renumbering a proto field or RPC, changing an install path, or changing a CLI flag or exit code.
- **Minimum versions:** Go 1.26, Node 22 and Postgres 15. Agent source builds select the patched Go toolchain in `go.mod`; its bundled helper runs on macOS and Linux.

### Tests

| Suite | Command |
|---|---|
| Go unit tests. Also Postgres integration tests when `HARNESS_TEST_DATABASE_URL` is set | `go test -race -count=1 ./...` |
| Go integration tests with a temporary Postgres container | `make test-integration` |
| Agent: typecheck, lint and tests | `make agent` |
| Admin UI: typecheck, lint and tests | `make web-test` |
| End to end: compose stack, built CLI, temporary home and git checkout | `scripts/acceptance.sh` |
| Proto lint and generation | `make proto` |

- **Acceptance script:** it tests installation, an update, a forced write failure, retraction, expiry and pause.
- **Proto:** `make proto` needs the Buf CLI. Commit the regenerated `gen/` with the proto change.

### Working on the UI

The compose stack already serves the built UI. Run the Vite dev server only to work on the UI:

```sh
pnpm --filter harness-control-plane-web dev
```

The dev server serves `http://localhost:5173`. It sends `/harnessctl.v1.` calls to `http://localhost:8080`, or to `HCP_SERVER` if it is set. To see sample data without a server, open `http://localhost:5173/?mock=1`.

### Contributing, security and license

- We accept issues and pull requests. [CONTRIBUTORS.md](../CONTRIBUTORS.md) lists the people who have contributed.
- Report a security vulnerability privately; see [SECURITY.md](../SECURITY.md). Do not report it in a public issue.
- The project uses the [MIT License](../LICENSE). Copyright 2026 Workloom Solutions Private Limited.
