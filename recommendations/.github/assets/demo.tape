# vhs tape for the recommendations demo GIF. Render from the repository root with
# `vhs recommendations/.github/assets/demo.tape` after starting the README quickstart
# Postgres on localhost:55432. It types the quickstart's DETECTED scan
# command verbatim over the bundled sample repository, so the GIF and the README cannot
# disagree. The connection string is set in a silent Env directive, so no DSN is shown; the
# scan's finding ids are deterministic (UUIDv5), so the recorded output is stable across runs
# and across databases.
Output recommendations/.github/assets/demo.gif
Set Width 1100
Set Height 820
Set FontSize 16
Set Padding 20
Env PS1 "$ "
Env POSTGRES_TEST_DSN "host=localhost port=55432 user=postgres password=postgres dbname=postgres sslmode=disable"

# Hidden warm-up: compile the command and populate the database once, off-camera, so the
# recorded run shows only the scan's output with no `go run` build pause.
Hide
Type `go run ./recommendations/cmd/recommendations scan --harness-dir ./recommendations/ondisk/testdata/sample-repo --dsn "$POSTGRES_TEST_DSN" >/dev/null 2>&1` Enter
Sleep 20s
Type "clear" Enter
Sleep 500ms
Show

Sleep 800ms
Type `go run ./recommendations/cmd/recommendations scan --harness-dir ./recommendations/ondisk/testdata/sample-repo --dsn "$POSTGRES_TEST_DSN"` Sleep 500ms Enter
Sleep 8s
