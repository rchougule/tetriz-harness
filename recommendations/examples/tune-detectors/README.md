This example tunes the DETECTED detectors from your own Go program with public imports only. It scans a
bundled sample repository twice: once with the built-in settings, and once with a
`recommendations.DetectorConfig` that adds `.windsurfrules` to the vendor-surface markers. The sample's
`CLAUDE.md` points the agent at `.windsurfrules`, so only the tuned scan reports it. Start from
`recommendations.DefaultDetectorConfig()` and change only the fields you need; every unset field keeps its
built-in value. Pass the config through `ondisk.Options.DetectorConfig`, or to
`recommendations.NewHarnessImproveEngine` when you assemble the engine yourself. It needs a local Postgres
because the engine persists its findings.

```sh
go run ./recommendations/examples/tune-detectors
```

```text
built-in markers: 0 vendor-surface finding(s)
with .windsurfrules added: 1 vendor-surface finding(s)
  [HARNESS_FINDING_DIMENSION_CONFLICT] File points Claude at another tool's config (priority 10)
```
