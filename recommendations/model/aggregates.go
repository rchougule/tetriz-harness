// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package model

// Aggregate scan targets — plain result rows returned by the DAO's GROUP BY aggregates over the
// session, VCS and PM signal tables. They are not table models (no TableName); each field is a
// SELECT column. All primitive-typed, so they carry no external dependency.

// SessionOrgSummary is the org-grain session rollup over the window.
type SessionOrgSummary struct {
	AvgScore float64 `gorm:"column:avg_score"`
	// ScoredConversations is the AVG(overall_score) denominator: rows whose score is present.
	ScoredConversations int     `gorm:"column:scored_conversations"`
	HighQualityPct      float64 `gorm:"column:high_quality_pct"`
	TotalConversations  int     `gorm:"column:total_conversations"`
	TotalWastedTokens   int64   `gorm:"column:total_wasted_tokens"`
	TotalTokens         int64   `gorm:"column:total_tokens"`
	// TotalCostUSD is the waste-attributed cost (dollar value of wasted tokens), not total spend.
	TotalCostUSD float64 `gorm:"column:total_cost_usd"`
	// TotalRawCostUSD is the true total spend — a straight SUM(estimated_cost_usd).
	TotalRawCostUSD                float64 `gorm:"column:total_raw_cost_usd"`
	AvgIterationsPerUserPerSession float64 `gorm:"column:avg_iterations_per_user_per_session"`
	AvgContextUsagePct             float64 `gorm:"column:avg_context_usage_pct"`
	AvgCacheHitPct                 float64 `gorm:"column:avg_cache_hit_pct"`
	AvgCompactions                 float64 `gorm:"column:avg_compactions"`
	// ActivePersonCount is the number of distinct persons with at least one session in the window.
	ActivePersonCount int `gorm:"column:active_person_count"`
}

// SessionDimensionScores is the org-grain mean per quality dimension.
type SessionDimensionScores struct {
	Clarity          float64 `gorm:"column:clarity"`
	Specificity      float64 `gorm:"column:specificity"`
	ContextProvision float64 `gorm:"column:context_provision"`
	Actionability    float64 `gorm:"column:actionability"`
	Completeness     float64 `gorm:"column:completeness"`
	Efficiency       float64 `gorm:"column:efficiency"`
}

// SessionIDESummary is one bucket of the per-IDE breakdown.
type SessionIDESummary struct {
	Ide        string  `gorm:"column:ide"`
	Sessions   int     `gorm:"column:sessions"`
	AvgOverall float64 `gorm:"column:avg_overall"`
}

// SessionProjectSummary is one project bucket's rollup. The AttributedPR* counts are session-scoped
// (not distinct PR counts); they are zero when PR attribution is not available.
type SessionProjectSummary struct {
	ProjectName            string  `gorm:"column:project_name"`
	SessionCount           int     `gorm:"column:session_count"`
	EffectiveSessionCount  int     `gorm:"column:effective_session_count"`
	TotalTokens            int64   `gorm:"column:total_tokens"`
	AvgWeightedScore       float64 `gorm:"column:avg_weighted_score"`
	TotalCostUSD           float64 `gorm:"column:total_cost_usd"`
	FilesEditedCount       int     `gorm:"column:files_edited_count"`
	AiCodePctWeighted      float64 `gorm:"column:ai_code_pct_weighted"`
	AttributedPRCount      int     `gorm:"column:attributed_pr_count"`
	TotalActiveDurationS   int64   `gorm:"column:total_active_duration_s"`
	SessionsWithCostCount  int     `gorm:"column:sessions_with_cost_count"`
	ContextSaturationCount int     `gorm:"column:context_saturation_count"`
	ContributorCount       int     `gorm:"column:contributor_count"`
	// AvgOverallScore is nil when the bucket has no scored sessions (distinct from a genuine 0).
	AvgOverallScore         *float64 `gorm:"column:avg_overall_score"`
	ShippingActiveDurationS int64    `gorm:"column:shipping_active_duration_s"`
	AttributedCostUSD       float64  `gorm:"column:attributed_cost_usd"`
}

// AILeverageCostByModel is per-model cost rollup.
type AILeverageCostByModel struct {
	Model                 string  `gorm:"column:model"`
	TotalCostUSD          float64 `gorm:"column:total_cost_usd"`
	SessionCount          int     `gorm:"column:session_count"`
	EffectiveSessionCount int     `gorm:"column:effective_session_count"`
}

// AILeverageLanguageBreakdown is per-language rollup. A session that touches several languages
// contributes to each language's row. AttributedPR* counts are zero when PR attribution is absent.
type AILeverageLanguageBreakdown struct {
	Language                string   `gorm:"column:language"`
	SessionCount            int      `gorm:"column:session_count"`
	AvgAiCodePct            float64  `gorm:"column:avg_ai_code_pct"`
	AttributedPRCount       int      `gorm:"column:attributed_pr_count"`
	ContextSaturationCount  int      `gorm:"column:context_saturation_count"`
	SessionsWithCostCount   int      `gorm:"column:sessions_with_cost_count"`
	EffectiveSessionCount   int      `gorm:"column:effective_session_count"`
	AvgOverallScore         *float64 `gorm:"column:avg_overall_score"`
	ShippingActiveDurationS int64    `gorm:"column:shipping_active_duration_s"`
	AttributedCostUSD       float64  `gorm:"column:attributed_cost_usd"`
	TotalCostUSD            float64  `gorm:"column:total_cost_usd"`
}

// AILeverageMCPBreakdown is per-MCP-server user count.
type AILeverageMCPBreakdown struct {
	ServerName string `gorm:"column:server_name"`
	UserCount  int    `gorm:"column:user_count"`
}

// BugSignalResult is the repo-attributed bug rollup.
type BugSignalResult struct {
	BugCount     int `gorm:"column:bug_count"`
	HighPriority int `gorm:"column:high_priority"`
}

// PRSizeResult is the PR-size distribution over the window. Pointer fields are NULL when no PR has a
// diff; the engine reads LOCP50 / LOCSum.
type PRSizeResult struct {
	LOCP50       *float64 `gorm:"column:loc_p50"`
	LOCSum       *float64 `gorm:"column:loc_sum"`
	AvgLines     *float64 `gorm:"column:avg_lines"`
	P95Lines     *float64 `gorm:"column:p95_lines"`
	LargePRCount int      `gorm:"column:large_pr_count"`
	PRCount      int      `gorm:"column:pr_count"`
}

// PerModuleChurnRow is one top-level module's summed changed lines over the window.
type PerModuleChurnRow struct {
	Module     string `gorm:"column:module"`
	ChurnLines int64  `gorm:"column:churn_lines"`
}

// PickupTimeResult is the open→first-review pickup-wait distribution. Pointer fields are NULL when no
// PR has a first review; the engine reads HoursP50.
type PickupTimeResult struct {
	HoursP50 *float64 `gorm:"column:hours_p50"`
	AvgHrs   *float64 `gorm:"column:avg_hrs"`
	P95Hrs   *float64 `gorm:"column:p95_hrs"`
	PRCount  int      `gorm:"column:pr_count"`
}
