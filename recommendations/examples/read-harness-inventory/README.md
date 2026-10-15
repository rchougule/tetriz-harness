This example classifies the harness surface files in a repository on disk and prints the inventory,
with no database, no model and no network call. It is the offline half of the DETECTED scan: it walks a
bundled sample repository, classifies every known harness file per tool (CLAUDE.md, rules, skills,
commands, agents, and a settings.json fanned into its permission and hook parts), counts the files the
path lenses resolve against, and lists any declared MCP servers. Read the inventory to see what the
scanner will look at before you stand up a database.

```sh
go run ./recommendations/examples/read-harness-inventory
```

```text
artifacts: 9 over 11 files
by type:
  HARNESS_ARTIFACT_TYPE_AGENT: 1
  HARNESS_ARTIFACT_TYPE_AGENTS_MD: 1
  HARNESS_ARTIFACT_TYPE_CLAUDE_MD: 1
  HARNESS_ARTIFACT_TYPE_COMMAND: 1
  HARNESS_ARTIFACT_TYPE_HOOK: 1
  HARNESS_ARTIFACT_TYPE_PERMISSIONS: 1
  HARNESS_ARTIFACT_TYPE_RULE: 2
  HARNESS_ARTIFACT_TYPE_SKILL: 1
mcp servers declared: [filesystem]
```
