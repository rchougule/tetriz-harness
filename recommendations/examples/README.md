# Examples

| You want | Example |
|---|---|
| Classify a repository's harness files offline, with no database | [read-harness-inventory](read-harness-inventory/) |
| Scan a repository's committed harness and print its findings (the quickstart) | [scan-a-repo](scan-a-repo/) |
| Run the DETECTED scan from your own Go program | [scan-from-go](scan-from-go/) |
| Change a detector setting, such as the vendor-surface markers | [tune-detectors](tune-detectors/) |

Run them all with `recommendations/scripts/run-examples.sh`, which runs each example's documented command
from a fresh clone and compares its output to the example's README. `read-harness-inventory` runs offline;
the scan examples need a local Postgres, so the runner creates a throwaway database from
`POSTGRES_TEST_DSN` for each one and drops it afterward. Finding ids are assigned per scan, so the runner
rewrites each finding-id line to `<finding-id>` before comparing.
