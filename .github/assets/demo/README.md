# README demo

The opening three scenes explain actual output from the bundled sample harness. The remaining
scenes are captures of the existing admin UI in its built-in mock mode. The explanatory slides
are narration, not a recommendation-review UI shipped by this repository.

## Text walkthrough

1. Scan `recommendations/ondisk/testdata/sample-repo` with the command in the
   [recommendations quick start](../../../recommendations/README.md#quick-start).
   The engine reports six findings. This on-disk scan runs config-only detectors, makes no
   model call and needs Postgres. Its coverage report names the skipped detectors.
2. Inspect the finding **Rule pattern matches no files in the repo**. The sample's
   `.claude/rules/a.md` targets `src/legacy/**/*.go`, but its source file is `src/main.go`.
3. In a temporary copy of the sample, change that pattern to `src/**/*.go` and scan again.
   The dead-pattern finding disappears; five other findings remain. This is a human-reviewed
   fix, not a generated patch or automatic repair. `scan-before.txt` and `scan-after.txt`
   preserve the actual output (with the temporary directory path normalized).
4. Separately, use the control plane to publish the approved content as a Claude Code rule
   named **Error-handling rules**, with slug `error-handling`.
5. Choose repository placement, `example-org/example-repo`, and the **Platform** team.
   The preview shows `<checkout>/.claude/rules/error-handling.md`. This mock distribution
   example uses a separate checkout, not the sample's existing unmanaged rule files.
6. Create the artifact and inspect per-person status. Ada and Alan are pending; Edsger is
   excluded because no known checkout matches. These are mock states, not a real agent
   installation. The [control plane quick start](../../../docs/control-plane.md#quick-start)
   demonstrates a real agent sync with a temporary home directory.

The scanner and control plane are independent. There is no automatic finding-to-publication
adapter in this demo. The agent does not adopt or overwrite a person's unmanaged files; reconcile
existing definitions before distributing a replacement to avoid duplicate or colliding rules.

## Refresh the captures

Run the admin UI with `pnpm --filter harness-control-plane-web dev`, open `/?mock=1`, and sign in
with the sample token `example-admin-token`. Follow steps 4–6 above. Save full-page screenshots
as `composer.png` (before creating the artifact) and `published.png` (after the detail page loads).
The checked-in captures are 1728 pixels wide; the renderer crops their relevant panels.

Re-run steps 1–3 against a disposable Postgres to refresh the scan transcripts. Keep credentials
out of the transcripts. Normalize the temporary fixed directory to `<fixed-sample-repo>`.

From the repository root, with Pillow installed:

```sh
python3 scripts/render-readme-demo.py
```

On Linux, pass `--font-dir /usr/share/fonts/truetype/dejavu`. Add `--preview-dir /tmp/harness-demo`
to export all six scenes for visual review. The renderer checks that the scan evidence supports
the finding counts and the demonstrated fix before writing `.github/assets/demo.gif`.
