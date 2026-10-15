# Troubleshooting

Find the symptom in the table, then do the fix. The [Control Plane guide](control-plane.md#try-it-safely) tells you how to test without a change to your own setup.

## Symptoms and fixes

| Symptom | Fix |
|---|---|
| `compose up` fails with `unrecognized arguments: --wait`, or `docker: 'compose' is not a docker command` | The stack needs Docker Compose v2. Install it, and check that `docker compose version` reports v2. |
| Docker shows `Cannot connect to the Docker daemon` | Start Docker. |
| `install path holds a file this agent did not write` | A file that the agent did not write is at that path. The agent never replaces it. Move the file away, and the next pass installs the artifact. |
| `install_path escapes root (through ".." or a symlink)` | A descendant path escapes the configured root, or uses an absolute symlink. Use a relative link that stays inside the root, or replace the link with a directory. Absolute descendant symlinks are refused even when their target is inside the root. |
| `refusing to send the device token over plain HTTP` | The server URL uses `http://` for a host other than this machine. Use `https://`. On a network that you trust, add `--insecure-http`. |
| `compose up` fails with `port is already allocated` | A different program uses port 8080. Run `HARNESS_PORT=18080 docker compose up --build --wait`. Then use port 18080 in each URL. |
| The server stops with `parsing directory file` or `directory file <path>:` and a reason | The file does not obey a rule in [Directory file](reference.md#directory-file). Correct each field that the message names. Then start the server again. |
| The sign-in screen shows `That token was rejected.`, or the API returns `unauthenticated` | Use the admin token from the directory file. In the compose stack, this is `local-only-admin-token`. A device token does not work here. |
| `sync: enforced set unavailable ([unavailable] connect ECONNREFUSED ...)` | The server does not run at `--server`. Start the stack first. |
| `sync: enforced set unavailable ([unauthenticated] missing or invalid bearer token)` | The token is not a device token in the directory file. With the compose stack, use `local-only-device-token`. Or add the token to the file and restart the host. |
| The target picker shows `No teams yet.` | The directory file has no teams. Add a team and restart the host. |
| A new terminal shows `harness-agent: command not found` | The `alias` is only for one terminal. See [Installing the command](reference.md#installing-the-command). |
| The status is `PENDING` after you save | The agent gets changes on its next pass. The default interval is `5m`. To run a pass now, run `harness-agent sync`. For the installed macOS service, run `launchctl kickstart -k gui/$(id -u)/harness-control-plane-agent`. `harness-agent status` shows the last pass. |
| The status is `EXCLUDED` for all people after a new database | No agent has reported to the new database yet. Wait for the next pass, or run `harness-agent sync`. |
| A file is deleted after you remove a person from the targets | This is the correct operation. The agent deletes the files of an artifact that is not in its set, on the second pass. See [Enforcement status](reference.md#enforcement-status). |
| A test skill loads in each Claude Code session | The service is installed for your real home directory. See [Try it safely](control-plane.md#try-it-safely). |
| Login Items shows `harness-control-plane-agent` as "Item from unidentified developer" | This is expected. The launcher does not have an Apple Developer ID signature. |

On Linux, `systemctl --user restart harness-control-plane-agent.service` runs a pass now, because `watch` starts with a pass.
