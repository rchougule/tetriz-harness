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

// SessionInsightDAO reads the per-session judged-quality aggregates off the flat session_analyses
// table. It is the reshape of a host application's session_analyses reader: that table carried fat
// JSONB blobs plus ~45 STORED generated columns to avoid detoasting them; the session-analysis
// producer this engine reads already flattens the same signals into plain typed columns (scores,
// volume, cost) plus a few wrapped-proto JSONB columns (subagent_usage, languages, friction_counts,
// invoked_skills). So every aggregate below reads flat columns directly — no gen-col accessor, no
// lateral detoast machinery. Single-tenant: the org is bound in the query text, and every matching
// record is read.
//
// Only a subset of ISessionInsightDAO is implemented in this phase (AggregateByOrg,
// AggregateDimensionsByOrg, AggregateInvokedSkillBehavior); the remaining readers land in the
// DAO-impl continuation, at which point this type satisfies the full interface.
type SessionInsightDAO struct {
	db *gorm.DB
}

// NewSessionInsightDAO builds the reader over db.
func NewSessionInsightDAO(db *gorm.DB) *SessionInsightDAO {
	return &SessionInsightDAO{db: db}
}

// baseQuery scopes the session_analyses read to the org and the [from, to] conversation window,
// excluding soft-deleted rows, then applies the caller's filters. The window compares the flat
// conversation_started_at timestamp directly.
func (r *SessionInsightDAO) baseQuery(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) *gorm.DB {
	q := r.db.WithContext(ctx).
		Table("session_analyses").
		Where("organization_id = ? AND conversation_started_at >= ? AND conversation_started_at <= ?", orgID, from, to).
		Where("deleted_at IS NULL")
	for _, filter := range filters {
		if filter != nil {
			q = filter.ApplyInGorm(q)
		}
	}
	return q
}

// AggregateByOrg returns the org-grain session rollup over the window. It averages and sums the flat
// session_analyses columns directly — score_overall, total_prompts, wasted_tokens, total_tokens,
// estimated_cost_usd, iteration_count, effective_context_usage_percent, cache_hit_rate_percent,
// compaction_count, and the subagent_usage JSONB (for its cost field). total_cost_usd is the
// waste-attributed dollar value (wasted-token fraction × the parent's own cost, net of subagent
// cost), total_raw_cost_usd the true spend. high_quality_pct is the % of sessions scoring ≥ 7.0.
func (r *SessionInsightDAO) AggregateByOrg(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) (*model.SessionOrgSummary, error) {
	if orgID == "" {
		return nil, fmt.Errorf("aggregating org summary: orgID cannot be empty")
	}
	var result model.SessionOrgSummary
	// parentCost nets the subagent cost out of the session's own estimated cost, so the
	// waste-attributed total counts only the parent conversation's spend.
	const parentCost = "GREATEST(COALESCE(estimated_cost_usd::float, 0) - COALESCE((subagent_usage->>'totalEstimatedCostUsd')::float, 0), 0)"
	err := r.baseQuery(ctx, orgID, from, to, filters...).
		Select("COALESCE(AVG(score_overall::float), 0) AS avg_score, " +
			"COALESCE(SUM(total_prompts::int), 0) AS total_prompts, " +
			"CASE WHEN COUNT(*) > 0 THEN " +
			"  ROUND((COUNT(*) FILTER (WHERE score_overall::float >= 7.0)::float / COUNT(*)::float * 100)::numeric, 1)::float " +
			"ELSE 0 END AS high_quality_pct, " +
			"COUNT(*) AS total_conversations, " +
			"COALESCE(SUM(wasted_tokens::bigint), 0) AS total_wasted_tokens, " +
			"COALESCE(SUM(total_tokens::bigint), 0) AS total_tokens, " +
			"COALESCE(SUM(CASE WHEN total_tokens::float > 0 " +
			"  THEN wasted_tokens::float / total_tokens::float * " + parentCost + " " +
			"  ELSE 0 END), 0) AS total_cost_usd, " +
			"COALESCE(SUM(estimated_cost_usd::float), 0) AS total_raw_cost_usd, " +
			"COALESCE(AVG(NULLIF(iteration_count::float, 0)), 0) AS avg_iterations_per_user_per_session, " +
			"COALESCE(AVG(effective_context_usage_percent::float), 0) AS avg_context_usage_pct, " +
			"COALESCE(AVG(cache_hit_rate_percent::float), 0) AS avg_cache_hit_pct, " +
			"COALESCE(AVG(compaction_count::float), 0) AS avg_compactions, " +
			"COUNT(DISTINCT person_id) AS active_person_count").
		Scan(&result).Error
	if err != nil {
		return nil, fmt.Errorf("aggregating org summary for org %s: %w", orgID, err)
	}
	return &result, nil
}

// AggregateDimensionsByOrg returns the org-grain mean per quality dimension — the average of the six
// flat score_* columns (score_clarity, score_specificity, score_context_provision, score_actionability,
// score_completeness, score_efficiency).
func (r *SessionInsightDAO) AggregateDimensionsByOrg(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) (*model.SessionDimensionScores, error) {
	if orgID == "" {
		return nil, fmt.Errorf("aggregating dimension scores: orgID cannot be empty")
	}
	var result model.SessionDimensionScores
	err := r.baseQuery(ctx, orgID, from, to, filters...).
		Select("COALESCE(AVG(score_clarity::float), 0) AS clarity, " +
			"COALESCE(AVG(score_specificity::float), 0) AS specificity, " +
			"COALESCE(AVG(score_context_provision::float), 0) AS context_provision, " +
			"COALESCE(AVG(score_actionability::float), 0) AS actionability, " +
			"COALESCE(AVG(score_completeness::float), 0) AS completeness, " +
			"COALESCE(AVG(score_efficiency::float), 0) AS efficiency").
		Scan(&result).Error
	if err != nil {
		return nil, fmt.Errorf("aggregating dimension scores for org %s: %w", orgID, err)
	}
	return &result, nil
}

// invokedSkillBehaviorRow scans one GROUP BY row of the invoked-skill behavioral aggregate.
type invokedSkillBehaviorRow struct {
	Name                   string  `gorm:"column:name"`
	Invocations            int     `gorm:"column:invocations"`
	CompletionRate         float64 `gorm:"column:completion_rate"`
	AvgEffectiveContextUse float64 `gorm:"column:avg_effective_context_use"`
	ContextCoverageN       int     `gorm:"column:context_coverage_n"`
	AvgToolErrors          float64 `gorm:"column:avg_tool_errors"`
	ToolErrorCoverageN     int     `gorm:"column:tool_error_coverage_n"`
}

// AggregateInvokedSkillBehavior returns the per-invoked-skill behavioral aggregate keyed by the raw
// invoked-skill name — the behavioral-attribution bridge's input. It LATERAL-unnests the flat
// invoked_skills JSONB column; the two per-session metrics read effective_context_usage_percent and
// the tool_errors generated column (session_meta_raw ->> 'toolErrors') directly.
//
// The completion-rate band is dropped (this engine's invoked_skills elements carry no `complete`
// field, so a completion rate cannot be measured); CompletionRate is reported as 0, and the
// behavioral detector must not score the completion band off it.
//
// The jsonb_typeof array guard coerces a non-array invoked_skills value to '[]' so one malformed
// row yields zero rows for that session instead of aborting the whole org-wide scan. A tool_errors
// of NULL on a row that HAS session metadata is a measured 0 (proto3 omits a zero), COALESCEd to 0;
// a NULL session_meta_raw (genuinely unmeasured) is excluded from the average, and the coverage
// count COUNT(session_meta_raw) matches that average's denominator exactly.
func (r *SessionInsightDAO) AggregateInvokedSkillBehavior(ctx context.Context, orgID string, from, to time.Time) (map[string]InvokedSkillBehavior, error) {
	if orgID == "" {
		return nil, fmt.Errorf("aggregating invoked-skill behavior: orgID cannot be empty")
	}
	var rows []invokedSkillBehaviorRow
	err := r.db.WithContext(ctx).
		Table("session_analyses").
		Joins("CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(invoked_skills) = 'array' THEN invoked_skills ELSE '[]'::jsonb END) AS s").
		Where("organization_id = ? AND conversation_started_at >= ? AND conversation_started_at <= ? AND deleted_at IS NULL",
			orgID, from, to).
		Where("NULLIF(s->>'name', '') IS NOT NULL").
		Select("s->>'name' AS name, " +
			"COUNT(*) AS invocations, " +
			// Completion band dropped: no 'complete' field's invoked_skills.
			"0.0 AS completion_rate, " +
			"COALESCE(AVG(effective_context_usage_percent::float), 0) AS avg_effective_context_use, " +
			"COUNT(effective_context_usage_percent) AS context_coverage_n, " +
			"COALESCE(AVG(CASE WHEN session_meta_raw IS NOT NULL THEN COALESCE(tool_errors::float, 0) END), 0) AS avg_tool_errors, " +
			"COUNT(session_meta_raw) AS tool_error_coverage_n").
		Group("s->>'name'").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("aggregating invoked-skill behavior for org %s: %w", orgID, err)
	}
	out := make(map[string]InvokedSkillBehavior, len(rows))
	for _, row := range rows {
		out[row.Name] = InvokedSkillBehavior{
			Invocations:            row.Invocations,
			CompletionRate:         row.CompletionRate,
			AvgEffectiveContextUse: row.AvgEffectiveContextUse,
			ContextCoverageN:       row.ContextCoverageN,
			AvgToolErrors:          row.AvgToolErrors,
			ToolErrorCoverageN:     row.ToolErrorCoverageN,
		}
	}
	return out, nil
}
