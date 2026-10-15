// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// prStateMerged is the merged state value stored in the pull_requests.state text column.
const prStateMerged = "PR_STATE_MERGED"

// partitionPruneLookbackDays bounds the opened_at lower bound on the merged-window reads: pull_requests
// is conceptually partitioned on opened_at, and a PR can open well before it merges in-window, so the
// lower bound prunes historical partitions while still admitting a long-lived PR that merged in-window.
const partitionPruneLookbackDays = 365

// Bug-signal classification tokens (lower-cased comparisons): a bug-meaning issue_type, the GitHub-only
// bug labels, and the high-priority tokens.
var (
	bugTypeTokens      = []string{"bug"}
	bugLabelTokens     = []string{"bug", "type: bug", "type:bug", "kind/bug", "defect"}
	priorityHighTokens = []string{"high", "critical", "highest", "urgent", "blocker", "p0", "p1"}
)

// PullRequestRepo implements IPullRequestDAO — the DERIVED PR-signal aggregates and the ComputePRCorrelation
// evidence read. Single-tenant: the org is bound in every query, and every matching record is read — the
// bug signal in particular is counted for everyone in the org, never a subset.
type PullRequestRepo struct {
	db *gorm.DB
}

// NewPullRequestRepo builds the repo over db.
func NewPullRequestRepo(db *gorm.DB) *PullRequestRepo {
	return &PullRequestRepo{db: db}
}

var _ IPullRequestDAO = (*PullRequestRepo)(nil)

// applyFilters applies each non-nil FilterOption to the query.
func applyFilters(query *gorm.DB, filters []FilterOption) *gorm.DB {
	for _, filter := range filters {
		if filter != nil {
			query = filter.ApplyInGorm(query)
		}
	}
	return query
}

// lifecycleBaseQuery is the shared base for the per-repo lifecycle aggregates (pickup, pr-size): merged
// PRs opened in [from, to) for the org, with the caller's filters (the physical-repo scope) applied.
func (r *PullRequestRepo) lifecycleBaseQuery(ctx context.Context, orgID string, from, to time.Time, filters []FilterOption) *gorm.DB {
	query := connFromContext(ctx, r.db).
		Table("pull_requests").
		Where("organization_id = ?", orgID).
		Where("state = ?", prStateMerged).
		Where("opened_at >= ? AND opened_at < ?", from, to)
	return applyFilters(query, filters)
}

// AggregateMergeHoursForIdentities returns the mean open→merge latency (hours) and merged-PR count for a
// cohort of platform identities over the window. PRCount == 0 means no merged PR matched (no signal), not
// a literal zero-hour latency. An empty cohort returns a zero-value result (no query).
func (r *PullRequestRepo) AggregateMergeHoursForIdentities(ctx context.Context, orgID string, platformIdentityIDs []string, from, to time.Time) (*PRMergeStats, error) {
	if orgID == "" {
		return nil, errors.New("aggregating merge hours: orgID cannot be empty")
	}
	if len(platformIdentityIDs) == 0 {
		return &PRMergeStats{}, nil
	}
	const sql = `
		SELECT
		  COALESCE(AVG(EXTRACT(EPOCH FROM (pr.merged_at - pr.opened_at)) / 3600.0), 0) AS avg_merge_hours,
		  COUNT(*) AS pr_count
		FROM pull_requests pr
		WHERE pr.organization_id = ?
		  AND pr.deleted_at IS NULL
		  AND pr.state = 'PR_STATE_MERGED'
		  AND pr.merged_at IS NOT NULL
		  AND pr.merged_at >= ? AND pr.merged_at < ?
		  AND pr.opened_at <= ?
		  AND pr.opened_at >= (?::timestamptz - INTERVAL '365 days')
		  AND pr.platform_identity_id IN ?`
	var stats PRMergeStats
	if err := connFromContext(ctx, r.db).
		Raw(sql, orgID, from, to, to, from, platformIdentityIDs).
		Scan(&stats).Error; err != nil {
		return nil, fmt.Errorf("aggregating merge hours for org %s: %w", orgID, err)
	}
	return &stats, nil
}

// AggregatePickupTime returns the open→first-review pickup-wait distribution (hours) over merged PRs
// opened in the window. Pointer fields are NULL when no PR has a first review; the engine reads HoursP50.
func (r *PullRequestRepo) AggregatePickupTime(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) (*model.PickupTimeResult, error) {
	if orgID == "" {
		return nil, errors.New("aggregating pickup time: orgID cannot be empty")
	}
	query := r.lifecycleBaseQuery(ctx, orgID, from, to, filters).Select(`
		PERCENTILE_CONT(0.5) WITHIN GROUP (
			ORDER BY EXTRACT(EPOCH FROM (first_review_at - COALESCE(ready_for_review_at, opened_at))) / 3600
		) FILTER (WHERE first_review_at IS NOT NULL) AS hours_p50,
		AVG(EXTRACT(EPOCH FROM (first_review_at - COALESCE(ready_for_review_at, opened_at))) / 3600)
			FILTER (WHERE first_review_at IS NOT NULL) AS avg_hrs,
		PERCENTILE_CONT(0.95) WITHIN GROUP (
			ORDER BY EXTRACT(EPOCH FROM (first_review_at - COALESCE(ready_for_review_at, opened_at))) / 3600
		) FILTER (WHERE first_review_at IS NOT NULL) AS p95_hrs,
		COUNT(*) FILTER (WHERE first_review_at IS NOT NULL) AS pr_count
	`)
	var result model.PickupTimeResult
	if err := query.Scan(&result).Error; err != nil {
		return nil, fmt.Errorf("aggregate pickup time for org %s: %w", orgID, err)
	}
	return &result, nil
}

// AggregatePRSize returns the P50 / sum / avg / p95 (lines_added + lines_deleted) across merged PRs opened
// in the window, plus the large-PR count (> 500 LOC). Diff-less rows (sum = 0) are excluded so
// metadata-only PRs do not skew the result. Pointer fields are NULL when no PR has a diff.
func (r *PullRequestRepo) AggregatePRSize(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) (*model.PRSizeResult, error) {
	if orgID == "" {
		return nil, errors.New("aggregating pr size: orgID cannot be empty")
	}
	query := r.lifecycleBaseQuery(ctx, orgID, from, to, filters).Select(`
		PERCENTILE_CONT(0.5) WITHIN GROUP (
			ORDER BY (lines_added + lines_deleted)::float8
		) FILTER (WHERE (lines_added + lines_deleted) > 0) AS loc_p50,
		SUM((lines_added + lines_deleted)::float8)
			FILTER (WHERE (lines_added + lines_deleted) > 0) AS loc_sum,
		AVG((lines_added + lines_deleted)::float8)
			FILTER (WHERE (lines_added + lines_deleted) > 0) AS avg_lines,
		PERCENTILE_CONT(0.95) WITHIN GROUP (
			ORDER BY (lines_added + lines_deleted)::float8
		) FILTER (WHERE (lines_added + lines_deleted) > 0) AS p95_lines,
		COUNT(*) FILTER (WHERE (lines_added + lines_deleted) > 500) AS large_pr_count,
		COUNT(*) FILTER (WHERE (lines_added + lines_deleted) > 0) AS pr_count
	`)
	var result model.PRSizeResult
	if err := query.Scan(&result).Error; err != nil {
		return nil, fmt.Errorf("aggregate pr size for org %s: %w", orgID, err)
	}
	return &result, nil
}

// AggregateAvgLoopsPerPR returns the mean review-loops-per-PR (review_cycles + 1) across merged PRs in the
// window. This engine carries no PR-attribution AI keys (session_analyses has no pr_attribution), so
// the AI/non-AI split collapses — aiPRKeys is accepted for interface parity but, being always empty here,
// every merged PR falls in the non-AI partition and AvgNonAI is the repo's overall mean. A non-empty
// aiPRKeys is treated the same (the key-matching columns are not carried), so the AI bucket stays zero.
func (r *PullRequestRepo) AggregateAvgLoopsPerPR(ctx context.Context, orgID string, from, to time.Time, aiPRKeys []string, filters ...FilterOption) (LoopsPerPRResult, error) {
	if orgID == "" {
		return LoopsPerPRResult{}, errors.New("aggregating avg loops per pr: orgID cannot be empty")
	}
	query := connFromContext(ctx, r.db).
		Table("pull_requests").
		Select(`AVG(COALESCE(review_cycles, 0) + 1)::float8 AS avg_loops, COUNT(*) AS pr_count`).
		Where("organization_id = ?", orgID).
		Where("state = ?", prStateMerged).
		Where("merged_at >= ? AND merged_at < ?", from, to).
		Where("opened_at <= ?", to).
		Where("opened_at >= ?", from.AddDate(0, 0, -partitionPruneLookbackDays))
	query = applyFilters(query, filters)
	var row struct {
		AvgLoops *float64 `gorm:"column:avg_loops"`
		PRCount  int64    `gorm:"column:pr_count"`
	}
	if err := query.Scan(&row).Error; err != nil {
		return LoopsPerPRResult{}, fmt.Errorf("aggregate avg loops per pr for org %s: %w", orgID, err)
	}
	result := LoopsPerPRResult{NonAIPRCount: row.PRCount}
	if row.AvgLoops != nil {
		result.AvgNonAI = *row.AvgLoops
	}
	return result, nil
}

// AggregatePerModuleChurn returns per-top-level-module summed changed lines for one repo's MERGED PRs
// opened in the window — the domain-specialist grounding signal for LANDED change. Module is the first
// path segment of each file in files_line_stats ([{path, additions, deletions}]); a root-level file maps
// to '(root)', and a non-array/null value coerces to '[]' before unnest.
func (r *PullRequestRepo) AggregatePerModuleChurn(ctx context.Context, orgID, repositoryID string, from, to time.Time) ([]model.PerModuleChurnRow, error) {
	if orgID == "" || repositoryID == "" {
		return nil, errors.New("per-module churn: orgID and repositoryID required")
	}
	const sql = `
SELECT CASE WHEN position('/' in f ->> 'path') = 0 THEN '(root)' ELSE split_part(f ->> 'path', '/', 1) END AS module,
       COALESCE(SUM(COALESCE((f ->> 'additions')::bigint, 0) + COALESCE((f ->> 'deletions')::bigint, 0)), 0) AS churn_lines
FROM pull_requests pr
CROSS JOIN LATERAL jsonb_array_elements(
    CASE WHEN jsonb_typeof(pr.files_line_stats) = 'array' THEN pr.files_line_stats ELSE '[]'::jsonb END
) AS f
WHERE pr.organization_id = ?
  AND pr.repository_id = ?::uuid
  AND pr.state = 'PR_STATE_MERGED'
  AND pr.opened_at >= ? AND pr.opened_at < ?
  AND pr.platform_identity_id IS NOT NULL
  AND pr.deleted_at IS NULL
  AND COALESCE(f ->> 'path', '') <> ''
GROUP BY module`
	var rows []model.PerModuleChurnRow
	if err := connFromContext(ctx, r.db).Raw(sql, orgID, repositoryID, from, to).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("aggregate per-module churn for repo %s: %w", repositoryID, err)
	}
	return rows, nil
}

// AggregateBugSignal returns the repo-attributed bug rollup for one repo: how many bug-type pm_issues (and
// how many high-priority) a repo's MERGED PR CLOSED in the window. A bug is attributed only via a CLOSURE
// link (closes_issue) to a MERGED PR in either mapping direction. Bug-type is a bug-meaning native
// issue_type OR (GitHub only) a bug label. The window bounds issue_created_at explicitly. Single-tenant:
// a bug read counts every matching bug in the org, so bugs are never undercounted for anyone.
func (r *PullRequestRepo) AggregateBugSignal(ctx context.Context, orgID, repositoryID string, from, to time.Time) (*model.BugSignalResult, error) {
	if orgID == "" || repositoryID == "" {
		return nil, errors.New("bug signal: orgID and repositoryID required")
	}
	const sql = `
SELECT
  COUNT(*) AS bug_count,
  COUNT(*) FILTER (WHERE lower(i.priority) IN ?) AS high_priority
FROM pm_issues i
WHERE i.org_id = ?
  AND i.issue_created_at >= ? AND i.issue_created_at < ?
  AND i.deleted_at IS NULL
  AND ( lower(i.issue_type) IN ?
        OR ( i.connector_id = 'github'
             AND EXISTS (
               SELECT 1 FROM jsonb_array_elements_text(i.labels) lbl WHERE lower(lbl) IN ?
             ) ) )
  AND (
    EXISTS (
      SELECT 1
      FROM pr_issue_mapping_candidates c
      JOIN pull_requests pr
        ON pr.id = c.other_entity_id
       AND pr.organization_id = c.org_id
      WHERE c.org_id = i.org_id
        AND c.entity_type = 'ENTITY_TYPE_PM_ISSUE'
        AND c.entity_id = i.id
        AND c.deleted_at IS NULL
        AND c.closes_issue
        AND pr.repository_id = ?::uuid
        AND pr.deleted_at IS NULL
        AND pr.state = 'PR_STATE_MERGED'
    )
    OR EXISTS (
      SELECT 1
      FROM pr_issue_mapping_candidates c
      JOIN pull_requests pr
        ON pr.id = c.entity_id
       AND pr.organization_id = c.org_id
      WHERE c.org_id = i.org_id
        AND c.entity_type = 'ENTITY_TYPE_PULL_REQUEST'
        AND c.deleted_at IS NULL
        AND c.other_entity_id = i.id
        AND c.closes_issue
        AND pr.repository_id = ?::uuid
        AND pr.deleted_at IS NULL
        AND pr.state = 'PR_STATE_MERGED'
    )
  )`
	var result model.BugSignalResult
	if err := connFromContext(ctx, r.db).
		Raw(sql, priorityHighTokens, orgID, from, to, bugTypeTokens, bugLabelTokens, repositoryID, repositoryID).
		Scan(&result).Error; err != nil {
		return nil, fmt.Errorf("aggregate bug signal for repo %s: %w", repositoryID, err)
	}
	return &result, nil
}
