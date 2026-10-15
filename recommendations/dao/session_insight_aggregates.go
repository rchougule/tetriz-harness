// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// This file carries the remaining ISessionInsightDAO readers (the per-project / per-task-type /
// per-IDE / gap / cost / language / friction / MCP aggregates), split from the core three in
// session_insight_repo.go. Every method is the single-tenant reshape of its source-system reader:
// flat typed columns in place of the g_* generated columns, the wrapped-proto JSONB columns
// (languages/friction_counts/mcp_server_names as {"values":[...]} / object) in place of the raw
// usage_metadata/analysis blobs, is_session_effective in place of the computed effectiveness
// predicate, and the PR-attribution reads DROPPED (this engine has no pr_attribution column, so
// every attributed-/shipping- count is a constant 0).

// sessionRepoBasenameSQL is the per-repo grouping key: the git-remote repo_name, falling back to the
// folder-basename project_name, trailing slash trimmed, last path segment kept — so the same repo in
// differently-named folders collapses to one row.
const sessionRepoBasenameSQL = "substring(rtrim(COALESCE(NULLIF(repo_name, ''), project_name), '/') from '[^/]*$')"

// AggregateSessionsByProject returns the per-project rollup over the window, grouped by the repo
// basename. The attributed-PR / shipping / attributed-cost numerators are dropped to 0, and
// effective_session_count reads the flat
// is_session_effective column; context_saturation_count gates on saturationThresholdPct.
func (r *SessionInsightDAO) AggregateSessionsByProject(ctx context.Context, orgID string, from, to time.Time, saturationThresholdPct float64, filters ...FilterOption) ([]*model.SessionProjectSummary, error) {
	if orgID == "" {
		return nil, fmt.Errorf("aggregating sessions by project: orgID cannot be empty")
	}
	var results []*model.SessionProjectSummary
	selectExpr := sessionRepoBasenameSQL + " AS project_name, " +
		"COUNT(*) AS session_count, " +
		"COUNT(*) FILTER (WHERE is_session_effective) AS effective_session_count, " +
		// Combined parent + delegated-child token volume, matching total_cost_usd's folded shape.
		"COALESCE(SUM(total_tokens::bigint + COALESCE((subagent_usage->>'totalTokens')::bigint, 0)), 0) AS total_tokens, " +
		"COALESCE(AVG(COALESCE(weighted_score::float, score_overall::float)), 0) AS avg_weighted_score, " +
		"COALESCE(SUM(estimated_cost_usd::float), 0) AS total_cost_usd, " +
		"COALESCE(SUM(files_edited_count::int), 0) AS files_edited_count, " +
		"COALESCE(AVG(effective_ai_code_percent::float), 0) AS ai_code_pct_weighted, " +
		"COALESCE(SUM(effective_active_duration_s::bigint), 0) AS total_active_duration_s, " +
		"0 AS attributed_pr_count, " + // no pr_attribution column
		"COUNT(*) FILTER (WHERE estimated_cost_usd::float > 0) AS sessions_with_cost_count, " +
		"COUNT(*) FILTER (WHERE effective_context_usage_percent::float > ?) AS context_saturation_count, " +
		"COUNT(DISTINCT person_id) AS contributor_count, " +
		// NULL when the bucket has no scored session (distinct from a genuine 0) — *float64.
		"AVG(score_overall::float) AS avg_overall_score, " +
		"0 AS shipping_active_duration_s, " +
		"0 AS attributed_cost_usd"
	err := r.baseQuery(ctx, orgID, from, to, filters...).
		Where(sessionRepoBasenameSQL+" IS NOT NULL AND "+sessionRepoBasenameSQL+" != ''").
		Select(selectExpr, saturationThresholdPct).
		Group(sessionRepoBasenameSQL).
		Order("session_count DESC, project_name ASC").
		Scan(&results).Error
	if err != nil {
		return nil, fmt.Errorf("aggregating sessions by project for org %s: %w", orgID, err)
	}
	return results, nil
}

// AggregateByTaskType returns the per-task-type rollup. A NULL/empty task_type folds to
// TASK_TYPE_UNSPECIFIED so no scored session is dropped; the six dimension means come from the flat
// score_* columns.
func (r *SessionInsightDAO) AggregateByTaskType(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) ([]*model.SessionTaskTypeSummary, error) {
	if orgID == "" {
		return nil, fmt.Errorf("aggregating by task type: orgID cannot be empty")
	}
	const taskExpr = "COALESCE(NULLIF(task_type, ''), 'TASK_TYPE_UNSPECIFIED')"
	var results []*model.SessionTaskTypeSummary
	err := r.baseQuery(ctx, orgID, from, to, filters...).
		Select(taskExpr + " AS task_type, " +
			"COUNT(*) AS sessions, " +
			"COALESCE(AVG(score_overall::float), 0) AS avg_overall, " +
			"COALESCE(AVG(score_clarity::float), 0) AS clarity, " +
			"COALESCE(AVG(score_specificity::float), 0) AS specificity, " +
			"COALESCE(AVG(score_context_provision::float), 0) AS context_provision, " +
			"COALESCE(AVG(score_actionability::float), 0) AS actionability, " +
			"COALESCE(AVG(score_completeness::float), 0) AS completeness, " +
			"COALESCE(AVG(score_efficiency::float), 0) AS efficiency").
		Group(taskExpr).
		Order("sessions DESC, task_type ASC").
		Scan(&results).Error
	if err != nil {
		return nil, fmt.Errorf("aggregating by task type for org %s: %w", orgID, err)
	}
	return results, nil
}

// AggregateByIDE returns the per-IDE session count + mean overall score. A NULL/empty ide_type folds
// to IDE_TYPE_UNSPECIFIED.
func (r *SessionInsightDAO) AggregateByIDE(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) ([]*model.SessionIDESummary, error) {
	if orgID == "" {
		return nil, fmt.Errorf("aggregating by IDE: orgID cannot be empty")
	}
	const ideExpr = "COALESCE(NULLIF(ide_type, ''), 'IDE_TYPE_UNSPECIFIED')"
	var results []*model.SessionIDESummary
	err := r.baseQuery(ctx, orgID, from, to, filters...).
		Select(ideExpr + " AS ide, " +
			"COUNT(*) AS sessions, " +
			"COALESCE(AVG(score_overall::float), 0) AS avg_overall").
		Group(ideExpr).
		Order("sessions DESC, ide ASC").
		Scan(&results).Error
	if err != nil {
		return nil, fmt.Errorf("aggregating by IDE for org %s: %w", orgID, err)
	}
	return results, nil
}

// AggregateHighVsLowGap returns the per-issue low/high session counts and the bucket totals. A
// session is low when score_overall <= lowScoreMax, high when >= highScoreMin; a NULL score is
// 'unscored' and excluded from the totals (the caller's scored-session denominators). Per-issue
// counts unnest the flat taxonomy_issues JSONB (elements carry categorySlug).
func (r *SessionInsightDAO) AggregateHighVsLowGap(ctx context.Context, orgID string, from, to time.Time, lowScoreMax, highScoreMin int, filters ...FilterOption) (*SessionGapBuckets, error) {
	if orgID == "" {
		return nil, fmt.Errorf("aggregating high-vs-low gap: orgID cannot be empty")
	}
	// lowScoreMax / highScoreMin are controlled config ints, inlined — an int carries no injection
	// risk and GORM's Select does not bind cleanly into a CASE.
	bucketExpr := fmt.Sprintf(
		"CASE WHEN score_overall IS NULL THEN 'unscored' "+
			"WHEN score_overall <= %d THEN 'low' "+
			"WHEN score_overall >= %d THEN 'high' ELSE 'mid' END",
		lowScoreMax, highScoreMin,
	)
	inner := func() *gorm.DB {
		return r.baseQuery(ctx, orgID, from, to, filters...).
			Select(bucketExpr + " AS bucket, id, taxonomy_issues")
	}
	db := r.db.WithContext(ctx)

	type bucketCountRow struct {
		Bucket string `gorm:"column:bucket"`
		Cnt    int    `gorm:"column:cnt"`
	}
	var bucketCounts []bucketCountRow
	if err := db.Raw(
		"SELECT bucket, COUNT(*) AS cnt FROM (?) b WHERE bucket IN ('low','mid','high') GROUP BY bucket",
		inner(),
	).Scan(&bucketCounts).Error; err != nil {
		return nil, fmt.Errorf("aggregating high-vs-low bucket totals for org %s: %w", orgID, err)
	}
	result := &SessionGapBuckets{}
	for _, bucket := range bucketCounts {
		switch bucket.Bucket {
		case "low":
			result.TotalLow = bucket.Cnt
		case "mid":
			result.TotalMid = bucket.Cnt
		case "high":
			result.TotalHigh = bucket.Cnt
		}
	}
	if err := db.Raw(`
		SELECT issue->>'categorySlug' AS slug,
		       COUNT(DISTINCT b.id) FILTER (WHERE b.bucket = 'low')  AS low_count,
		       COUNT(DISTINCT b.id) FILTER (WHERE b.bucket = 'high') AS high_count
		FROM (?) b,
		     jsonb_array_elements(
		       CASE WHEN jsonb_typeof(b.taxonomy_issues) = 'array'
		            THEN b.taxonomy_issues ELSE '[]'::jsonb END
		     ) AS issue
		WHERE b.bucket IN ('low','high')
		  AND issue->>'categorySlug' IS NOT NULL AND issue->>'categorySlug' != ''
		GROUP BY issue->>'categorySlug'
	`, inner()).Scan(&result.Issues).Error; err != nil {
		return nil, fmt.Errorf("aggregating high-vs-low issue gap for org %s: %w", orgID, err)
	}
	return result, nil
}

// AggregatePersonScoreBuckets splits the org's persons into low/high cohorts by each person's mean
// score_overall in the window (low when the mean <= lowScoreMax, high when >= highScoreMin). A
// person whose only sessions are unscored is in neither cohort.
func (r *SessionInsightDAO) AggregatePersonScoreBuckets(ctx context.Context, orgID string, from, to time.Time, lowScoreMax, highScoreMin int, filters ...FilterOption) (*PersonScoreBuckets, error) {
	if orgID == "" {
		return nil, fmt.Errorf("aggregating person score buckets: orgID cannot be empty")
	}
	type personAvgRow struct {
		PersonID string  `gorm:"column:person_id"`
		AvgScore float64 `gorm:"column:avg_score"`
	}
	var rows []personAvgRow
	err := r.baseQuery(ctx, orgID, from, to, filters...).
		Where("score_overall IS NOT NULL").
		Select("person_id, AVG(score_overall::float) AS avg_score").
		Group("person_id").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("aggregating person score buckets for org %s: %w", orgID, err)
	}
	buckets := &PersonScoreBuckets{}
	for _, person := range rows {
		switch {
		case person.AvgScore <= float64(lowScoreMax):
			buckets.LowPersonIDs = append(buckets.LowPersonIDs, person.PersonID)
		case person.AvgScore >= float64(highScoreMin):
			buckets.HighPersonIDs = append(buckets.HighPersonIDs, person.PersonID)
		}
	}
	return buckets, nil
}

// AggregateCostByModel returns the per-model cost rollup. The model bucket is the first element of
// the flat models_used JSONB array, folding a missing/blank value to "unknown".
func (r *SessionInsightDAO) AggregateCostByModel(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) ([]*model.AILeverageCostByModel, error) {
	if orgID == "" {
		return nil, fmt.Errorf("aggregating cost by model: orgID cannot be empty")
	}
	const bucket = "COALESCE(NULLIF(models_used->>0, ''), 'unknown')"
	var results []*model.AILeverageCostByModel
	err := r.baseQuery(ctx, orgID, from, to, filters...).
		Select(bucket + " AS model, " +
			"COALESCE(SUM(estimated_cost_usd::float), 0) AS total_cost_usd, " +
			"COUNT(*) AS session_count, " +
			"COUNT(*) FILTER (WHERE is_session_effective) AS effective_session_count").
		Group(bucket).
		Order("total_cost_usd DESC, model ASC").
		Scan(&results).Error
	if err != nil {
		return nil, fmt.Errorf("aggregating cost by model for org %s: %w", orgID, err)
	}
	return results, nil
}

// AggregateMCPBreakdown returns the per-MCP-server distinct-user count, reading the wrapped
// mcp_server_names->'values' array (a STORED generated column). Top 50 by user count.
func (r *SessionInsightDAO) AggregateMCPBreakdown(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) ([]*model.AILeverageMCPBreakdown, error) {
	if orgID == "" {
		return nil, fmt.Errorf("aggregating MCP breakdown: orgID cannot be empty")
	}
	var results []*model.AILeverageMCPBreakdown
	err := r.baseQuery(ctx, orgID, from, to, filters...).
		Joins("CROSS JOIN LATERAL jsonb_array_elements_text(CASE WHEN jsonb_typeof(mcp_server_names->'values') = 'array' THEN mcp_server_names->'values' ELSE '[]'::jsonb END) AS server_name").
		Select("server_name AS server_name, COUNT(DISTINCT person_id) AS user_count").
		Group("server_name").
		Order("user_count DESC, server_name ASC").
		Limit(50).
		Scan(&results).Error
	if err != nil {
		return nil, fmt.Errorf("aggregating MCP breakdown for org %s: %w", orgID, err)
	}
	return results, nil
}

// AggregateSubagentDelegations returns the total subagent delegations over the window, summing
// subagent_usage->>'childCount'.
func (r *SessionInsightDAO) AggregateSubagentDelegations(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) (int, error) {
	if orgID == "" {
		return 0, fmt.Errorf("aggregating subagent delegations: orgID cannot be empty")
	}
	var total int
	err := r.baseQuery(ctx, orgID, from, to, filters...).
		Select("COALESCE(SUM(COALESCE(NULLIF(subagent_usage->>'childCount', '')::int, 0)), 0) AS total").
		Scan(&total).Error
	if err != nil {
		return 0, fmt.Errorf("aggregating subagent delegations for org %s: %w", orgID, err)
	}
	return total, nil
}

// AggregateLanguageBreakdown returns the per-language rollup, unnesting the wrapped
// languages->'values' array; a session touching several languages contributes to each. The
// attributed-PR / shipping / attributed-cost numerators are dropped to 0. criticalPct gates
// the context-saturation count. Top 50 by session count.
func (r *SessionInsightDAO) AggregateLanguageBreakdown(ctx context.Context, orgID string, from, to time.Time, criticalPct float64, filters ...FilterOption) ([]*model.AILeverageLanguageBreakdown, error) {
	if orgID == "" {
		return nil, fmt.Errorf("aggregating language breakdown: orgID cannot be empty")
	}
	selectExpr := "lang AS language, " +
		"COUNT(*) AS session_count, " +
		"COALESCE(AVG(effective_ai_code_percent::float), 0) AS avg_ai_code_pct, " +
		"0 AS attributed_pr_count, " +
		"COUNT(*) FILTER (WHERE effective_context_usage_percent::float > ?) AS context_saturation_count, " +
		"COUNT(*) FILTER (WHERE estimated_cost_usd::float > 0) AS sessions_with_cost_count, " +
		"COUNT(*) FILTER (WHERE is_session_effective) AS effective_session_count, " +
		// NULL when the language bucket has no scored session — *float64.
		"AVG(NULLIF(score_overall::float, 0)) AS avg_overall_score, " +
		"0 AS shipping_active_duration_s, " +
		"0 AS attributed_cost_usd, " +
		"COALESCE(SUM(estimated_cost_usd::float), 0) AS total_cost_usd"
	var results []*model.AILeverageLanguageBreakdown
	err := r.baseQuery(ctx, orgID, from, to, filters...).
		Joins("CROSS JOIN LATERAL jsonb_array_elements_text(CASE WHEN jsonb_typeof(languages->'values') = 'array' THEN languages->'values' ELSE '[]'::jsonb END) AS lang").
		Select(selectExpr, criticalPct).
		Group("lang").
		Order("session_count DESC, lang ASC").
		Limit(50).
		Scan(&results).Error
	if err != nil {
		return nil, fmt.Errorf("aggregating language breakdown for org %s: %w", orgID, err)
	}
	return results, nil
}

// AggregateFrictionCounts returns the friction-type → summed-count map, reading the flat
// friction_counts JSONB object. A non-object value coerces to an empty object inline (so one
// malformed row yields no pairs rather than aborting the scan); a non-numeric count value is 0.
func (r *SessionInsightDAO) AggregateFrictionCounts(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) (map[string]int, error) {
	if orgID == "" {
		return nil, fmt.Errorf("aggregating friction counts: orgID cannot be empty")
	}
	type frictionRow struct {
		FrictionType string `gorm:"column:friction_type"`
		TotalCount   int    `gorm:"column:total_count"`
	}
	var rows []frictionRow
	err := r.baseQuery(ctx, orgID, from, to, filters...).
		Joins("CROSS JOIN LATERAL jsonb_each_text(CASE WHEN jsonb_typeof(friction_counts) = 'object' THEN friction_counts ELSE '{}'::jsonb END) AS kv").
		Select("kv.key AS friction_type, " +
			"SUM(CASE WHEN kv.value ~ '^-{0,1}[0-9]+$' THEN kv.value::int ELSE 0 END) AS total_count").
		Group("kv.key").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("aggregating friction counts for org %s: %w", orgID, err)
	}
	result := make(map[string]int, len(rows))
	for _, friction := range rows {
		if friction.FrictionType != "" && friction.TotalCount > 0 {
			result[friction.FrictionType] = friction.TotalCount
		}
	}
	return result, nil
}

// DistinctMcpServerNames returns the distinct, non-blank MCP server names seen in the window,
// reading the wrapped mcp_server_names->'values' array (a STORED generated column).
func (r *SessionInsightDAO) DistinctMcpServerNames(ctx context.Context, orgID string, from, to time.Time) ([]string, error) {
	if orgID == "" {
		return nil, fmt.Errorf("listing distinct MCP server names: orgID cannot be empty")
	}
	var names []string
	err := r.db.WithContext(ctx).
		Table("session_analyses").
		Joins("CROSS JOIN LATERAL jsonb_array_elements_text(CASE WHEN jsonb_typeof(mcp_server_names->'values') = 'array' THEN mcp_server_names->'values' ELSE '[]'::jsonb END) AS server").
		Where("organization_id = ? AND conversation_started_at >= ? AND conversation_started_at <= ? AND deleted_at IS NULL",
			orgID, from, to).
		Where("NULLIF(TRIM(server), '') IS NOT NULL").
		Distinct().
		Order("server ASC").
		Pluck("server", &names).Error
	if err != nil {
		return nil, fmt.Errorf("listing distinct MCP server names for org %s: %w", orgID, err)
	}
	return names, nil
}

// Compile-time assertion that the reshape reader satisfies the full lean interface.
var _ ISessionInsightDAO = (*SessionInsightDAO)(nil)
