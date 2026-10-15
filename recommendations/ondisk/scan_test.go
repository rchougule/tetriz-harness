// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package ondisk_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/tetriz-ai/tetriz-harness/recommendations"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/testsupport"
	"github.com/tetriz-ai/tetriz-harness/recommendations/ondisk"
)

// allMigrations is the full schema the on-disk scan needs — the inventory tables it seeds plus the
// prompt_library / task_executions / content tables the engine writes.
var allMigrations = []string{
	"0001_session_signals.sql", "0002_vcs_pr_signals.sql", "0003_pm_signals.sql",
	"0004_harness_inventory.sql", "0005_identity.sql", "0006_prompt_library.sql",
	"0007_task_executions.sql", "0008_prompt_library_content.sql",
}

// TestScan_PersistsDetectedFindingsFromDisk is the zero-setup acceptance test: point the reader at a
// fixture repo on disk (no org/repo ids, no session data, no LLM) and assert the config-only DETECTED
// detectors persist their findings. It exercises three detectors end-to-end from disk: dup-id and
// dead-glob (the Conflict lens) and the permission inventory (over the fanned settings.json). It runs
// in one rolled-back transaction loading every migration; SKIPs cleanly without POSTGRES_TEST_DSN.
func TestScan_PersistsDetectedFindingsFromDisk(t *testing.T) {
	tx := testsupport.OpenTestTx(t)
	testsupport.LoadMigrations(t, tx, allMigrations...)

	result, err := ondisk.Scan(context.Background(), tx, ondisk.Options{Dir: "testdata/sample-repo"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if result.ArtifactCount < 8 {
		t.Errorf("ArtifactCount = %d, want >= 8 (the classified harness surfaces)", result.ArtifactCount)
	}
	if len(result.Findings) == 0 {
		t.Fatal("no DETECTED findings persisted from disk — the config-only detectors produced nothing")
	}

	// Every finding is a DETECTED finding (the on-disk path runs only the DETECTED detectors).
	for _, f := range result.Findings {
		if f.Source != "ARTIFACT_RECOMMENDATION_SOURCE_DETECTED" {
			t.Errorf("finding %q source = %q, want DETECTED", f.Slug, f.Source)
		}
		if f.ScopeValue != result.RepoID {
			t.Errorf("finding %q scope_value = %q, want the repo id %q", f.Slug, f.ScopeValue, result.RepoID)
		}
	}

	// The three config-only detectors that must fire on this fixture.
	var dupID, deadGlob, permissionGrant, defaultMode *ondisk.Finding
	for i := range result.Findings {
		f := &result.Findings[i]
		switch {
		case f.Dimension == "HARNESS_FINDING_DIMENSION_CONFLICT" && strings.Contains(f.Slug, "dup-id"):
			dupID = f
		case f.Dimension == "HARNESS_FINDING_DIMENSION_CONFLICT" && strings.Contains(f.Slug, "dead-glob"):
			deadGlob = f
		case f.Dimension == "HARNESS_FINDING_DIMENSION_SECURITY" && strings.Contains(f.Slug, "permission-grant"):
			permissionGrant = f
		case f.Dimension == "HARNESS_FINDING_DIMENSION_SECURITY" && strings.Contains(f.Slug, "permission-default-mode"):
			defaultMode = f
		}
	}

	if dupID == nil {
		t.Errorf("no dup-id CONFLICT finding; findings = %s", summarize(result.Findings))
	} else if !dupID.IsLatest {
		t.Error("dup-id finding is_latest = false, want true (the reconcile promotes the latest visible run)")
	}
	if deadGlob == nil {
		t.Errorf("no dead-glob CONFLICT finding; findings = %s", summarize(result.Findings))
	}
	if permissionGrant == nil {
		t.Errorf("no permission-grant SECURITY finding (over-broad allow in settings.json); findings = %s", summarize(result.Findings))
	}
	if defaultMode == nil {
		t.Errorf("no permission-default-mode SECURITY finding (bypassPermissions in settings.json); findings = %s", summarize(result.Findings))
	}

	// Pin the EXACT live membership, not just the four named detectors above: without this a
	// too-aggressive filter in readDetectedFindings that silently drops a LIVE finding (the second
	// permission grant, or the instruction-intent density finding) would still pass the suite. The full
	// slug is not portable across machines — it is hashed from the repo id, which is derived from the
	// fixture's absolute path (see mintArtifactID / conflictSlug / securitySlug), which is exactly why
	// run-examples.sh normalizes the per-scan finding-id line away. The detector key — the slug's stable
	// `<dimension>:<family>` middle segments — IS portable, so assert the exact multiset of the six live
	// findings this fixture yields (two distinct permission grants included). Dropping any one live
	// finding fails either the count or the multiset.
	wantLiveDetectors := map[string]int{
		"security:permission-default-mode": 1,
		"conflict:dead-glob":               1,
		"security:permission-grant":        2,
		"conflict:dup-id":                  1,
		"instruction-intent:tok-per-line":  1,
	}
	const wantLive = 6
	gotLive := 0
	gotLiveDetectors := map[string]int{}
	for _, f := range result.Findings {
		if !f.IsLatest {
			continue
		}
		gotLive++
		gotLiveDetectors[detectorKey(f.Slug)]++
	}
	if gotLive != wantLive {
		t.Fatalf("live finding count = %d, want %d; findings = %s", gotLive, wantLive, summarize(result.Findings))
	}
	if !reflect.DeepEqual(gotLiveDetectors, wantLiveDetectors) {
		t.Errorf("live detector multiset = %v, want %v; findings = %s", gotLiveDetectors, wantLiveDetectors, summarize(result.Findings))
	}
}

// detectorKey returns a finding slug's portable detector identity: its "<dimension>:<family>" middle
// segments (e.g. "security:permission-grant"). The leading repo id and the trailing content hash are
// both derived from the fixture's absolute path, so only this middle pair is stable across machines.
func detectorKey(slug string) string {
	parts := strings.Split(slug, ":")
	if len(parts) < 3 {
		return slug
	}
	return parts[1] + ":" + parts[2]
}

// TestScan_IsIdempotentOnReScan proves a second scan of the same directory succeeds (no vcs_repositories
// primary-key conflict) and the findings reconcile in place: the second run's DETECTED findings are the
// live (is_latest) partition, and no duplicate harness_files rows accumulate for unchanged bodies.
func TestScan_IsIdempotentOnReScan(t *testing.T) {
	tx := testsupport.OpenTestTx(t)
	testsupport.LoadMigrations(t, tx, allMigrations...)
	ctx := context.Background()

	first, err := ondisk.Scan(ctx, tx, ondisk.Options{Dir: "testdata/sample-repo"})
	if err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	var filesAfterFirst int64
	if err := tx.Raw(`SELECT count(*) FROM harness_files WHERE organization_id = ?`, first.OrgID).Scan(&filesAfterFirst).Error; err != nil {
		t.Fatalf("counting harness_files after first scan: %v", err)
	}

	second, err := ondisk.Scan(ctx, tx, ondisk.Options{Dir: "testdata/sample-repo"})
	if err != nil {
		t.Fatalf("second Scan (re-scan must not PK-conflict): %v", err)
	}

	if second.RepoID != first.RepoID {
		t.Errorf("re-scan repo id = %q, want the same partition %q", second.RepoID, first.RepoID)
	}
	if len(second.Findings) == 0 {
		t.Fatal("re-scan persisted no DETECTED findings")
	}
	// The scan surfaces only the live (is_latest) partition, so after the reconcile the re-scan's
	// findings are the current run's — assert the dup-id finding is present and live.
	var dupLive bool
	for _, f := range second.Findings {
		if f.Dimension == "HARNESS_FINDING_DIMENSION_CONFLICT" && strings.Contains(f.Slug, "dup-id") && f.IsLatest {
			dupLive = true
		}
	}
	if !dupLive {
		t.Errorf("after re-scan no live (is_latest) dup-id finding; findings = %s", summarize(second.Findings))
	}

	// No duplicate harness_files rows accumulated for the unchanged bodies.
	var filesAfterSecond int64
	if err := tx.Raw(`SELECT count(*) FROM harness_files WHERE organization_id = ?`, second.OrgID).Scan(&filesAfterSecond).Error; err != nil {
		t.Fatalf("counting harness_files after second scan: %v", err)
	}
	if filesAfterSecond != filesAfterFirst {
		t.Errorf("harness_files grew from %d to %d on re-scan; content-addressed bodies must be reused", filesAfterFirst, filesAfterSecond)
	}

	// Exactly one vcs_repositories row for the partition (the re-seed replaced it, never duplicated it).
	var repoRows int64
	if err := tx.Raw(`SELECT count(*) FROM vcs_repositories WHERE id = ?`, second.RepoID).Scan(&repoRows).Error; err != nil {
		t.Fatalf("counting vcs_repositories: %v", err)
	}
	if repoRows != 1 {
		t.Errorf("vcs_repositories rows for the partition = %d, want 1", repoRows)
	}
}

// TestScan_FindingSlugsStableAcrossScans is the regression guard for the deterministic seeded ids:
// a DETECTED finding's slug is hashed from the harness_artifacts id, so scanning the same fixture
// twice must yield the SAME slug for every finding. (Before the fix the seed minted a fresh random
// artifact id per scan, so the slug changed every scan — breaking cross-scan suppression and
// loosening the re-scan reconcile.) The live (is_latest) slug set of the first scan must equal that
// of the second: the scan surfaces only the live partition each run owns, so the two live slug sets
// compare directly.
func TestScan_FindingSlugsStableAcrossScans(t *testing.T) {
	tx := testsupport.OpenTestTx(t)
	testsupport.LoadMigrations(t, tx, allMigrations...)
	ctx := context.Background()

	// first.Findings is captured before the second scan supersedes it, so its is_latest rows are the
	// first run's own live partition.
	first, err := ondisk.Scan(ctx, tx, ondisk.Options{Dir: "testdata/sample-repo"})
	if err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	second, err := ondisk.Scan(ctx, tx, ondisk.Options{Dir: "testdata/sample-repo"})
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}

	firstSlugs := liveSlugsOf(first.Findings)
	secondSlugs := liveSlugsOf(second.Findings)
	if len(firstSlugs) == 0 {
		t.Fatal("first scan produced no live findings to compare slugs for")
	}
	if len(firstSlugs) != len(secondSlugs) {
		t.Fatalf("live finding count changed across scans: first=%d second=%d\n first:%s\n second:%s",
			len(firstSlugs), len(secondSlugs), strings.Join(firstSlugs, ","), strings.Join(secondSlugs, ","))
	}
	for i := range firstSlugs {
		if firstSlugs[i] != secondSlugs[i] {
			t.Errorf("finding slug not stable across scans: first=%q second=%q", firstSlugs[i], secondSlugs[i])
		}
	}
}

// TestRunMigrations_IdempotentAcrossScans guards the CLI re-scan path. The `recommendations scan` command
// calls recommendations.RunMigrations on every invocation, so a second scan against the same database
// re-applies the whole migration set. This asserts the real RunMigrations (not the test LoadMigrations
// copy) is a no-op on an already-migrated database, and that a migrate+scan cycle therefore succeeds
// twice with a stable live finding set — the re-scannable-in-place claim the README makes for the CLI.
func TestRunMigrations_IdempotentAcrossScans(t *testing.T) {
	tx := testsupport.OpenTestTx(t)
	ctx := context.Background()

	if err := recommendations.RunMigrations(tx); err != nil {
		t.Fatalf("first RunMigrations: %v", err)
	}
	// The second apply is the regression: before the IF NOT EXISTS guard it failed with
	// "relation already exists", which is exactly what broke a second CLI scan.
	if err := recommendations.RunMigrations(tx); err != nil {
		t.Fatalf("second RunMigrations must be a no-op on an already-migrated database: %v", err)
	}

	first, err := ondisk.Scan(ctx, tx, ondisk.Options{Dir: "testdata/sample-repo"})
	if err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	second, err := ondisk.Scan(ctx, tx, ondisk.Options{Dir: "testdata/sample-repo"})
	if err != nil {
		t.Fatalf("second Scan after a re-run of migrations: %v", err)
	}

	firstSlugs := liveSlugsOf(first.Findings)
	secondSlugs := liveSlugsOf(second.Findings)
	if len(firstSlugs) == 0 {
		t.Fatal("first scan produced no live findings")
	}
	if len(firstSlugs) != len(secondSlugs) {
		t.Fatalf("live finding count changed across scans: first=%d second=%d", len(firstSlugs), len(secondSlugs))
	}
	for i := range firstSlugs {
		if firstSlugs[i] != secondSlugs[i] {
			t.Errorf("live finding slug not stable across scans: first=%q second=%q", firstSlugs[i], secondSlugs[i])
		}
	}
}

// TestScan_LiveFindingCountStableAcrossReScans is the targeted guard for the re-scan count bug: the
// Scan Result (and, through it, the CLI's count and listing) must surface only the LIVE (is_latest)
// DETECTED findings, so re-scanning the SAME directory reports the SAME number of findings every run.
// Before the is_latest filter, the projection returned every historical row, so three scans of one
// fixture printed 6 → 12 → 18 findings — directly contradicting the README's reconcile-not-duplicate
// promise. The reconcile still keeps the superseded rows in the table as history; they just must not
// leak into the surfaced result. The fixture's live DETECTED finding count is asserted stable across
// three consecutive re-scans.
func TestScan_LiveFindingCountStableAcrossReScans(t *testing.T) {
	tx := testsupport.OpenTestTx(t)
	testsupport.LoadMigrations(t, tx, allMigrations...)
	ctx := context.Background()

	const scans = 3
	counts := make([]int, 0, scans)
	for i := 0; i < scans; i++ {
		result, err := ondisk.Scan(ctx, tx, ondisk.Options{Dir: "testdata/sample-repo"})
		if err != nil {
			t.Fatalf("scan %d: %v", i+1, err)
		}
		// Every surfaced finding must be live; a superseded row leaking in is the exact bug guarded here.
		for _, f := range result.Findings {
			if !f.IsLatest {
				t.Errorf("scan %d surfaced a superseded (is_latest=false) finding %q; only live findings must be returned", i+1, f.Slug)
			}
		}
		counts = append(counts, len(result.Findings))
	}

	// The surfaced count must be identical every scan (6 → 6 → 6), never growing by the live count per
	// run (the 6 → 12 → 18 regression this test locks out).
	for i := 1; i < len(counts); i++ {
		if counts[i] != counts[0] {
			t.Fatalf("live finding count not stable across re-scans: got %v, want all equal (re-scan must reconcile, not duplicate)", counts)
		}
	}
	if counts[0] == 0 {
		t.Fatal("scan produced no live findings; the config-only detectors must fire on this fixture")
	}
	t.Logf("live DETECTED finding count across %d re-scans: %v", scans, counts)
}

// TestScan_DeadGlobFiresForGitIgnoredOnlyMatch is the end-to-end face of the gitignore-aware walk (the
// ticket's repro): a git work tree whose .gitignore excludes node_modules/, and a rule whose scoping glob
// (`paths: node_modules/**`) matches ONLY a file under node_modules/. Before the fix the walk recorded the
// ignored file in RepoPaths, so the glob "matched" it and NO dead-glob finding was raised; after the fix
// the ignored file is excluded, so the glob matches nothing and the dead-glob CONFLICT finding fires. It
// also asserts the ignored file is excluded from the scan's RepoPathCount. Needs Postgres (SKIPs without
// POSTGRES_TEST_DSN); SKIPs if git is not on PATH.
func TestScan_DeadGlobFiresForGitIgnoredOnlyMatch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; the gitignore-aware walk cannot be exercised")
	}
	tx := testsupport.OpenTestTx(t)
	testsupport.LoadMigrations(t, tx, allMigrations...)

	dir := t.TempDir()
	mustWrite(t, dir, ".gitignore", "node_modules/\n")
	// The rule's scoping glob matches only the ignored file below.
	mustWrite(t, dir, ".claude/rules/scoped.md", "---\npaths: node_modules/**\n---\nscoped rule body\n")
	mustWrite(t, dir, "src/main.go", "package main\n")
	mustWrite(t, dir, "node_modules/dep/index.md", "the only file node_modules/** matches\n")
	mustGit(t, dir, "init")

	result, err := ondisk.Scan(context.Background(), tx, ondisk.Options{Dir: dir})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// The ignored file is excluded from the committed file-path set the path lenses resolve against.
	if result.RepoPathCount != 3 { // .gitignore, .claude/rules/scoped.md, src/main.go
		t.Errorf("RepoPathCount = %d, want 3 (the ignored node_modules/ tree must be excluded)", result.RepoPathCount)
	}

	var deadGlob bool
	for _, f := range result.Findings {
		if f.IsLatest && f.Dimension == "HARNESS_FINDING_DIMENSION_CONFLICT" && strings.Contains(f.Slug, "dead-glob") {
			deadGlob = true
		}
	}
	if !deadGlob {
		t.Errorf("no live dead-glob CONFLICT finding; the rule's `node_modules/**` glob matches only the git-ignored file, which must now count as non-existent. findings = %s", summarize(result.Findings))
	}
}

// mustWrite writes rel (slash-relative to dir), creating parent directories, failing the test on error.
func mustWrite(t *testing.T, dir, rel, body string) {
	t.Helper()
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir for %q: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %q: %v", rel, err)
	}
}

// mustGit runs a git command in dir, failing the test on error.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestScan_DetectorConfigReachesTheDetectors proves Options.DetectorConfig is honoured: the fixture's
// CLAUDE.md cites .windsurfrules, which is not a built-in vendor-surface marker, so the default scan
// raises no vendor-surface finding, and a scan whose config adds that marker raises one. The config is
// built with the public recommendations.DefaultDetectorConfig, the path a caller outside the module uses.
func TestScan_DetectorConfigReachesTheDetectors(t *testing.T) {
	tx := testsupport.OpenTestTx(t)
	testsupport.LoadMigrations(t, tx, allMigrations...)
	ctx := context.Background()
	const vendorSurfaceTitle = "File points Claude at another tool's config"
	countVendorSurface := func(findings []ondisk.Finding) int {
		n := 0
		for _, f := range findings {
			if f.Title == vendorSurfaceTitle {
				n++
			}
		}
		return n
	}

	defaults, err := ondisk.Scan(ctx, tx, ondisk.Options{
		Dir:    "testdata/custom-marker-repo",
		RepoID: "44444444-4444-4444-4444-444444444441",
	})
	if err != nil {
		t.Fatalf("default Scan: %v", err)
	}
	if got := countVendorSurface(defaults.Findings); got != 0 {
		t.Fatalf("default scan raised %d vendor-surface findings, want 0:%s", got, summarize(defaults.Findings))
	}

	cfg := recommendations.DefaultDetectorConfig()
	cfg.VendorSurfaceMarkers = append(cfg.VendorSurfaceMarkers, ".windsurfrules")
	tuned, err := ondisk.Scan(ctx, tx, ondisk.Options{
		Dir:            "testdata/custom-marker-repo",
		RepoID:         "44444444-4444-4444-4444-444444444442",
		DetectorConfig: &cfg,
	})
	if err != nil {
		t.Fatalf("tuned Scan: %v", err)
	}
	if got := countVendorSurface(tuned.Findings); got != 1 {
		t.Fatalf("tuned scan raised %d vendor-surface findings, want 1:%s", got, summarize(tuned.Findings))
	}
}

// liveSlugsOf returns the is_latest findings' slugs, sorted, so the live partitions of two scans
// compare order-independently.
func liveSlugsOf(findings []ondisk.Finding) []string {
	slugs := make([]string, 0, len(findings))
	for _, f := range findings {
		if f.IsLatest {
			slugs = append(slugs, f.Slug)
		}
	}
	sort.Strings(slugs)
	return slugs
}

func summarize(findings []ondisk.Finding) string {
	var b strings.Builder
	for _, f := range findings {
		b.WriteString("\n    ")
		b.WriteString(f.Dimension)
		b.WriteString(" ")
		b.WriteString(f.Slug)
	}
	return b.String()
}
