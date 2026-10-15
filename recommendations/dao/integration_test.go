// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"math"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/testsupport"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// These are real-Postgres DAO tests: they prove the session-analyses readers read the flat typed
// columns (and the two JSONB columns) correctly. They run inside one
// transaction that creates the schema, seeds fixtures, and is rolled back at cleanup, so the test is
// rerunnable against the same database. The suite skips only when no test DSN is set; a DSN that is
// set but unusable fails it (testsupport.OpenTestTx). Never put it behind an `integration` build tag,
// which nothing builds.

func openTestTx(t *testing.T) *gorm.DB {
	t.Helper()
	tx := testsupport.OpenTestTx(t)
	loadSchemaFile(t, tx, "0001_session_signals.sql")
	return tx
}

// loadSchemaFile applies a single migration file into the open transaction. It delegates to the
// shared testsupport loader so the dao and engine test suites share one migration-loading codepath.
func loadSchemaFile(t *testing.T, tx *gorm.DB, name string) {
	t.Helper()
	testsupport.LoadMigrations(t, tx, name)
}

const (
	testOrgA = "11111111-1111-1111-1111-111111111111"
	testOrgB = "22222222-2222-2222-2222-222222222222"
	testP1   = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa1"
	testP2   = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa2"
	testRR1  = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb1"
	testRR2  = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb2"
	testRR3  = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb3"
)

// seedSessionAnalyses inserts three sessions: two for org A (one high-quality + effective, one not;
// both run the same skill and the same repo, with distinct IDEs/languages) and one for org B (which
// every org-A aggregate must exclude). The fixtures exercise the flat scalar columns, the generated
// tool_errors + mcp_server_names columns (off session_meta_raw), and the JSONB LATERAL unnests
// (invoked_skills, languages->'values', friction_counts object, taxonomy_issues).
func seedSessionAnalyses(t *testing.T, tx *gorm.DB, now time.Time) {
	t.Helper()
	const insert = `INSERT INTO session_analyses
		(organization_id, person_id, raw_record_id, conversation_started_at, ide_type, task_type,
		 score_overall, effective_context_usage_percent, effective_ai_code_percent, is_session_effective,
		 total_tokens, wasted_tokens, estimated_cost_usd, iteration_count,
		 repo_name, project_name, invoked_skills, languages, friction_counts, models_used,
		 taxonomy_issues, session_meta_raw)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?::jsonb, ?::jsonb, ?::jsonb, ?::jsonb, ?::jsonb, ?::jsonb)`
	rows := []struct {
		org, person, rr, ide, taskType         string
		score                                  int
		ctxPct, aiCodePct                      float64
		effective                              bool
		totalTokens, wastedTokens              int64
		cost                                   float64
		iters                                  int
		repo, project, skills, langs, friction string
		models, taxonomy, meta                 string
	}{
		{testOrgA, testP1, testRR1, "IDE_TYPE_CLAUDE_CODE", "debug", 8, 50, 40, true, 1000, 100, 2.0, 3,
			"acme/api", "api", `[{"name":"/spec-driven-dev"}]`, `{"values":["Go","TypeScript"]}`,
			`{"context_loss":"2","tool_errors":"1"}`, `["claude-sonnet-4"]`, `[{"categorySlug":"error-handling"}]`,
			`{"toolErrors":"2","mcpServerNames":["github","filesystem"]}`},
		{testOrgA, testP2, testRR2, "IDE_TYPE_CURSOR", "test", 6, 70, 20, false, 2000, 0, 1.0, 1,
			"acme/api", "api", `[{"name":"/spec-driven-dev"}]`, `{"values":["Go"]}`,
			`{"context_loss":"3"}`, `["claude-sonnet-4"]`, `[{"categorySlug":"testing"}]`,
			`{"mcpServerNames":["github"]}`},
		{testOrgB, testP1, testRR3, "IDE_TYPE_CLAUDE_CODE", "debug", 1, 10, 5, false, 500, 400, 5.0, 9,
			"other/repo", "repo", `[{"name":"/other"}]`, `{"values":["Rust"]}`,
			`{"context_loss":"9"}`, `["other-model"]`, `[{"categorySlug":"security"}]`,
			`{"toolErrors":"9","mcpServerNames":["other"]}`},
	}
	for _, r := range rows {
		if err := tx.Exec(insert, r.org, r.person, r.rr, now, r.ide, r.taskType, r.score, r.ctxPct,
			r.aiCodePct, r.effective, r.totalTokens, r.wastedTokens, r.cost, r.iters, r.repo, r.project,
			r.skills, r.langs, r.friction, r.models, r.taxonomy, r.meta).Error; err != nil {
			t.Fatalf("seeding session_analyses: %v", err)
		}
	}
}

func seedSecurityFlags(t *testing.T, tx *gorm.DB, now time.Time) {
	t.Helper()
	const insert = `INSERT INTO security_flags
		(organization_id, person_id, raw_record_id, flag_type, status, flag_date)
		VALUES (?, ?, ?, ?, ?, ?)`
	rows := []struct{ org, flagType, status string }{
		{testOrgA, "api-key-leak", "open"},
		{testOrgA, "api-key-leak", "open"},
		{testOrgA, "pii-injection", "resolved"}, // excluded: not open
		{testOrgB, "api-key-leak", "open"},      // excluded: other org
	}
	for _, r := range rows {
		if err := tx.Exec(insert, r.org, testP1, testRR1, r.flagType, r.status, now).Error; err != nil {
			t.Fatalf("seeding security_flags: %v", err)
		}
	}
}

func approxEqual(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestSessionInsightDAO_AggregateByOrg_Reshape(t *testing.T) {
	tx := openTestTx(t)
	now := time.Now().UTC()
	seedSessionAnalyses(t, tx, now)
	from, to := now.Add(-24*time.Hour), now.Add(24*time.Hour)

	got, err := NewSessionInsightDAO(tx).AggregateByOrg(context.Background(), testOrgA, from, to)
	if err != nil {
		t.Fatalf("AggregateByOrg: %v", err)
	}
	if !approxEqual(got.AvgScore, 7.0) {
		t.Errorf("AvgScore = %v, want 7.0 (mean of 8,6 — org B's 1 excluded)", got.AvgScore)
	}
	if got.TotalConversations != 2 {
		t.Errorf("TotalConversations = %d, want 2", got.TotalConversations)
	}
	if !approxEqual(got.HighQualityPct, 50.0) {
		t.Errorf("HighQualityPct = %v, want 50.0 (one of two sessions scores >= 7)", got.HighQualityPct)
	}
	if got.ActivePersonCount != 2 {
		t.Errorf("ActivePersonCount = %d, want 2", got.ActivePersonCount)
	}
	if got.TotalTokens != 3000 {
		t.Errorf("TotalTokens = %d, want 3000", got.TotalTokens)
	}
	if got.TotalWastedTokens != 100 {
		t.Errorf("TotalWastedTokens = %d, want 100", got.TotalWastedTokens)
	}
	if !approxEqual(got.TotalRawCostUSD, 3.0) {
		t.Errorf("TotalRawCostUSD = %v, want 3.0 (2.0 + 1.0)", got.TotalRawCostUSD)
	}
}

func TestSessionInsightDAO_AggregateInvokedSkillBehavior_Reshape(t *testing.T) {
	tx := openTestTx(t)
	now := time.Now().UTC()
	seedSessionAnalyses(t, tx, now)
	from, to := now.Add(-24*time.Hour), now.Add(24*time.Hour)

	got, err := NewSessionInsightDAO(tx).AggregateInvokedSkillBehavior(context.Background(), testOrgA, from, to)
	if err != nil {
		t.Fatalf("AggregateInvokedSkillBehavior: %v", err)
	}
	skill, ok := got["/spec-driven-dev"]
	if !ok {
		t.Fatalf("expected skill /spec-driven-dev in result, got keys %v", keysOf(got))
	}
	if skill.Invocations != 2 {
		t.Errorf("Invocations = %d, want 2", skill.Invocations)
	}
	if !approxEqual(skill.AvgEffectiveContextUse, 60.0) {
		t.Errorf("AvgEffectiveContextUse = %v, want 60.0 (mean of 50,70)", skill.AvgEffectiveContextUse)
	}
	if skill.ContextCoverageN != 2 {
		t.Errorf("ContextCoverageN = %d, want 2", skill.ContextCoverageN)
	}
	// tool_errors is the generated column off session_meta_raw: row1 "2", row2 present-but-absent
	// (measured 0, proto3-omit). Mean = 1.0, coverage = 2 (both have session metadata).
	if !approxEqual(skill.AvgToolErrors, 1.0) {
		t.Errorf("AvgToolErrors = %v, want 1.0", skill.AvgToolErrors)
	}
	if skill.ToolErrorCoverageN != 2 {
		t.Errorf("ToolErrorCoverageN = %d, want 2", skill.ToolErrorCoverageN)
	}
	// Completion band dropped: no 'complete' field's invoked_skills.
	if !approxEqual(skill.CompletionRate, 0.0) {
		t.Errorf("CompletionRate = %v, want 0.0 (dropped band)", skill.CompletionRate)
	}
	if _, leaked := got["/other"]; leaked {
		t.Error("org B's skill /other leaked into org A's result")
	}
}

func TestSecurityFlagDAO_CountByFlagType_Reshape(t *testing.T) {
	tx := openTestTx(t)
	now := time.Now().UTC()
	seedSecurityFlags(t, tx, now)
	from, to := now.Add(-24*time.Hour), now.Add(24*time.Hour)

	got, err := NewSecurityFlagDAO(tx).CountByFlagType(context.Background(), testOrgA, from, to)
	if err != nil {
		t.Fatalf("CountByFlagType: %v", err)
	}
	if got["api-key-leak"] != 2 {
		t.Errorf("api-key-leak count = %d, want 2 (two open org-A flags)", got["api-key-leak"])
	}
	if _, present := got["pii-injection"]; present {
		t.Error("resolved pii-injection flag was counted; only status='open' must count")
	}
	if len(got) != 1 {
		t.Errorf("result has %d flag types, want 1 (org B's open flag must be excluded)", len(got))
	}
}

func orgAWindow(now time.Time) (from, to time.Time) {
	return now.Add(-24 * time.Hour), now.Add(24 * time.Hour)
}

func TestSessionInsightDAO_AggregateSessionsByProject_Reshape(t *testing.T) {
	tx := openTestTx(t)
	now := time.Now().UTC()
	seedSessionAnalyses(t, tx, now)
	from, to := orgAWindow(now)

	got, err := NewSessionInsightDAO(tx).AggregateSessionsByProject(context.Background(), testOrgA, from, to, 60.0)
	if err != nil {
		t.Fatalf("AggregateSessionsByProject: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d project rows, want 1 (both org-A sessions share repo basename 'api')", len(got))
	}
	p := got[0]
	if p.ProjectName != "api" {
		t.Errorf("ProjectName = %q, want 'api' (basename of acme/api)", p.ProjectName)
	}
	if p.SessionCount != 2 {
		t.Errorf("SessionCount = %d, want 2", p.SessionCount)
	}
	if p.EffectiveSessionCount != 1 {
		t.Errorf("EffectiveSessionCount = %d, want 1 (only row1 is_session_effective)", p.EffectiveSessionCount)
	}
	if p.ContextSaturationCount != 1 {
		t.Errorf("ContextSaturationCount = %d, want 1 (only row2's 70%% > 60%% threshold)", p.ContextSaturationCount)
	}
	if p.ContributorCount != 2 {
		t.Errorf("ContributorCount = %d, want 2", p.ContributorCount)
	}
	if p.AttributedPRCount != 0 {
		t.Errorf("AttributedPRCount = %d, want 0 (PR attribution dropped)", p.AttributedPRCount)
	}
	if p.AvgOverallScore == nil || !approxEqual(*p.AvgOverallScore, 7.0) {
		t.Errorf("AvgOverallScore = %v, want 7.0", p.AvgOverallScore)
	}
}

func TestSessionInsightDAO_AggregateLanguageBreakdown_Reshape(t *testing.T) {
	tx := openTestTx(t)
	now := time.Now().UTC()
	seedSessionAnalyses(t, tx, now)
	from, to := orgAWindow(now)

	got, err := NewSessionInsightDAO(tx).AggregateLanguageBreakdown(context.Background(), testOrgA, from, to, 60.0)
	if err != nil {
		t.Fatalf("AggregateLanguageBreakdown: %v", err)
	}
	byLang := map[string]*model.AILeverageLanguageBreakdown{}
	for _, row := range got {
		byLang[row.Language] = row
	}
	if byLang["Go"] == nil || byLang["Go"].SessionCount != 2 {
		t.Errorf("Go.SessionCount = %v, want 2 (both org-A sessions list Go)", byLang["Go"])
	}
	if byLang["TypeScript"] == nil || byLang["TypeScript"].SessionCount != 1 {
		t.Errorf("TypeScript.SessionCount = %v, want 1", byLang["TypeScript"])
	}
	if byLang["Go"] != nil && byLang["Go"].ContextSaturationCount != 1 {
		t.Errorf("Go.ContextSaturationCount = %d, want 1 (only row2's 70%% > 60%%)", byLang["Go"].ContextSaturationCount)
	}
	if byLang["Go"] != nil && byLang["Go"].AttributedPRCount != 0 {
		t.Errorf("Go.AttributedPRCount = %d, want 0", byLang["Go"].AttributedPRCount)
	}
	if _, leaked := byLang["Rust"]; leaked {
		t.Error("org B's Rust leaked into org A's language breakdown")
	}
}

func TestSessionInsightDAO_AggregateFrictionCounts_Reshape(t *testing.T) {
	tx := openTestTx(t)
	now := time.Now().UTC()
	seedSessionAnalyses(t, tx, now)
	from, to := orgAWindow(now)

	got, err := NewSessionInsightDAO(tx).AggregateFrictionCounts(context.Background(), testOrgA, from, to)
	if err != nil {
		t.Fatalf("AggregateFrictionCounts: %v", err)
	}
	if got["context_loss"] != 5 {
		t.Errorf("context_loss = %d, want 5 (2 + 3)", got["context_loss"])
	}
	if got["tool_errors"] != 1 {
		t.Errorf("tool_errors = %d, want 1", got["tool_errors"])
	}
}

func TestSessionInsightDAO_MCPServerNames_Reshape(t *testing.T) {
	tx := openTestTx(t)
	now := time.Now().UTC()
	seedSessionAnalyses(t, tx, now)
	from, to := orgAWindow(now)
	dao := NewSessionInsightDAO(tx)

	mcp, err := dao.AggregateMCPBreakdown(context.Background(), testOrgA, from, to)
	if err != nil {
		t.Fatalf("AggregateMCPBreakdown: %v", err)
	}
	byServer := map[string]int{}
	for _, row := range mcp {
		byServer[row.ServerName] = row.UserCount
	}
	if byServer["github"] != 2 {
		t.Errorf("github user_count = %d, want 2 (both persons)", byServer["github"])
	}
	if byServer["filesystem"] != 1 {
		t.Errorf("filesystem user_count = %d, want 1", byServer["filesystem"])
	}

	names, err := dao.DistinctMcpServerNames(context.Background(), testOrgA, from, to)
	if err != nil {
		t.Fatalf("DistinctMcpServerNames: %v", err)
	}
	want := []string{"filesystem", "github"}
	if len(names) != 2 || names[0] != want[0] || names[1] != want[1] {
		t.Errorf("DistinctMcpServerNames = %v, want %v (org B's 'other' excluded)", names, want)
	}
}

func TestSessionInsightDAO_AggregateHighVsLowGap_Reshape(t *testing.T) {
	tx := openTestTx(t)
	now := time.Now().UTC()
	seedSessionAnalyses(t, tx, now)
	from, to := orgAWindow(now)

	got, err := NewSessionInsightDAO(tx).AggregateHighVsLowGap(context.Background(), testOrgA, from, to, 4, 7)
	if err != nil {
		t.Fatalf("AggregateHighVsLowGap: %v", err)
	}
	if got.TotalHigh != 1 || got.TotalMid != 1 || got.TotalLow != 0 {
		t.Errorf("buckets low/mid/high = %d/%d/%d, want 0/1/1 (score 8 high, score 6 mid)",
			got.TotalLow, got.TotalMid, got.TotalHigh)
	}
	var errHandling *SessionIssueGapCount
	for i := range got.Issues {
		if got.Issues[i].Slug == "error-handling" {
			errHandling = &got.Issues[i]
		}
	}
	if errHandling == nil || errHandling.HighCount != 1 || errHandling.LowCount != 0 {
		t.Errorf("error-handling gap = %+v, want HighCount 1 / LowCount 0 (row1's taxonomy, high bucket)", errHandling)
	}
}

func TestSessionInsightDAO_AggregateByIDE_And_CostByModel_Reshape(t *testing.T) {
	tx := openTestTx(t)
	now := time.Now().UTC()
	seedSessionAnalyses(t, tx, now)
	from, to := orgAWindow(now)
	dao := NewSessionInsightDAO(tx)

	ides, err := dao.AggregateByIDE(context.Background(), testOrgA, from, to)
	if err != nil {
		t.Fatalf("AggregateByIDE: %v", err)
	}
	if len(ides) != 2 {
		t.Errorf("got %d IDE rows, want 2 (Claude Code + Cursor)", len(ides))
	}

	models, err := dao.AggregateCostByModel(context.Background(), testOrgA, from, to)
	if err != nil {
		t.Fatalf("AggregateCostByModel: %v", err)
	}
	if len(models) != 1 || models[0].Model != "claude-sonnet-4" {
		t.Fatalf("got %+v, want one bucket claude-sonnet-4", models)
	}
	if !approxEqual(models[0].TotalCostUSD, 3.0) || models[0].SessionCount != 2 || models[0].EffectiveSessionCount != 1 {
		t.Errorf("cost-by-model = cost %v sessions %d effective %d, want 3.0 / 2 / 1",
			models[0].TotalCostUSD, models[0].SessionCount, models[0].EffectiveSessionCount)
	}
}

func keysOf(m map[string]InvokedSkillBehavior) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
