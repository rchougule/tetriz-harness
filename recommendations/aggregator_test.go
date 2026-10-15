// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
	daomocks "github.com/tetriz-ai/tetriz-harness/recommendations/dao/mocks"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/pyclient"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

const aggTestOrgID = "11111111-1111-1111-1111-111111111111"

func testParams() DerivationParams {
	return DerivationParams{LowScoreMax: 3, HighScoreMin: 8, RepoTreeMaxTokens: 0}
}

func TestApplyBenjaminiHochberg(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name              string
		pValues           []float64
		wantPrimary       []bool // q < fdrQThreshold
		wantLowConfidence []bool
	}{
		{
			// Two strongly-significant issues survive FDR as primary; the noise
			// issue does not.
			name:              "strong signals are primary",
			pValues:           []float64{0.001, 0.002, 0.9},
			wantPrimary:       []bool{true, true, false},
			wantLowConfidence: []bool{false, false, false},
		},
		{
			// 0.045 clears raw p<0.05 but its BH q (0.045·3/1 = 0.135) fails FDR →
			// surfaced as low_confidence, not dropped, not primary.
			name:              "marginal signal is low-confidence",
			pValues:           []float64{0.045, 0.5, 0.6},
			wantPrimary:       []bool{false, false, false},
			wantLowConfidence: []bool{true, false, false},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gaps := make([]*promptlibraryv1.IssueGap, len(tc.pValues))
			for i, p := range tc.pValues {
				gaps[i] = &promptlibraryv1.IssueGap{PValue: float32(p)}
			}
			applyBenjaminiHochberg(gaps)
			for i := range tc.pValues {
				assert.Equal(t, tc.wantPrimary[i], gaps[i].GetFdrQ() < fdrQThreshold, "issue %d primary", i)
				assert.Equal(t, tc.wantLowConfidence[i], gaps[i].GetLowConfidence(), "issue %d low_confidence", i)
			}
		})
	}
}

// TestGapsWithFDR_DeterministicOrder asserts gapsWithFDR returns issues in a
// stable order (fdr_q asc, then issue_slug) regardless of the DAO's arrival
// order — the slice is LLM input, so a nondeterministic order would defeat
// prompt caching and make goldens flaky.
func TestGapsWithFDR_DeterministicOrder(t *testing.T) {
	t.Parallel()
	// Fed deliberately unsorted; a-tie/b-tie share counts → identical q (slug
	// breaks the tie), strong-signal has the lowest q, weak-signal the highest.
	buckets := &dao.SessionGapBuckets{
		TotalLow:  100,
		TotalHigh: 100,
		Issues: []dao.SessionIssueGapCount{
			{Slug: "weak-signal", LowCount: 51, HighCount: 49},
			{Slug: "strong-signal", LowCount: 80, HighCount: 10},
			{Slug: "b-tie", LowCount: 60, HighCount: 40},
			{Slug: "a-tie", LowCount: 60, HighCount: 40},
		},
	}
	gaps := gapsWithFDR(buckets)
	require.Len(t, gaps, 4)
	for i := 1; i < len(gaps); i++ {
		prev, cur := gaps[i-1], gaps[i]
		if prev.GetFdrQ() == cur.GetFdrQ() {
			assert.Less(t, prev.GetIssueSlug(), cur.GetIssueSlug(), "equal q must be slug-ordered")
		} else {
			assert.Less(t, prev.GetFdrQ(), cur.GetFdrQ(), "q must be non-decreasing")
		}
	}
	assert.Equal(t, "strong-signal", gaps[0].GetIssueSlug(), "smallest q sorts first")
}

// TestGapsWithFDR_ExcludesInverseGaps asserts only low-cohort weaknesses survive:
// an issue more common among high scorers (inverse) or equally common (no gap) is
// not an anti-pattern to remediate and must be dropped before it can lead the
// q-sorted evidence.
func TestGapsWithFDR_ExcludesInverseGaps(t *testing.T) {
	t.Parallel()
	buckets := &dao.SessionGapBuckets{
		TotalLow:  100,
		TotalHigh: 100,
		Issues: []dao.SessionIssueGapCount{
			{Slug: "low-weakness", LowCount: 70, HighCount: 30},  // low > high → kept
			{Slug: "high-strength", LowCount: 20, HighCount: 80}, // high > low → dropped (inverse)
			{Slug: "no-gap", LowCount: 50, HighCount: 50},        // equal → dropped
		},
	}
	gaps := gapsWithFDR(buckets)
	require.Len(t, gaps, 1)
	assert.Equal(t, "low-weakness", gaps[0].GetIssueSlug())
	assert.Positive(t, gaps[0].GetGapPp())
}

// TestGapsWithFDR_ExcludesSecuritySlugs asserts the security-layer slugs
// (api-key-leak / db-credential-leak / pii-injection) never enter HighVsLowGap:
// they score the user's pasted prompt text, not a harness-governable standard, so
// a rule can't be derived from them. This is the invariant securityIncidentCount
// documents — without the drop, a below-threshold org mints api-key-leak / pii
// rules from these prompt-quality gaps.
func TestGapsWithFDR_ExcludesSecuritySlugs(t *testing.T) {
	t.Parallel()
	buckets := &dao.SessionGapBuckets{
		TotalLow:  100,
		TotalHigh: 100,
		Issues: []dao.SessionIssueGapCount{
			{Slug: "missing-internal-context", LowCount: 70, HighCount: 10}, // quality gap → kept
			{Slug: "api-key-leak", LowCount: 40, HighCount: 0},              // security → dropped
			{Slug: "db-credential-leak", LowCount: 30, HighCount: 0},        // security → dropped
			{Slug: "pii-injection", LowCount: 25, HighCount: 0},             // security → dropped
			{Slug: "  API-Key-Leak  ", LowCount: 20, HighCount: 0},          // case/whitespace variant → still dropped
		},
	}
	gaps := gapsWithFDR(buckets)
	require.Len(t, gaps, 1)
	assert.Equal(t, "missing-internal-context", gaps[0].GetIssueSlug())
	for _, gap := range gaps {
		assert.False(t, isSecurityGapSlug(gap.GetIssueSlug()), "security slug leaked into gaps: %s", gap.GetIssueSlug())
	}
}

func TestBucketThemes(t *testing.T) {
	t.Parallel()
	bodies := []string{"b0-conf0.6", "b1-conf0.9", "b2-conf0.7", "b3-conf0.8", "b4-other"}
	classifications := []ThemeClassification{
		{Index: 0, Theme: "testing", Confidence: 0.6},
		{Index: 1, Theme: "testing", Confidence: 0.9},
		{Index: 2, Theme: "testing", Confidence: 0.7},
		{Index: 3, Theme: "testing", Confidence: 0.8},
		{Index: 4, Theme: "other", Confidence: 0.95}, // excluded — not an engineering theme
	}
	summaries := bucketThemes(classifications, bodies)
	require.Len(t, summaries, 1)
	testing := summaries[0]
	assert.Equal(t, "testing", testing.GetTheme())
	assert.Equal(t, int32(4), testing.GetCount(), "count is all comments in the theme")
	// examples are the top-3 by confidence desc; the 0.6 comment is excluded.
	assert.Equal(t, []string{"b1-conf0.9", "b3-conf0.8", "b2-conf0.7"}, testing.GetExamples())
}

func TestTwoProportionPValue(t *testing.T) {
	t.Parallel()
	// A large, clear gap (80% vs 10%) is highly significant.
	strong := twoProportionPValue(2, 20, 80, 100)
	assert.Less(t, strong, 0.01)
	// Equal rates carry no signal → p ≈ 1.
	none := twoProportionPValue(10, 100, 10, 100)
	assert.InDelta(t, 1.0, none, 0.01)
	// Empty cohort → no evidence.
	assert.Equal(t, 1.0, twoProportionPValue(0, 0, 5, 10))
}

// aggregatorMocks bundles the mock DAOs + an httptest classify server for a
// BuildContext run.
type aggregatorMocks struct {
	insight          *daomocks.MockISessionInsightDAO
	pull             *daomocks.MockIPullRequestDAO
	raw              *daomocks.MockIRawRecordDAO
	security         *daomocks.MockISecurityFlagDAO
	repo             *daomocks.MockIRepositoryDAO
	repoProfile      *daomocks.MockIVCSRepoProfileDAO
	library          *daomocks.MockIPromptLibraryDAO
	platformIdentity *daomocks.MockIPlatformIdentityMapDAO
	server           *httptest.Server
}

func newAggregatorMocks(t *testing.T, classify http.HandlerFunc) (*Aggregator, *aggregatorMocks) {
	t.Helper()
	return newAggregatorMocksWithParams(t, classify, testParams)
}

// newAggregatorMocksWithParams is newAggregatorMocks with a caller-supplied params
// accessor, for tests that drive config-dependent behaviour (the review-window
// clamp / corpus cap).
func newAggregatorMocksWithParams(t *testing.T, classify http.HandlerFunc, params func() DerivationParams) (*Aggregator, *aggregatorMocks) {
	t.Helper()
	ctrl := gomock.NewController(t)
	m := &aggregatorMocks{
		insight:          daomocks.NewMockISessionInsightDAO(ctrl),
		pull:             daomocks.NewMockIPullRequestDAO(ctrl),
		raw:              daomocks.NewMockIRawRecordDAO(ctrl),
		security:         daomocks.NewMockISecurityFlagDAO(ctrl),
		repo:             daomocks.NewMockIRepositoryDAO(ctrl),
		repoProfile:      daomocks.NewMockIVCSRepoProfileDAO(ctrl),
		library:          daomocks.NewMockIPromptLibraryDAO(ctrl),
		platformIdentity: daomocks.NewMockIPlatformIdentityMapDAO(ctrl),
	}
	if classify == nil {
		classify = func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(pyclient.ClassifyThemesResponse{})
		}
	}
	m.server = httptest.NewServer(classify)
	t.Cleanup(m.server.Close)

	agg := NewAggregator(m.insight, m.pull, m.raw, m.security, m.repo, m.repoProfile, m.library,
		NewLLMClient(m.server.URL, nil, nil), m.platformIdentity, params)
	return agg, m
}

// expectBaselineReads stubs the standard (non-grounding) BuildContext reads a
// non-securityOnly run makes, returning minimal-valid data so the run reaches the
// grounding scope. treeFiles seeds the repo-tree blob (no rule files → the resolved
// tool falls through to the ide_breakdown row). ideRows drives resolveDominantTool.
func expectBaselineReads(t *testing.T, m *aggregatorMocks, treeFiles []string, ideRows []*model.SessionIDESummary) {
	t.Helper()
	m.insight.EXPECT().AggregateHighVsLowGap(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), 3, 8, gomock.Any()).
		Return(&dao.SessionGapBuckets{TotalLow: 10, TotalHigh: 10}, nil)
	m.insight.EXPECT().AggregateByOrg(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(&model.SessionOrgSummary{TotalConversations: 40}, nil)
	m.insight.EXPECT().AggregateDimensionsByOrg(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(&model.SessionDimensionScores{}, nil)
	m.insight.EXPECT().AggregateSessionsByProject(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
	m.insight.EXPECT().AggregateByTaskType(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
	m.insight.EXPECT().AggregateByIDE(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(ideRows, nil)
	m.security.EXPECT().CountByFlagType(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any()).Return(map[string]int{}, nil)
	treeContent := string(makeBlob(t, false, treeFiles))
	m.repo.EXPECT().GetByID(gomock.Any(), aggTestOrgID, "rid").
		Return(&model.Repository{ID: "rid", FullName: "owner/repo", Name: "repo", ConnectorID: "github", TreeContent: &treeContent}, nil)
	m.library.EXPECT().ListDismissedSlugs(gomock.Any(), aggTestOrgID, gomock.Any(), "rid").Return(nil, nil)
	m.library.EXPECT().ListPinnedSlugs(gomock.Any(), aggTestOrgID, gomock.Any(), "rid").Return(nil, nil)
	m.raw.EXPECT().ListReviewBodies(gomock.Any(), aggTestOrgID, []string{"owner/repo"}, []string{"github"}, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
}

// TestBuildContext_GroundingSignals asserts that with the hooks-and-agents feature enabled a
// non-securityOnly run populates every new AggregatedContext grounding field (13,15-19)
// from readGroundingSignals and sets resolved_target_tool (field 20) from the resolver.
func TestBuildContext_GroundingSignals(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	agg, m := newAggregatorMocks(t, nil)

	// No rule files in the tree → dominantRuleTool is UNSPECIFIED, so resolveDominantTool
	// falls through to the ide_breakdown row (cursor is the dominant tool).
	expectBaselineReads(t, m, []string{"main.go", "dao/repo.go"},
		[]*model.SessionIDESummary{{Ide: "IDE_TYPE_CURSOR", Sessions: 20, AvgOverall: 7.5}})

	avgHrs := 4.5
	enforced := "VCS_MERGE_ENFORCEMENT_ENFORCED"
	m.pull.EXPECT().AggregatePickupTime(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(&model.PickupTimeResult{AvgHrs: &avgHrs}, nil)
	m.pull.EXPECT().AggregatePRSize(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(&model.PRSizeResult{LargePRCount: 3, PRCount: 10}, nil)
	m.pull.EXPECT().AggregateAvgLoopsPerPR(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(dao.LoopsPerPRResult{AvgNonAI: 2}, nil)
	m.repoProfile.EXPECT().GetByRepoID(gomock.Any(), aggTestOrgID, "rid").Return(&model.VCSRepoProfile{MergeEnforcement: &enforced}, nil)
	m.pull.EXPECT().AggregatePerModuleChurn(gomock.Any(), aggTestOrgID, "rid", gomock.Any(), gomock.Any()).Return([]model.PerModuleChurnRow{{Module: "dao", ChurnLines: 500}}, nil)
	m.pull.EXPECT().AggregateBugSignal(gomock.Any(), aggTestOrgID, "rid", gomock.Any(), gomock.Any()).Return(&model.BugSignalResult{BugCount: 5, HighPriority: 2}, nil)
	m.insight.EXPECT().AggregateLanguageBreakdown(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return([]*model.AILeverageLanguageBreakdown{{Language: "go"}}, nil)
	m.insight.EXPECT().AggregateCostByModel(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return([]*model.AILeverageCostByModel{{Model: "opus"}}, nil)
	m.insight.EXPECT().AggregateMCPBreakdown(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return([]*model.AILeverageMCPBreakdown{{ServerName: "github"}}, nil)
	m.insight.EXPECT().AggregateSubagentDelegations(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(7, nil)
	m.insight.EXPECT().AggregateFrictionCounts(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(map[string]int{"context_saturation": 4}, nil)

	got, _, _, err := agg.BuildContext(context.Background(), aggTestOrgID, "rid", from, to, false, true)
	require.NoError(t, err)

	require.Len(t, got.GetPrSignals(), 2, "one repo-level row + one per-module row")
	repoRow := got.GetPrSignals()[0]
	assert.Empty(t, repoRow.GetModule(), "repo-level row carries no module")
	assert.EqualValues(t, 2, repoRow.GetIterations())
	assert.EqualValues(t, 30, repoRow.GetLargePrPct(), "3/10 large PRs → 30%")
	assert.InDelta(t, 4.5, repoRow.GetAvgReviewHrs(), 0.001)
	assert.Equal(t, enforced, repoRow.GetMergeEnforcement())
	moduleRow := got.GetPrSignals()[1]
	assert.Equal(t, "dao", moduleRow.GetModule())
	assert.EqualValues(t, 500, moduleRow.GetChurnLines())

	assert.EqualValues(t, 5, got.GetBugSignal().GetBugCount())
	assert.EqualValues(t, 2, got.GetBugSignal().GetHighPriority())
	assert.EqualValues(t, 7, got.GetSubagentUsage().GetTotalDelegations())
	assert.EqualValues(t, 4, got.GetFriction().GetFrictionCounts()["context_saturation"])
	assert.Equal(t, []string{"go"}, got.GetLanguages())
	assert.Equal(t, []string{"opus"}, got.GetModelsUsed())
	assert.Equal(t, []string{"github"}, got.GetMcpServers())
	assert.Equal(t, commonv1.IdeType_IDE_TYPE_CURSOR, got.GetResolvedTargetTool(), "ide_breakdown dominant tool resolves when no rule-file tool is checked in")
}

func TestBuildContext(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	classify := func(w http.ResponseWriter, r *http.Request) {
		var req pyclient.ClassifyThemesRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp := pyclient.ClassifyThemesResponse{StrategyUsed: "sbert"}
		for i := range req.Comments {
			theme := "testing"
			if i%2 == 1 {
				theme = "security"
			}
			resp.Classifications = append(resp.Classifications, pyclient.ThemeClassification{Index: i, Theme: theme, Confidence: 0.8})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
	agg, m := newAggregatorMocks(t, classify)

	buckets := &dao.SessionGapBuckets{
		TotalLow:  10,
		TotalMid:  15,
		TotalHigh: 20,
		Issues:    []dao.SessionIssueGapCount{{Slug: "error-handling", LowCount: 8, HighCount: 2}},
	}
	m.insight.EXPECT().AggregateHighVsLowGap(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), 3, 8, gomock.Any()).Return(buckets, nil)
	// TotalConversations (55) exceeds the scored low+mid+high (45): 10 sessions have no
	// overallScore. mid must read the scored count (15), NOT 55-10-20=25 — the old
	// subtraction folded those 10 unscored sessions into mid.
	m.insight.EXPECT().AggregateByOrg(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&model.SessionOrgSummary{TotalConversations: 55}, nil)
	m.insight.EXPECT().AggregateDimensionsByOrg(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&model.SessionDimensionScores{Clarity: 5, Specificity: 6, ContextProvision: 7, Actionability: 8, Completeness: 4, Efficiency: 3}, nil)
	m.insight.EXPECT().AggregateSessionsByProject(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]*model.SessionProjectSummary{{ProjectName: "owner/repo", SessionCount: 30}}, nil)
	// One scored task type (efficiency 4 is the min → weakest) and one all-zero
	// task type (no scored quality data → empty weakest_dim guard).
	m.insight.EXPECT().AggregateByTaskType(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]*model.SessionTaskTypeSummary{
			{TaskType: "feature", Sessions: 12, AvgOverall: 7.2, Clarity: 8, Specificity: 7, ContextProvision: 9, Actionability: 6, Completeness: 5, Efficiency: 4},
			{TaskType: "TASK_TYPE_UNSPECIFIED", Sessions: 3, AvgOverall: 0},
		}, nil)
	m.insight.EXPECT().AggregateByIDE(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]*model.SessionIDESummary{
			{Ide: "cursor", Sessions: 20, AvgOverall: 7.5},
			{Ide: "IDE_TYPE_UNSPECIFIED", Sessions: 5, AvgOverall: 6.1},
		}, nil)
	// flag_type holds the kebab-case taxonomy category_slug verbatim (tetriz-py
	// analyze_prompt.py) — the mock must use real slug shape so this guards the
	// aggregator's lookup keys.
	m.security.EXPECT().CountByFlagType(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any()).
		Return(map[string]int{"api-key-leak": 2, "pii-injection": 1}, nil)
	treeContent := string(makeBlob(t, false, []string{"main.go", "dao/repo.go"}))
	m.repo.EXPECT().GetByID(gomock.Any(), aggTestOrgID, "rid").
		Return(&model.Repository{ID: "rid", FullName: "owner/repo", Name: "repo", ConnectorID: "github", TreeContent: &treeContent}, nil)
	m.library.EXPECT().ListDismissedSlugs(gomock.Any(), aggTestOrgID, gomock.Any(), "rid").Return([]string{"Old rule"}, nil)
	m.library.EXPECT().ListPinnedSlugs(gomock.Any(), aggTestOrgID, gomock.Any(), "rid").Return([]model.ArtifactSlugRef{{ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE, Slug: "Keep rule"}}, nil)
	m.raw.EXPECT().ListReviewBodies(gomock.Any(), aggTestOrgID, []string{"owner/repo"}, []string{"github"}, gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]string{"add a test here", "this leaks a secret"}, nil)

	got, _, _, err := agg.BuildContext(context.Background(), aggTestOrgID, "rid", from, to, false, false)
	require.NoError(t, err)

	assert.Equal(t, aggTestOrgID, got.GetOrgId())
	assert.Equal(t, "2026-01-01", got.GetDateRange().GetDateFrom())
	// Session summary: TotalSessions is the full count (incl. unscored); the
	// distribution carries the scored buckets and sums to scored sessions only.
	assert.Equal(t, int32(55), got.GetSessionSummary().GetTotalSessions())
	assert.Equal(t, map[string]int32{"low": 10, "mid": 15, "high": 20}, got.GetSessionSummary().GetQualityDistribution())
	assert.InDelta(t, 5.0, got.GetSessionSummary().GetDimensionAverages()["clarity"], 0.001)
	// Gap: highPct 10, lowPct 80, gap 70.
	require.Len(t, got.GetHighVsLowGap(), 1)
	assert.Equal(t, "error-handling", got.GetHighVsLowGap()[0].GetIssueSlug())
	assert.InDelta(t, 10.0, got.GetHighVsLowGap()[0].GetHighPct(), 0.01)
	assert.InDelta(t, 80.0, got.GetHighVsLowGap()[0].GetLowPct(), 0.01)
	assert.InDelta(t, 70.0, got.GetHighVsLowGap()[0].GetGapPp(), 0.01)
	// Security mapping.
	assert.Equal(t, int32(2), got.GetSecurityIncidents().GetApiKeyLeak())
	assert.Equal(t, int32(1), got.GetSecurityIncidents().GetPiiInjection())
	// Repo tree attached.
	require.Len(t, got.GetRepoTrees(), 1)
	assert.Equal(t, "owner/repo", got.GetRepoTrees()[0].GetRepoFullName())
	// Admin feedback.
	assert.Equal(t, []string{"Old rule"}, got.GetAdminFeedback().GetDismissedRuleSlugs())
	assert.Equal(t, []string{"Keep rule"}, got.GetAdminFeedback().GetPinnedRuleSlugs())
	// Review themes classified + bucketed (testing + security).
	assert.Len(t, got.GetReviewThemes(), 2)
	// Project map.
	require.Len(t, got.GetPerProject(), 1)
	assert.Equal(t, "owner/repo", got.GetPerProject()[0].GetProject())
	assert.Equal(t, "owner/repo", got.GetPerProject()[0].GetRepoFullName())
	assert.Equal(t, int32(30), got.GetPerProject()[0].GetSessions())
	// Per-task-type: sessions + mean avg map through; weakest dim is the min of the
	// six means (efficiency=4), and the all-zero row emits an empty weakest_dim.
	require.Len(t, got.GetPerTaskType(), 2)
	assert.Equal(t, "feature", got.GetPerTaskType()[0].GetTaskType())
	assert.Equal(t, int32(12), got.GetPerTaskType()[0].GetSessions())
	assert.InDelta(t, 7.2, got.GetPerTaskType()[0].GetAvg(), 0.001)
	assert.Equal(t, "efficiency", got.GetPerTaskType()[0].GetWeakestDim())
	assert.InDelta(t, 4.0, got.GetPerTaskType()[0].GetWeakestVal(), 0.001)
	assert.Equal(t, "TASK_TYPE_UNSPECIFIED", got.GetPerTaskType()[1].GetTaskType())
	assert.Empty(t, got.GetPerTaskType()[1].GetWeakestDim(), "no scored quality data → no weakest dimension")
	assert.Zero(t, got.GetPerTaskType()[1].GetWeakestVal())
	// IDE breakdown: per-IDE sessions + mean avg.
	require.Len(t, got.GetIdeBreakdown(), 2)
	assert.Equal(t, "cursor", got.GetIdeBreakdown()[0].GetIde())
	assert.Equal(t, int32(20), got.GetIdeBreakdown()[0].GetSessions())
	assert.InDelta(t, 7.5, got.GetIdeBreakdown()[0].GetAvg(), 0.001)
	assert.Equal(t, "IDE_TYPE_UNSPECIFIED", got.GetIdeBreakdown()[1].GetIde())
}

// TestBuildContext_SecurityOnlyStripsGeneralSignals asserts the spec context
// steering: on a securityOnly run BuildContext keeps the security evidence + repo
// tree + admin feedback but strips the general quality signals (session summary,
// gap, per-task-type, IDE, per-project) so the derive isn't prompted to produce
// general rules — and it never reads/classifies review bodies.
func TestBuildContext_SecurityOnlyStripsGeneralSignals(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// classify must never be hit on a securityOnly run (review bodies aren't read).
	classify := func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("classify-themes must not be called on a securityOnly run")
	}
	agg, m := newAggregatorMocks(t, classify)

	// The general-signal aggregations (gap, session/org, dimensions, per-project,
	// task-type, IDE) are NOT expected: a securityOnly run skips the reads entirely
	// rather than computing then discarding them. gomock fails the test if any of
	// them is called, which is the assertion.
	m.security.EXPECT().CountByFlagType(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any()).
		Return(map[string]int{"api-key-leak": 2}, nil)
	treeContent := string(makeBlob(t, false, []string{"main.go"}))
	m.repo.EXPECT().GetByID(gomock.Any(), aggTestOrgID, "rid").
		Return(&model.Repository{ID: "rid", FullName: "owner/repo", Name: "repo", ConnectorID: "github", TreeContent: &treeContent}, nil)
	m.library.EXPECT().ListDismissedSlugs(gomock.Any(), aggTestOrgID, gomock.Any(), "rid").Return([]string{"Old rule"}, nil)
	m.library.EXPECT().ListPinnedSlugs(gomock.Any(), aggTestOrgID, gomock.Any(), "rid").Return(nil, nil)
	// ListReviewBodies is NOT expected — it must not be called on a securityOnly run.
	// Nor is the catalog read: BuildContext builds the derive context only and never reads
	// the marketplace catalog — that digest belongs to the recommender pipeline.

	got, _, _, err := agg.BuildContext(context.Background(), aggTestOrgID, "rid", from, to, true, false)
	require.NoError(t, err)

	// Kept: identity + security evidence + grounding.
	assert.Equal(t, aggTestOrgID, got.GetOrgId())
	assert.Equal(t, "2026-01-01", got.GetDateRange().GetDateFrom())
	assert.Equal(t, int32(2), got.GetSecurityIncidents().GetApiKeyLeak())
	require.Len(t, got.GetRepoTrees(), 1)
	assert.Equal(t, []string{"Old rule"}, got.GetAdminFeedback().GetDismissedRuleSlugs())
	// Stripped: the general quality signals that would prompt general rules.
	assert.Nil(t, got.GetSessionSummary(), "session summary stripped on securityOnly")
	assert.Empty(t, got.GetHighVsLowGap(), "gap stripped on securityOnly")
	assert.Empty(t, got.GetPerTaskType(), "per-task-type stripped on securityOnly")
	assert.Empty(t, got.GetIdeBreakdown(), "IDE breakdown stripped on securityOnly")
	assert.Empty(t, got.GetPerProject(), "per-project stripped on securityOnly")
	assert.Empty(t, got.GetReviewThemes(), "review themes not classified on securityOnly")
}

func TestBuildContext_RepoTreeMissingSkips(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	agg, m := newAggregatorMocks(t, nil)

	m.insight.EXPECT().AggregateHighVsLowGap(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), 3, 8, gomock.Any()).
		Return(&dao.SessionGapBuckets{TotalLow: 5, TotalHigh: 5}, nil)
	m.insight.EXPECT().AggregateByOrg(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(&model.SessionOrgSummary{TotalConversations: 10}, nil)
	m.insight.EXPECT().AggregateDimensionsByOrg(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(&model.SessionDimensionScores{}, nil)
	m.insight.EXPECT().AggregateByTaskType(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
	m.insight.EXPECT().AggregateByIDE(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
	m.insight.EXPECT().AggregateSessionsByProject(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
	m.security.EXPECT().CountByFlagType(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any()).Return(map[string]int{}, nil)
	m.repo.EXPECT().GetByID(gomock.Any(), aggTestOrgID, "rid").
		Return(&model.Repository{ID: "rid", FullName: "owner/repo", Name: "repo", ConnectorID: "github"}, nil)
	// Blob is gone: NoSuchKey → skip the repo's tree, do NOT fail the run.
	m.library.EXPECT().ListDismissedSlugs(gomock.Any(), aggTestOrgID, gomock.Any(), "rid").Return(nil, nil)
	m.library.EXPECT().ListPinnedSlugs(gomock.Any(), aggTestOrgID, gomock.Any(), "rid").Return(nil, nil)
	m.raw.EXPECT().ListReviewBodies(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)

	got, _, _, err := agg.BuildContext(context.Background(), aggTestOrgID, "rid", from, to, false, false)
	require.NoError(t, err)
	assert.Empty(t, got.GetRepoTrees(), "a missing tree blob skips the repo, not fails the run")
}

// TestBuildContext_EnrichedTopN pins the enriched-path decision: a repo below
// MinReviewCommentsPerRepo classifies with maxThemes = MaxReviewThemesPerComment and
// BuildContext returns enriched=true; at/above it classifies 1:1 and returns false. It
// captures the max_themes_per_comment the classify request carries.
func TestBuildContext_EnrichedTopN(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		bodies        []string
		reviewCap     int // ReviewClassifyMaxComments; 0 = no cap
		uncappedCount int // >0 ⇒ the read truncates at a below-floor cap and this uncapped count is consulted
		wantEnriched  bool
		wantMax       int
	}{
		{name: "below the comment floor → enriched top-3", bodies: []string{"a", "b"}, wantEnriched: true, wantMax: 3},
		{name: "at/above the comment floor → standard 1:1", bodies: []string{"a", "b", "c", "d"}, wantEnriched: false, wantMax: 1},
		// Derive-time read empty (e.g. the classify window shifted after admission the
		// repo): still enriched (count 0 < floor), and NO classify call is made — so the engine
		// terminates REPO_FEW_THEMES rather than paying for a derive with no review evidence.
		{name: "below floor but empty derive-time read → enriched, no classify", bodies: nil, wantEnriched: true, wantMax: -1},
		// When a cap set BELOW the floor truncates the read (len == cap == 2 < floor 3), the
		// enriched decision consults the UNCAPPED count, not len — so a big repo (true count 5 ≥
		// floor) is correctly standard, not wrongly enriched by the small read...
		{name: "below-floor cap truncates a big repo → standard via uncapped count", bodies: []string{"a", "b"}, reviewCap: 2, uncappedCount: 5, wantEnriched: false, wantMax: 1},
		// ...and a genuinely-sparse repo whose count fills the same cap (uncapped 2 < floor 3)
		// still enriches — the uncapped count prevents a false NEGATIVE too.
		{name: "below-floor cap, genuinely sparse repo → still enriched", bodies: []string{"a", "b"}, reviewCap: 2, uncappedCount: 2, wantEnriched: true, wantMax: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMax := -1 // -1 = classify not called
			classify := func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					MaxThemesPerComment int `json:"max_themes_per_comment"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				gotMax = req.MaxThemesPerComment
				_, _ = w.Write([]byte(`{"classifications":[{"index":0,"theme":"testing","confidence":0.9}],"strategy_used":"sbert"}`))
			}
			// MinReviewCommentsPerRepo=3 sets the standard/enriched boundary; MaxReviewThemesPerComment=3
			// is the top-N used on the enriched path.
			params := func() DerivationParams {
				return DerivationParams{LowScoreMax: 3, HighScoreMin: 8, MinReviewCommentsPerRepo: 3, MaxReviewThemesPerComment: 3, ReviewClassifyMaxComments: tt.reviewCap}
			}
			agg, m := newAggregatorMocksWithParams(t, classify, params)

			m.insight.EXPECT().AggregateHighVsLowGap(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), 3, 8, gomock.Any()).Return(&dao.SessionGapBuckets{TotalLow: 5, TotalHigh: 5}, nil)
			m.insight.EXPECT().AggregateByOrg(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(&model.SessionOrgSummary{TotalConversations: 10}, nil)
			m.insight.EXPECT().AggregateDimensionsByOrg(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(&model.SessionDimensionScores{}, nil)
			m.insight.EXPECT().AggregateSessionsByProject(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
			m.insight.EXPECT().AggregateByTaskType(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
			m.insight.EXPECT().AggregateByIDE(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
			m.security.EXPECT().CountByFlagType(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any()).Return(map[string]int{}, nil)
			treeContent := string(makeBlob(t, false, []string{"main.go"}))
			m.repo.EXPECT().GetByID(gomock.Any(), aggTestOrgID, "rid").Return(&model.Repository{ID: "rid", FullName: "owner/repo", Name: "repo", ConnectorID: "github", TreeContent: &treeContent}, nil)
			m.library.EXPECT().ListDismissedSlugs(gomock.Any(), aggTestOrgID, gomock.Any(), "rid").Return(nil, nil)
			m.library.EXPECT().ListPinnedSlugs(gomock.Any(), aggTestOrgID, gomock.Any(), "rid").Return(nil, nil)
			m.raw.EXPECT().ListReviewBodies(gomock.Any(), aggTestOrgID, []string{"owner/repo"}, []string{"github"}, gomock.Any(), gomock.Any(), gomock.Any()).Return(tt.bodies, nil)
			// Only a read truncated at a below-floor cap consults the uncapped count; the
			// common cases (no cap / len<cap) decide from len(reviewBodies) and never call it.
			if tt.uncappedCount > 0 {
				m.raw.EXPECT().CountReviewBodies(gomock.Any(), aggTestOrgID, "owner/repo", "github", gomock.Any(), gomock.Any()).Return(tt.uncappedCount, nil)
			}

			_, enriched, _, err := agg.BuildContext(context.Background(), aggTestOrgID, "rid", from, to, false, false)
			require.NoError(t, err)
			assert.Equal(t, tt.wantEnriched, enriched, "enriched flag")
			assert.Equal(t, tt.wantMax, gotMax, "max_themes_per_comment sent to classify")
		})
	}
}

// TestBuildContext_ReviewWindowClampAndCap pins the review-corpus
// bounding: the review-body read is narrowed to the trailing
// ReviewClassifyLookbackDays (clamped, never widening past the derivation `from`)
// and capped at ReviewClassifyMaxComments — applied ONLY at the review read, while
// the org-grain quality/security aggregations keep the broad [from, to] window. It
// captures the exact `from`/`maxBodies` args ListReviewBodies receives.
func TestBuildContext_ReviewWindowClampAndCap(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC) // window ≫ 30d, so the clamp narrows it
	const lookbackDays, maxComments = 30, 10
	params := func() DerivationParams {
		return DerivationParams{LowScoreMax: 3, HighScoreMin: 8, ReviewClassifyLookbackDays: lookbackDays, ReviewClassifyMaxComments: maxComments}
	}
	agg, m := newAggregatorMocksWithParams(t, nil, params)

	// The broad-window aggregations must still see [from, to] — assert the quality
	// gap read is NOT narrowed by the review clamp. Each session read also carries a
	// repo filter (Workstream B: repo-scoped signals) — the trailing gomock.Any()
	// matches it; the gap call additionally captures it to assert a filter is passed.
	var gotGapFilters int
	m.insight.EXPECT().AggregateHighVsLowGap(gomock.Any(), aggTestOrgID, from, to, 3, 8, gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, _, _ time.Time, _, _ int, filters ...dao.FilterOption) (*dao.SessionGapBuckets, error) {
			gotGapFilters = len(filters)
			return &dao.SessionGapBuckets{TotalLow: 5, TotalHigh: 5}, nil
		})
	// Each of the other 4 session reads must ALSO receive the repo filter — a bare
	// gomock.Any() would pass whether or not repoFilter is threaded, so capture and
	// assert exactly one filter reaches each (guards a dropped-scope regression).
	m.insight.EXPECT().AggregateByOrg(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, _, _ time.Time, filters ...dao.FilterOption) (*model.SessionOrgSummary, error) {
			assert.Len(t, filters, 1, "AggregateByOrg must be repo-scoped")
			return &model.SessionOrgSummary{TotalConversations: 10}, nil
		})
	m.insight.EXPECT().AggregateDimensionsByOrg(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, _, _ time.Time, filters ...dao.FilterOption) (*model.SessionDimensionScores, error) {
			assert.Len(t, filters, 1, "AggregateDimensionsByOrg must be repo-scoped")
			return &model.SessionDimensionScores{}, nil
		})
	m.insight.EXPECT().AggregateByTaskType(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, _, _ time.Time, filters ...dao.FilterOption) ([]*model.SessionTaskTypeSummary, error) {
			assert.Len(t, filters, 1, "AggregateByTaskType must be repo-scoped")
			return nil, nil
		})
	m.insight.EXPECT().AggregateByIDE(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, _, _ time.Time, filters ...dao.FilterOption) ([]*model.SessionIDESummary, error) {
			assert.Len(t, filters, 1, "AggregateByIDE must be repo-scoped")
			return nil, nil
		})
	m.insight.EXPECT().AggregateSessionsByProject(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
	m.security.EXPECT().CountByFlagType(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any()).Return(map[string]int{}, nil)
	treeContent := string(makeBlob(t, false, []string{"main.go"}))
	m.repo.EXPECT().GetByID(gomock.Any(), aggTestOrgID, "rid").
		Return(&model.Repository{ID: "rid", FullName: "owner/repo", Name: "repo", ConnectorID: "github", TreeContent: &treeContent}, nil)
	m.library.EXPECT().ListDismissedSlugs(gomock.Any(), aggTestOrgID, gomock.Any(), "rid").Return(nil, nil)
	m.library.EXPECT().ListPinnedSlugs(gomock.Any(), aggTestOrgID, gomock.Any(), "rid").Return(nil, nil)

	var gotReviewFrom, gotReviewTo time.Time
	var gotMaxBodies int
	m.raw.EXPECT().ListReviewBodies(gomock.Any(), aggTestOrgID, []string{"owner/repo"}, []string{"github"}, gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, _, _ []string, reviewFrom, reviewTo time.Time, maxBodies int) ([]string, error) {
			gotReviewFrom, gotReviewTo, gotMaxBodies = reviewFrom, reviewTo, maxBodies
			return nil, nil
		})

	_, _, _, err := agg.BuildContext(context.Background(), aggTestOrgID, "rid", from, to, false, false)
	require.NoError(t, err)

	assert.Equal(t, to.AddDate(0, 0, -lookbackDays), gotReviewFrom, "review read narrowed to the trailing 30d lookback")
	assert.Equal(t, to, gotReviewTo, "review window upper bound is unchanged")
	assert.Equal(t, maxComments, gotMaxBodies, "review corpus capped at ReviewClassifyMaxComments")
	assert.Equal(t, 1, gotGapFilters, "the gap read must be repo-scoped (one WithVCSRepos filter passed)")
}

// TestBuildContext_RepoNotFoundFailsRun asserts that an unresolvable repo id
// (deleted mid-window) fails the run rather than silently deriving ungrounded —
// grounding must bind to exactly the queued repo.
func TestBuildContext_RepoNotFoundFailsRun(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	agg, m := newAggregatorMocks(t, nil)

	// The repo resolves first — its full_name scopes every session read — so an
	// unresolvable repo fails the run before any signal read is attempted (no other
	// mock is set: a stray read would be an unexpected call).
	m.repo.EXPECT().GetByID(gomock.Any(), aggTestOrgID, "gone").Return(nil, dao.ErrNotFound)

	_, _, _, err := agg.BuildContext(context.Background(), aggTestOrgID, "gone", from, to, false, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolving repo gone")
}

func TestComputePRCorrelation(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	agg, m := newAggregatorMocks(t, nil)

	m.insight.EXPECT().AggregatePersonScoreBuckets(gomock.Any(), aggTestOrgID, from, to, 3, 8).
		Return(&dao.PersonScoreBuckets{HighPersonIDs: []string{"p-high"}, LowPersonIDs: []string{"p-low"}}, nil)
	// The helper resolves per VCS platform (github/gitlab/bitbucket) in 500-person
	// chunks, so each cohort is looked up once per platform. The mock branches on
	// the requested person; only github carries an identity for these persons
	// (gitlab/bitbucket return empty), so each cohort resolves to a single id —
	// exercising both the per-platform fan-out and the union. Times = cohorts ×
	// platforms.
	m.platformIdentity.EXPECT().BatchGetIdentityIDsForPersons(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, personIDs []string, platform string) (map[string]string, error) {
			require.NotEmpty(t, platform, "identity lookup must carry a platform")
			if platform != "github" {
				return map[string]string{}, nil
			}
			person := personIDs[0]
			identity := "pi-low"
			if person == "p-high" {
				identity = "pi-high"
			}
			return map[string]string{person: identity}, nil
		}).Times(2 * len(vcsPlatforms))
	m.pull.EXPECT().AggregateMergeHoursForIdentities(gomock.Any(), aggTestOrgID, []string{"pi-high"}, from, to).
		Return(&dao.PRMergeStats{AvgMergeHours: 10, PRCount: 5}, nil)
	m.pull.EXPECT().AggregateMergeHoursForIdentities(gomock.Any(), aggTestOrgID, []string{"pi-low"}, from, to).
		Return(&dao.PRMergeStats{AvgMergeHours: 20, PRCount: 5}, nil)

	got, err := agg.ComputePRCorrelation(context.Background(), aggTestOrgID, from, to)
	require.NoError(t, err)
	assert.InDelta(t, 10.0, got.GetHighSessionsAvgMergeHrs(), 0.01)
	assert.InDelta(t, 20.0, got.GetLowSessionsAvgMergeHrs(), 0.01)
	// Low-session cohort merges 100% slower than the high cohort.
	assert.InDelta(t, 100.0, got.GetDeltaPct(), 0.01)
}

func TestComputePRCorrelation_EmptyCohortNoSignal(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	agg, m := newAggregatorMocks(t, nil)

	// High cohort has people; low cohort is empty → no two-sided signal.
	m.insight.EXPECT().AggregatePersonScoreBuckets(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), 3, 8).
		Return(&dao.PersonScoreBuckets{HighPersonIDs: []string{"p-high"}, LowPersonIDs: nil}, nil)
	// Only the high cohort triggers identity lookups (an empty person set short-circuits),
	// once per VCS platform; only github carries an identity.
	m.platformIdentity.EXPECT().BatchGetIdentityIDsForPersons(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, personIDs []string, platform string) (map[string]string, error) {
			if platform != "github" {
				return map[string]string{}, nil
			}
			return map[string]string{"p-high": "pi-high"}, nil
		}).Times(len(vcsPlatforms))
	m.pull.EXPECT().AggregateMergeHoursForIdentities(gomock.Any(), aggTestOrgID, []string{"pi-high"}, gomock.Any(), gomock.Any()).
		Return(&dao.PRMergeStats{AvgMergeHours: 10, PRCount: 5}, nil)
	m.pull.EXPECT().AggregateMergeHoursForIdentities(gomock.Any(), aggTestOrgID, gomock.Nil(), gomock.Any(), gomock.Any()).
		Return(&dao.PRMergeStats{}, nil) // empty cohort → zero stats

	got, err := agg.ComputePRCorrelation(context.Background(), aggTestOrgID, from, to)
	require.NoError(t, err)
	assert.Zero(t, got.GetDeltaPct(), "a cohort with no merged PRs yields no signal, not a -100%% delta")
	assert.Zero(t, got.GetHighSessionsAvgMergeHrs())
	assert.Zero(t, got.GetLowSessionsAvgMergeHrs())
}

// TestPlatformIdentitiesForPersons_BatchesAt500 guards the 500-person batch cap:
// a 501-person cohort must split into 500+1 per platform, never a single
// unbounded IN clause.
func TestPlatformIdentitiesForPersons_BatchesAt500(t *testing.T) {
	t.Parallel()
	agg, m := newAggregatorMocks(t, nil)
	persons := make([]string, 501)
	for i := range persons {
		persons[i] = "p-" + strconv.Itoa(i)
	}
	var chunkSizes []int
	m.platformIdentity.EXPECT().BatchGetIdentityIDsForPersons(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, personIDs []string, platform string) (map[string]string, error) {
			require.NotEmpty(t, platform, "identity lookup must carry a platform")
			require.LessOrEqual(t, len(personIDs), maxPersonsPerIdentityBatch, "no batch exceeds the cap")
			chunkSizes = append(chunkSizes, len(personIDs))
			return map[string]string{}, nil
		}).Times(2 * len(vcsPlatforms)) // 501 persons → 2 chunks per platform

	got := agg.platformIdentitiesForPersons(context.Background(), aggTestOrgID, persons)
	assert.Empty(t, got)
	total := 0
	for _, size := range chunkSizes {
		total += size
	}
	// Every person resolved exactly once per platform — no gap, no overlap.
	assert.Equal(t, len(persons)*len(vcsPlatforms), total)
}

func TestWeakestDimension(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		summary *model.SessionTaskTypeSummary
		wantDim string
		wantVal float32
	}{
		{
			name:    "distinct minimum returns that dimension",
			summary: &model.SessionTaskTypeSummary{Clarity: 8, Specificity: 7, ContextProvision: 9, Actionability: 6, Completeness: 5, Efficiency: 4},
			wantDim: "efficiency",
			wantVal: 4,
		},
		{
			name:    "minimum on the first dimension is still detected",
			summary: &model.SessionTaskTypeSummary{Clarity: 3, Specificity: 7, ContextProvision: 9, Actionability: 6, Completeness: 5, Efficiency: 4},
			wantDim: "clarity",
			wantVal: 3,
		},
		{
			// Two dimensions share the minimum but it is below the max, so it is a
			// real (not all-equal) weakness — the first dimension in iteration order
			// wins the tie deterministically. Pins that contract against a reorder.
			name:    "partial tie on the minimum — first-in-order wins",
			summary: &model.SessionTaskTypeSummary{Clarity: 4, Specificity: 8, ContextProvision: 8, Actionability: 8, Completeness: 8, Efficiency: 4},
			wantDim: "clarity",
			wantVal: 4,
		},
		{
			name:    "all dimensions equal and non-zero — no distinguishable weakness",
			summary: &model.SessionTaskTypeSummary{Clarity: 7, Specificity: 7, ContextProvision: 7, Actionability: 7, Completeness: 7, Efficiency: 7},
			wantDim: "",
			wantVal: 0,
		},
		{
			name:    "all dimensions zero — no scored quality data",
			summary: &model.SessionTaskTypeSummary{},
			wantDim: "",
			wantVal: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotDim, gotVal := weakestDimension(tt.summary)
			assert.Equal(t, tt.wantDim, gotDim)
			assert.InDelta(t, tt.wantVal, gotVal, 0.0001)
		})
	}
}

// TestBuildContext_BreakdownDAOErrorFailsRun asserts the documented contract that a
// breakdown-query failure fails the whole derivation run (it is required-reads
// evidence, same class as the session summary — not best-effort grounding).
func TestBuildContext_BreakdownDAOErrorFailsRun(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		setupMock func(m *aggregatorMocks)
		wantErr   string
	}{
		{
			name: "task-type breakdown DAO error fails the run",
			setupMock: func(m *aggregatorMocks) {
				m.insight.EXPECT().AggregateByTaskType(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).
					Return(nil, errors.New("db unavailable"))
			},
			wantErr: "building task-type breakdown",
		},
		{
			name: "ide breakdown DAO error fails the run",
			setupMock: func(m *aggregatorMocks) {
				m.insight.EXPECT().AggregateByTaskType(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).
					Return(nil, nil)
				m.insight.EXPECT().AggregateByIDE(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).
					Return(nil, errors.New("db unavailable"))
			},
			wantErr: "building ide breakdown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agg, m := newAggregatorMocks(t, nil)
			// The repo resolves first now (its full_name scopes the session reads).
			m.repo.EXPECT().GetByID(gomock.Any(), aggTestOrgID, "rid").
				Return(&model.Repository{ID: "rid", FullName: "owner/repo"}, nil)
			// Members that run before the breakdowns in the required-reads scope; each
			// session read carries the repo filter (trailing matcher).
			m.insight.EXPECT().AggregateHighVsLowGap(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), 3, 8, gomock.Any()).
				Return(&dao.SessionGapBuckets{TotalLow: 5, TotalHigh: 5}, nil)
			m.insight.EXPECT().AggregateByOrg(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).
				Return(&model.SessionOrgSummary{}, nil)
			m.insight.EXPECT().AggregateDimensionsByOrg(gomock.Any(), aggTestOrgID, gomock.Any(), gomock.Any(), gomock.Any()).
				Return(&model.SessionDimensionScores{}, nil)
			tt.setupMock(m)

			_, _, _, err := agg.BuildContext(context.Background(), aggTestOrgID, "rid", from, to, false, false)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
