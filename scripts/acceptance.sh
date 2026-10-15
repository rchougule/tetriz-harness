#!/usr/bin/env bash
# End-to-end acceptance: the compose stack, the built agent CLI and a throwaway
# HOME and git checkout, driven through the whole enforcement lifecycle.
#
# Requires: Docker with Compose v2, curl, jq, git, Go toolchain selection, node >= 22, pnpm.
# The agent only ever runs with a temporary HOME and state dir.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
export COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-hcp-acceptance}"
export HARNESS_PORT="${HARNESS_PORT:-18080}"
export HARNESS_ALLOW_HOOKS=true
URL="http://127.0.0.1:${HARNESS_PORT}"
ADMIN_TOKEN="${ADMIN_TOKEN:-local-only-admin-token}"

for dep in docker curl jq git go node pnpm; do
  command -v "$dep" >/dev/null || { echo "acceptance: missing dependency: $dep" >&2; exit 1; }
done
# The stack relies on `compose up --wait`, which Docker Compose accepts from v2 on.
# Read the version rather than the exit status: a `docker` that is Podman's shim
# answers `compose version` successfully from podman-compose (1.x), which has no --wait.
compose_version="$(docker compose version --short 2>/dev/null || true)"
compose_major="${compose_version#v}"
compose_major="${compose_major%%.*}"
case "$compose_major" in
  '' | *[!0-9]*) compose_major=0 ;;
esac
if [ "$compose_major" -lt 2 ]; then
  echo "acceptance: needs Docker Compose v2 or later; 'docker compose version --short' reported '${compose_version:-nothing}'" >&2
  exit 1
fi
if [ "$(id -u)" = 0 ]; then
  echo "acceptance: run as a non-root user; the forced write failure relies on file permissions" >&2
  exit 1
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/hcp-acceptance.XXXXXX")"
AGENT_HOME="$WORK/home"
STATE_DIR="$AGENT_HOME/.config/harness-agent"
REPO="$WORK/src/example-repo"
CLI="$ROOT/agent/dist/cli/main.js"

compose() { docker compose -p "$COMPOSE_PROJECT_NAME" -f "$ROOT/docker-compose.yml" "$@"; }

cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    echo "acceptance: FAILED (exit $status); server logs follow" >&2
    compose logs --no-color --tail 80 server >&2 || true
  fi
  compose down -v --remove-orphans >/dev/null 2>&1 || true
  chmod -R u+w "$WORK" 2>/dev/null || true
  rm -rf "$WORK"
  exit "$status"
}
trap cleanup EXIT INT TERM

step() { printf '\n== %s\n' "$*"; }
fail() { echo "acceptance: $*" >&2; exit 1; }

# admin METHOD JSON: one AdminService call over Connect JSON; prints the response.
admin() {
  curl -sS --fail-with-body -X POST "$URL/harnessctl.v1.AdminService/$1" \
    -H 'Content-Type: application/json' -H "Authorization: Bearer $ADMIN_TOKEN" -d "$2"
}

# agent ARGS...: the CLI with only the temporary HOME in its environment.
agent() {
  env -i PATH="$PATH" HOME="$AGENT_HOME" TMPDIR="${TMPDIR:-/tmp}" \
    node "$CLI" "$@" --server "$URL" --token "$DEVICE_TOKEN" --state-dir "$STATE_DIR" \
    --project "$REPO" --no-discover
}

# sync [EXPECTED_EXIT]: one pass; the exit code must match (default 0).
sync_once() {
  want="${1:-0}"
  set +e
  out="$(agent sync)"
  code=$?
  set -e
  echo "  $out"
  [ "$code" = "$want" ] || fail "sync exited $code, want $want"
}

# expect_status ARTIFACT_ID WANT: the person's status for the artifact's only placement.
expect_status() {
  got="$(admin GetEnforcementStatus "$(jq -nc --arg id "$1" '{artifactId: $id}')" |
    jq -r --arg p "$PERSON" '[.statuses[]? | select(.personId == $p)] | if length == 1 then .[0].status + " " + (.[0].detail // "") else "rows=\(length)" end')"
  echo "  status $1: $got"
  case "$got" in
    "ENFORCEMENT_STATUS_$2"*) ;;
    *) fail "status of $1 is '$got', want $2" ;;
  esac
}

skill_body() { printf -- '---\nname: review\ndescription: Review changes\n---\n%s\n' "$1"; }

step "compose up ($COMPOSE_PROJECT_NAME on :$HARNESS_PORT)"
compose up --build --wait --quiet-pull

step "build the agent CLI"
(cd "$ROOT" && pnpm install --frozen-lockfile --reporter=silent && pnpm -r --reporter=silent build)
[ -x "$CLI" ] || fail "agent CLI not built at $CLI"

step "temp HOME and checkout"
mkdir -p "$AGENT_HOME" "$REPO"
git -C "$REPO" init -q
# Mixed case on purpose: repository names match case-insensitively.
git -C "$REPO" remote add origin https://git.example.com/Example-Org/Example-Repo.git
echo "  HOME=$AGENT_HOME repo=$REPO"

step "person, team and device token from the sample directory"
# cmd/harnessctl-server/directory.local.json, mounted by docker-compose.yml.
PERSON=dev-1
TEAM=platform
DEVICE_TOKEN="${DEVICE_TOKEN:-local-only-device-token}"
admin ListTeams '{}' | jq -e --arg t "$TEAM" --arg p "$PERSON" '.teams[] | select(.teamId == $t) | .memberPersonIds | index($p)' >/dev/null ||
  fail "sample directory has no team $TEAM with $PERSON"
echo "  person=$PERSON team=$TEAM"

step "an unknown bearer is refused on both services"
for svc in AdminService/GetSettings AgentService/GetEnforcedSet; do
  code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$URL/harnessctl.v1.$svc" -H 'Content-Type: application/json' -H 'Authorization: Bearer wrong' -d '{}')"
  [ "$code" = 401 ] || fail "$svc with a wrong token returned $code, want 401"
done

step "a Claude hook for the team (user-global) and a skill for the team (repo-local)"
HOOK="$(admin UpsertArtifact "$(jq -nc '{ideType: "IDE_TYPE_CLAUDE_CODE", type: "ARTIFACT_TYPE_HOOK", slug: "guard", name: "Guard",
  inlineContent: "#!/bin/sh\nexit 0\n", hookConfig: {event: "PreToolUse", matcher: "Bash", timeoutSeconds: 10}}')" | jq -er '.artifactId')"
admin SetTargets "$(jq -nc --arg id "$HOOK" --arg t "$TEAM" '{artifactId: $id, scope: "ARTIFACT_SCOPE_USER_GLOBAL", audience: {teamIds: [$t]}}')" >/dev/null
# upsert_skill BODY [ARTIFACT_ID]: create the skill, or replace its body.
upsert_skill() {
  req="$(jq -nc --arg id "${2:-}" --arg body "$(skill_body "$1")" \
    '{ideType: "IDE_TYPE_CLAUDE_CODE", type: "ARTIFACT_TYPE_SKILL", slug: "review", name: "Review",
      files: [{relativePath: "SKILL.md", content: $body}]} + (if $id == "" then {} else {artifactId: $id} end)')"
  admin UpsertArtifact "$req" | jq -er '.artifactId'
}
SKILL="$(upsert_skill "v1")"
admin SetTargets "$(jq -nc --arg id "$SKILL" --arg t "$TEAM" '{artifactId: $id, scope: "ARTIFACT_SCOPE_REPO_LOCAL", repository: "example-org/example-repo", audience: {teamIds: [$t]}}')" >/dev/null
SKILL_FILE="$REPO/.claude/skills/review/SKILL.md"
echo "  hook=$HOOK skill=$SKILL"

step "sync installs both"
sync_once
jq -e '.hooks.PreToolUse[0].hooks[0].command | test("guard\\.sh")' "$AGENT_HOME/.claude/settings.json" >/dev/null ||
  fail "settings.json has no merged guard hook"
[ -x "$AGENT_HOME/.claude/hooks/guard.sh" ] || fail "hook script not placed"
[ "$(cat "$SKILL_FILE")" = "$(skill_body v1)" ] || fail "skill body not placed"
expect_status "$HOOK" INSTALLED
expect_status "$SKILL" INSTALLED

step "a body change is PENDING until the next sync"
upsert_skill "v2" "$SKILL" >/dev/null
expect_status "$SKILL" PENDING
sync_once
[ "$(cat "$SKILL_FILE")" = "$(skill_body v2)" ] || fail "skill not updated"
expect_status "$SKILL" INSTALLED

step "a write failure reads FAILED, and recovers"
upsert_skill "v3" "$SKILL" >/dev/null
chmod 0555 "$(dirname "$SKILL_FILE")"
sync_once 1
expect_status "$SKILL" FAILED
chmod 0755 "$(dirname "$SKILL_FILE")"
sync_once
expect_status "$SKILL" INSTALLED

step "retracting the skill removes its unmodified file"
admin RemoveArtifact "$(jq -nc --arg id "$SKILL" '{artifactId: $id}')" >/dev/null
sync_once # the empty-read floor holds a destructive retract one pass
sync_once
[ ! -e "$SKILL_FILE" ] || fail "retracted skill file still on disk"
expect_status "$SKILL" REMOVED

step "a closed enforcement window reads EXPIRED"
RULE="$(admin UpsertArtifact '{"ideType":"IDE_TYPE_CLAUDE_CODE","type":"ARTIFACT_TYPE_RULE","slug":"style","name":"Style","inlineContent":"be terse\n"}' | jq -er '.artifactId')"
admin SetTargets "$(jq -nc --arg id "$RULE" --arg t "$TEAM" '{artifactId: $id, scope: "ARTIFACT_SCOPE_USER_GLOBAL", audience: {teamIds: [$t]}}')" >/dev/null
sync_once
expect_status "$RULE" INSTALLED
# The API only ever opens a window; closing one early is a clock the test does not have.
compose exec -T postgres psql -q -U harness -d harness -c \
  "UPDATE org_artifacts SET enforce_until = now() - interval '1 minute' WHERE id = '$RULE'" >/dev/null
expect_status "$RULE" EXPIRED
sync_once
[ -f "$AGENT_HOME/.claude/rules/style.md" ] || fail "an expired artifact must be left in place"

step "pause: the agent gets a paused set and writes nothing"
admin PauseEnforcement '{}' | jq -e '.paused == true' >/dev/null
before="$(cat "$AGENT_HOME/.claude/settings.json")"
set +e
paused="$(agent sync --json)"
code=$?
set -e
[ "$code" = 0 ] || fail "paused sync exited $code"
echo "$paused" | jq -e '.paused == true and .fetched == true and .artifacts == 0' >/dev/null || fail "agent did not see a paused set: $paused"
[ "$(cat "$AGENT_HOME/.claude/settings.json")" = "$before" ] || fail "settings changed while paused"
[ -x "$AGENT_HOME/.claude/hooks/guard.sh" ] || fail "hook removed while paused"
expect_status "$HOOK" INSTALLED

printf '\nacceptance: PASS\n'
