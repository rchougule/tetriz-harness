// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"
)

// Real-Postgres tests for the PR-signal aggregates: open→merge latency, pickup time, PR size, loops-per-PR
// (the AI/non-AI split collapses), files_line_stats per-module churn, and the
// bug-signal closure join. Each runs in one rolled-back tx loading the migrations it
// needs.

const (
	prOrgA   = "11111111-1111-1111-1111-111111111111"
	prRepoID = "33333333-3333-3333-3333-333333333331"
	prConn   = "conn-1"
	prPRepo  = "prepo-1"
	prIdent1 = "99999999-9999-9999-9999-999999999991"
	prIdent2 = "99999999-9999-9999-9999-999999999992"
)

// insertPR seeds one pull_requests row. openedAgo/mergedAgo/firstReviewAgo are hours before `now`; a
// negative value means the timestamp is NULL.
func insertPR(t *testing.T, tx *gorm.DB, id string, now time.Time, openedAgo, mergedAgo, firstReviewAgo float64,
	linesAdded, linesDeleted, reviewCycles int, filesLineStats string) {
	t.Helper()
	opened := now.Add(-time.Duration(openedAgo * float64(time.Hour)))
	var merged, firstReview any
	if mergedAgo >= 0 {
		merged = now.Add(-time.Duration(mergedAgo * float64(time.Hour)))
	}
	if firstReviewAgo >= 0 {
		firstReview = now.Add(-time.Duration(firstReviewAgo * float64(time.Hour)))
	}
	var files any
	if filesLineStats == "" {
		files = nil
	} else {
		files = filesLineStats
	}
	const ins = `INSERT INTO pull_requests
		(id, organization_id, repository_id, connector_id, platform_repo_id, platform_identity_id, state,
		 opened_at, ready_for_review_at, first_review_at, merged_at, lines_added, lines_deleted, review_cycles, files_line_stats)
		VALUES (?, ?, ?, ?, ?, ?, 'PR_STATE_MERGED', ?, ?, ?, ?, ?, ?, ?, ?::jsonb)`
	if err := tx.Exec(ins, id, prOrgA, prRepoID, prConn, prPRepo, prIdent1, opened, opened, firstReview, merged,
		linesAdded, linesDeleted, reviewCycles, files).Error; err != nil {
		t.Fatalf("seeding pull_requests %s: %v", id, err)
	}
}

func prFilter() FilterOption {
	return WithPullRequestRepoIdentities([]ConnectorRepo{{ConnectorID: prConn, PlatformRepoID: prPRepo}})
}

func TestPullRequestRepo_MergeHours(t *testing.T) {
	tx := openInputTx(t, "0002_vcs_pr_signals.sql")
	repo := NewPullRequestRepo(tx)
	ctx := context.Background()
	now := time.Now().UTC()
	from, to := now.Add(-24*time.Hour), now.Add(time.Hour)

	insertPR(t, tx, "a0000000-0000-0000-0000-000000000001", now, 10, 0, 9, 100, 0, 1, "")  // 10h open→merge
	insertPR(t, tx, "a0000000-0000-0000-0000-000000000002", now, 20, 0, 19, 100, 0, 1, "") // 20h

	stats, err := repo.AggregateMergeHoursForIdentities(ctx, prOrgA, []string{prIdent1}, from, to)
	if err != nil {
		t.Fatalf("AggregateMergeHoursForIdentities: %v", err)
	}
	if stats.PRCount != 2 {
		t.Errorf("PRCount = %d, want 2", stats.PRCount)
	}
	if stats.AvgMergeHours < 14.9 || stats.AvgMergeHours > 15.1 {
		t.Errorf("AvgMergeHours = %v, want ~15 (mean of 10,20)", stats.AvgMergeHours)
	}

	empty, err := repo.AggregateMergeHoursForIdentities(ctx, prOrgA, nil, from, to)
	if err != nil {
		t.Fatalf("empty cohort: %v", err)
	}
	if empty.PRCount != 0 || empty.AvgMergeHours != 0 {
		t.Errorf("empty cohort = %+v, want zero (no signal)", empty)
	}

	// A cohort with no merged PR in the window is no-signal, not a zero-hour latency.
	other, err := repo.AggregateMergeHoursForIdentities(ctx, prOrgA, []string{prIdent2}, from, to)
	if err != nil {
		t.Fatalf("other identity: %v", err)
	}
	if other.PRCount != 0 {
		t.Errorf("identity with no PRs = %+v, want PRCount 0", other)
	}
}

func TestPullRequestRepo_PickupAndSize(t *testing.T) {
	tx := openInputTx(t, "0002_vcs_pr_signals.sql")
	repo := NewPullRequestRepo(tx)
	ctx := context.Background()
	now := time.Now().UTC()
	from, to := now.Add(-24*time.Hour), now.Add(time.Hour)

	// opened 2h ago, first review 1h ago → 1h pickup; a large PR (550 LOC) and a small one (150 LOC).
	insertPR(t, tx, "b0000000-0000-0000-0000-000000000001", now, 2, 0, 1, 300, 250, 2, "")
	insertPR(t, tx, "b0000000-0000-0000-0000-000000000002", now, 3, 0, 2, 100, 50, 0, "")

	pickup, err := repo.AggregatePickupTime(ctx, prOrgA, from, to, prFilter())
	if err != nil {
		t.Fatalf("AggregatePickupTime: %v", err)
	}
	if pickup.PRCount != 2 || pickup.HoursP50 == nil {
		t.Fatalf("pickup = %+v, want PRCount 2 and a non-nil p50", pickup)
	}

	size, err := repo.AggregatePRSize(ctx, prOrgA, from, to, prFilter())
	if err != nil {
		t.Fatalf("AggregatePRSize: %v", err)
	}
	if size.PRCount != 2 || size.LargePRCount != 1 {
		t.Errorf("size = %+v, want PRCount 2, LargePRCount 1 (550 LOC > 500)", size)
	}
	if size.LOCSum == nil || *size.LOCSum != 700 {
		t.Errorf("LOCSum = %v, want 700 (550 + 150)", size.LOCSum)
	}

	// The repo filter excludes a PR of a different physical repo.
	wrong := WithPullRequestRepoIdentities([]ConnectorRepo{{ConnectorID: "other", PlatformRepoID: "nope"}})
	none, err := repo.AggregatePRSize(ctx, prOrgA, from, to, wrong)
	if err != nil {
		t.Fatalf("filtered size: %v", err)
	}
	if none.PRCount != 0 {
		t.Errorf("filtered-out size = %+v, want PRCount 0", none)
	}
}

func TestPullRequestRepo_AvgLoopsPerPR_NoAISplit(t *testing.T) {
	tx := openInputTx(t, "0002_vcs_pr_signals.sql")
	repo := NewPullRequestRepo(tx)
	ctx := context.Background()
	now := time.Now().UTC()
	from, to := now.Add(-24*time.Hour), now.Add(time.Hour)

	insertPR(t, tx, "c0000000-0000-0000-0000-000000000001", now, 5, 2, 4, 100, 0, 2, "") // loops 3
	insertPR(t, tx, "c0000000-0000-0000-0000-000000000002", now, 5, 2, 4, 100, 0, 0, "") // loops 1

	loops, err := repo.AggregateAvgLoopsPerPR(ctx, prOrgA, from, to, nil, prFilter())
	if err != nil {
		t.Fatalf("AggregateAvgLoopsPerPR: %v", err)
	}
	if loops.NonAIPRCount != 2 || loops.AvgNonAI != 2.0 {
		t.Errorf("loops = %+v, want AvgNonAI 2.0 / NonAIPRCount 2 (mean of 3,1)", loops)
	}
	// no PR-attribution keys, so the AI bucket is always empty — even if keys are
	// passed, there is no AI split.
	if loops.AIPRCount != 0 || loops.AvgAI != 0 {
		t.Errorf("AI bucket = count %d avg %v, want empty (no AI split)", loops.AIPRCount, loops.AvgAI)
	}
	withKeys, err := repo.AggregateAvgLoopsPerPR(ctx, prOrgA, from, to, []string{"some/repo#PR-1"}, prFilter())
	if err != nil {
		t.Fatalf("AggregateAvgLoopsPerPR with keys: %v", err)
	}
	if withKeys.AIPRCount != 0 {
		t.Errorf("AI bucket with keys = %d, want 0 (keys are inert)", withKeys.AIPRCount)
	}
}

func TestPullRequestRepo_PerModuleChurn(t *testing.T) {
	tx := openInputTx(t, "0002_vcs_pr_signals.sql")
	repo := NewPullRequestRepo(tx)
	ctx := context.Background()
	now := time.Now().UTC()
	from, to := now.Add(-24*time.Hour), now.Add(time.Hour)

	files := `[{"path":"src/a.go","additions":10,"deletions":5},{"path":"src/b.go","additions":3,"deletions":0},{"path":"README.md","additions":2,"deletions":0}]`
	insertPR(t, tx, "d0000000-0000-0000-0000-000000000001", now, 5, 2, 4, 20, 5, 1, files)
	// A non-array files value must coerce to [] (no churn), not error.
	insertPR(t, tx, "d0000000-0000-0000-0000-000000000002", now, 5, 2, 4, 1, 0, 0, `"not-an-array"`)

	rows, err := repo.AggregatePerModuleChurn(ctx, prOrgA, prRepoID, from, to)
	if err != nil {
		t.Fatalf("AggregatePerModuleChurn: %v", err)
	}
	byModule := map[string]int64{}
	for _, row := range rows {
		byModule[row.Module] = row.ChurnLines
	}
	if byModule["src"] != 18 { // (10+5) + (3+0)
		t.Errorf("src churn = %d, want 18", byModule["src"])
	}
	if byModule["(root)"] != 2 {
		t.Errorf("(root) churn = %d, want 2 (README.md)", byModule["(root)"])
	}
}

func TestPullRequestRepo_BugSignal_ClosureJoin(t *testing.T) {
	tx := openInputTx(t, "0002_vcs_pr_signals.sql")
	repo := NewPullRequestRepo(tx)
	ctx := context.Background()
	now := time.Now().UTC()
	loadSchemaFile(t, tx, "0003_pm_signals.sql")
	from, to := now.Add(-24*time.Hour), now.Add(time.Hour)

	// A merged PR in the repo that closes a high-priority bug.
	prID := "e0000000-0000-0000-0000-000000000001"
	insertPR(t, tx, prID, now, 5, 2, 4, 50, 10, 1, "")

	const insIssue = `INSERT INTO pm_issues (id, org_id, connector_id, issue_type, priority, labels, issue_created_at)
		VALUES (?, ?, ?, ?, ?, ?::jsonb, ?)`
	bugID := "e1000000-0000-0000-0000-000000000001"
	taskID := "e1000000-0000-0000-0000-000000000002"
	labelBugID := "e1000000-0000-0000-0000-000000000003"
	unlinkedBugID := "e1000000-0000-0000-0000-000000000004"
	mustExec := func(sql string, args ...any) {
		if err := tx.Exec(sql, args...).Error; err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
	mustExec(insIssue, bugID, prOrgA, "jira", "bug", "high", `[]`, now.Add(-2*time.Hour))                   // native bug, high prio
	mustExec(insIssue, taskID, prOrgA, "jira", "task", "low", `[]`, now.Add(-2*time.Hour))                  // not a bug → excluded
	mustExec(insIssue, labelBugID, prOrgA, "github", "story", "low", `["kind/bug"]`, now.Add(-2*time.Hour)) // github label bug
	mustExec(insIssue, unlinkedBugID, prOrgA, "jira", "bug", "low", `[]`, now.Add(-2*time.Hour))            // bug but no closure link

	const insCand = `INSERT INTO pr_issue_mapping_candidates (id, org_id, entity_type, entity_id, other_entity_id, closes_issue)
		VALUES (?, ?, ?, ?, ?, ?)`
	// bug closed by the merged PR (PM_ISSUE → PR direction).
	mustExec(insCand, "e2000000-0000-0000-0000-000000000001", prOrgA, "ENTITY_TYPE_PM_ISSUE", bugID, prID, true)
	// github label-bug closed by the PR (PR → PM_ISSUE direction).
	mustExec(insCand, "e2000000-0000-0000-0000-000000000002", prOrgA, "ENTITY_TYPE_PULL_REQUEST", prID, labelBugID, true)
	// task linked but it is not a bug → still excluded by the bug-type filter.
	mustExec(insCand, "e2000000-0000-0000-0000-000000000003", prOrgA, "ENTITY_TYPE_PM_ISSUE", taskID, prID, true)
	// a references-only (closes_issue=false) link for the unlinked bug → excluded.
	mustExec(insCand, "e2000000-0000-0000-0000-000000000004", prOrgA, "ENTITY_TYPE_PM_ISSUE", unlinkedBugID, prID, false)

	bug, err := repo.AggregateBugSignal(ctx, prOrgA, prRepoID, from, to)
	if err != nil {
		t.Fatalf("AggregateBugSignal: %v", err)
	}
	if bug.BugCount != 2 {
		t.Errorf("BugCount = %d, want 2 (native bug + github label bug; task + references-only excluded)", bug.BugCount)
	}
	if bug.HighPriority != 1 {
		t.Errorf("HighPriority = %d, want 1 (only the native bug is high)", bug.HighPriority)
	}
}
