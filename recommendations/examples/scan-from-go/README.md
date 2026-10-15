This example runs the DETECTED on-disk scan from a Go program instead of the CLI, so you get the findings
as values to filter, route or store. It opens the database named by `POSTGRES_TEST_DSN`, applies the
bundled schema, scans a bundled sample repository with `ondisk.Scan`, and prints each finding and the
scan's detector coverage. Use this when you embed the scanner in your own tool. It needs a local Postgres
because the engine persists its findings.

```sh
go run ./recommendations/examples/scan-from-go
```

```text
DETECTED findings: 6
  [HARNESS_FINDING_DIMENSION_SECURITY] Permissions default mode bypasses all prompts (priority 30)
  [HARNESS_FINDING_DIMENSION_CONFLICT] Rule pattern matches no files in the repo (priority 20)
  [HARNESS_FINDING_DIMENSION_SECURITY] Over-broad permission grant (priority 20)
  [HARNESS_FINDING_DIMENSION_SECURITY] Over-broad permission grant (priority 20)
  [HARNESS_FINDING_DIMENSION_CONFLICT] Rule ID defined in more than one file (priority 10)
  [HARNESS_FINDING_DIMENSION_INSTRUCTION_INTENT] Lines are denser than most files in this repo (priority 5)
ran 3 detector families; skipped 4 for lack of on-disk input
```
