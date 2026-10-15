# vhs tape for the control plane demo GIF. Build the agent and start the compose stack first
# (`pnpm install --frozen-lockfile && pnpm -r build && docker compose up --build --wait`), then
# render from the repository root with `vhs .github/assets/control-plane.tape`. It types the quick start's commands from
# docs/control-plane.md, so the GIF and the guide cannot disagree. The agent runs with a
# temporary HOME, as in the guide. The hidden admin helper also prints a newline after each
# response, so the prompt starts on its own line.
Output .github/assets/control-plane.gif
Set Width 1100
Set Height 400
Set FontSize 16
Set Padding 20
Set TypingSpeed 15ms
Env PS1 "$ "

# Hidden setup: the quick start's alias, admin helper and temporary home directory.
Hide
Type `alias harness-agent="node $PWD/agent/dist/cli/main.js"` Enter
Type `admin() { curl -sS -X POST "http://127.0.0.1:8080/harnessctl.v1.AdminService/$1" -H 'Content-Type: application/json' -H 'Authorization: Bearer local-only-admin-token' -d "$2"; echo; }` Enter
Type `DEV_HOME=$(mktemp -d /tmp/dev-home.XXXXXX)` Enter
Type "clear" Enter
Sleep 500ms
Show

Sleep 800ms
Type `SKILL=$(admin UpsertArtifact '{"ideType":"IDE_TYPE_CLAUDE_CODE","type":"ARTIFACT_TYPE_SKILL","slug":"review","name":"Review","files":[{"relativePath":"SKILL.md","content":"---\nname: review\ndescription: Review the current diff\n---\nList risky changes.\n"}]}' | jq -r .artifactId)` Sleep 300ms Enter
Sleep 1s
Type `admin SetTargets "{\"artifactId\":\"$SKILL\",\"scope\":\"ARTIFACT_SCOPE_USER_GLOBAL\",\"audience\":{\"personIds\":[\"dev-1\"]}}"` Sleep 300ms Enter
Sleep 2s
Type `HOME="$DEV_HOME" harness-agent sync --server http://127.0.0.1:8080 --token local-only-device-token --no-discover` Sleep 300ms Enter
Sleep 3s
Type `find "$DEV_HOME/.claude" -type f` Sleep 300ms Enter
Sleep 1500ms
Type `admin GetEnforcementStatus "{\"artifactId\":\"$SKILL\"}" | jq -r '.statuses[].status'` Sleep 300ms Enter
Sleep 4s
