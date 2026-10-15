This example is the quickstart: it points the DETECTED scan at a repository's committed harness on disk
and prints the findings and the scan's detector coverage. It needs no session data, no model and no
organization onboarding, and it is the same command the recommendations README's quick start runs. It
needs a local Postgres because the engine persists its findings; the runner reads the connection
from `POSTGRES_TEST_DSN`, and the finding id on each second line is assigned per scan.

```sh
go run ./recommendations/cmd/recommendations scan --harness-dir ./recommendations/ondisk/testdata/sample-repo --dsn "$POSTGRES_TEST_DSN"
```

```text
scanned ./recommendations/ondisk/testdata/sample-repo: 9 harness artifacts over 11 files; 6 DETECTED finding(s)
  - [HARNESS_FINDING_DIMENSION_SECURITY] Permissions default mode bypasses all prompts (priority 30) [live]
    <finding-id>
  - [HARNESS_FINDING_DIMENSION_CONFLICT] Rule pattern matches no files in the repo (priority 20) [live]
    <finding-id>
  - [HARNESS_FINDING_DIMENSION_SECURITY] Over-broad permission grant (priority 20) [live]
    <finding-id>
  - [HARNESS_FINDING_DIMENSION_SECURITY] Over-broad permission grant (priority 20) [live]
    <finding-id>
  - [HARNESS_FINDING_DIMENSION_CONFLICT] Rule ID defined in more than one file (priority 10) [live]
    <finding-id>
  - [HARNESS_FINDING_DIMENSION_INSTRUCTION_INTENT] Lines are denser than most files in this repo (priority 5) [live]
    <finding-id>

detector coverage:
  ran (config-only, over the committed harness on disk):
    - Conflict (dup-id, dead-glob, broken-path, root-restatement)
    - Instruction-Intent (placement and density over committed instruction files)
    - Security permission inventory (grants and default-mode in a settings.json)
  skipped (need session/usage/org data a repository walk does not provide):
    - Behavioral deviation: needs per-skill usage behavior (session data) and LLM intent classification
    - Personal / never-used: needs per-person usage and primary-repo ownership
    - Org MCP sprawl: needs org-wide MCP usage (the org-relational scan, not wired on disk)
    - Org coverage-gap: needs org-wide signals (the org-relational scan, not wired on disk)
```
