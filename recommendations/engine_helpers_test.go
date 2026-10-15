// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
)

// TestReviewClassifyWindow pins the shared review-window helper that
// both the per-repo admission gate and BuildContext use, so the gate's review-count
// cannot drift from the corpus the derive classifies: a non-positive lookback does
// not narrow; a lookback narrows the start to `to`−lookback; a lookback wider than the window
// clamps to `from` (never widens past the data-window floor).
func TestReviewClassifyWindow(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		lookbackDays int
		want         time.Time
	}{
		{"zero lookback does not narrow", 0, from},
		{"negative lookback does not narrow", -5, from},
		{"lookback narrows to the trailing window", 30, to.AddDate(0, 0, -30)},
		{"lookback wider than the window clamps to from (never widens)", 365, from},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ReviewClassifyWindow(from, to, tt.lookbackDays))
		})
	}
}

func TestPriorityToInt(t *testing.T) {
	tests := []struct {
		priority string
		want     int32
	}{
		{"critical", priorityCritical},
		{"high", priorityHigh},
		{"medium", priorityMedium},
		{"low", priorityLow},      // weakest-evidence tier; ranks below medium but above the 0 default
		{"", priorityMedium},      // unknown defaults to medium, never drops
		{"bogus", priorityMedium}, // ditto
	}
	for _, tt := range tests {
		t.Run(tt.priority, func(t *testing.T) {
			if got := priorityToInt(tt.priority); got != tt.want {
				t.Errorf("priorityToInt(%q) = %d, want %d", tt.priority, got, tt.want)
			}
		})
	}
}

func TestAffectedSessions(t *testing.T) {
	tests := []struct {
		name     string
		lowPct   float32
		lowTotal int
		want     int32
	}{
		{"half of fifty", 50, 50, 25},
		{"rounds to nearest", 33.3, 100, 33},
		{"zero total yields zero", 80, 0, 0},
		{"negative total yields zero", 80, -5, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := affectedSessions(tt.lowPct, tt.lowTotal); got != tt.want {
				t.Errorf("affectedSessions(%v, %d) = %d, want %d", tt.lowPct, tt.lowTotal, got, tt.want)
			}
		})
	}
}

func TestCapChars(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"under cap unchanged", "short", 10, "short"},
		{"at cap unchanged", "12345", 5, "12345"},
		{"over cap truncated", "1234567", 5, "12345"},
		{"multibyte not split", "héllo wörld", 5, "héllo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := capChars(tt.in, tt.max); got != tt.want {
				t.Errorf("capChars(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
		})
	}
}

func TestTaskTypeColumn(t *testing.T) {
	debug := "debug"
	tests := []struct {
		name         string
		artifactType promptlibraryv1.ArtifactType
		taskType     *string
		want         string
	}{
		{"template carries its task type", promptlibraryv1.ArtifactType_ARTIFACT_TYPE_TEMPLATE, &debug, "debug"},
		{"template with nil task type is empty", promptlibraryv1.ArtifactType_ARTIFACT_TYPE_TEMPLATE, nil, ""},
		{"rule never carries a task type", promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE, &debug, ""},
		{"skill never carries a task type", promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL, &debug, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ga := GradedArtifact{ArtifactType: tt.artifactType, Artifact: DerivedArtifact{TaskType: tt.taskType}}
			if got := taskTypeColumn(ga); got != tt.want {
				t.Errorf("taskTypeColumn = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildArtifactContent_TypeSpecificByBucket(t *testing.T) {
	rule := GradedArtifact{
		ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE,
		Artifact:     DerivedArtifact{Body: "rule body", CitedPaths: []string{"main.go"}, AddressesIssues: []string{"missing-tests"}},
	}
	tree := &promptlibraryv1.RepoTree{RuleFiles: []string{"CLAUDE.md"}}
	content := buildArtifactContent(context.Background(), rule, tree, commonv1.IdeType_IDE_TYPE_UNSPECIFIED)
	if content.GetOriginal() != "rule body" || content.GetCurrent() != "rule body" {
		t.Errorf("body must populate both original and current: got %q / %q", content.GetOriginal(), content.GetCurrent())
	}
	if content.GetRule() == nil {
		t.Fatal("rule bucket must set the Rule oneof")
	}
	if got := content.GetRule().GetAddressesIssues(); len(got) != 1 || got[0] != "missing-tests" {
		t.Errorf("rule addresses_issues = %v, want [missing-tests]", got)
	}
	// With no LLM-supplied target, resolution falls back to the repo's rule file and
	// derives the tool from it (ignoring the resolved tool, which is for hooks/agents).
	if got := content.GetRule().GetTargetFile(); got != "CLAUDE.md" {
		t.Errorf("rule target_file = %q, want CLAUDE.md", got)
	}
	if got := content.GetRule().GetTargetTool(); got != commonv1.IdeType_IDE_TYPE_CLAUDE_CODE {
		t.Errorf("rule target_tool = %v, want CLAUDE_CODE", got)
	}

	tmpl := GradedArtifact{
		ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_TEMPLATE,
		Artifact:     DerivedArtifact{Body: "t", TriggerKeywords: []string{"debug", "crash"}},
	}
	tmplContent := buildArtifactContent(context.Background(), tmpl, nil, commonv1.IdeType_IDE_TYPE_UNSPECIFIED)
	if tmplContent.GetTemplate() == nil {
		t.Error("template bucket must set the Template oneof")
	}
	// The LLM trigger_keywords must thread into the stored TemplateContent — the tier-1 routing
	// signals the install path reads (they were dropped as an empty TemplateContent{} before).
	if got := tmplContent.GetTemplate().GetTriggerKeywords(); len(got) != 2 || got[0] != "debug" || got[1] != "crash" {
		t.Errorf("template trigger_keywords = %v, want [debug crash]", got)
	}
	// A non-template never carries trigger_keywords (read only in the template branch).
	if got := buildArtifactContent(context.Background(),
		GradedArtifact{ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE, Artifact: DerivedArtifact{Body: "r", TriggerKeywords: []string{"leak"}}},
		tree, commonv1.IdeType_IDE_TYPE_UNSPECIFIED).GetTemplate(); got != nil {
		t.Error("a rule must not carry a Template oneof / trigger_keywords")
	}
	skill := GradedArtifact{ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL, Artifact: DerivedArtifact{Body: "s"}}
	if buildArtifactContent(context.Background(), skill, nil, commonv1.IdeType_IDE_TYPE_UNSPECIFIED).GetSkill() == nil {
		t.Error("skill bucket must set the Skill oneof")
	}

	// HOOK bucket: the Hook oneof carries the structured event/matcher/command verbatim and
	// the tool-scoped target. The edit-scoped matcher MUST preserve MultiEdit (an omitted
	// MultiEdit silently skips multi-edit operations).
	hook := GradedArtifact{
		ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK,
		Artifact:     DerivedArtifact{Body: `{"hooks":{}}`, Slug: "secret-guard", Event: "PreToolUse", Matcher: "Edit|Write|MultiEdit", Command: "guard.sh"},
	}
	hookContent := buildArtifactContent(context.Background(), hook, nil, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE)
	if hookContent.GetHook() == nil {
		t.Fatal("hook bucket must set the Hook oneof")
	}
	if got := hookContent.GetHook().GetMatcher(); got != "Edit|Write|MultiEdit" {
		t.Errorf("hook matcher = %q, want it to preserve MultiEdit", got)
	}
	if got := hookContent.GetHook().GetEvent(); got != "PreToolUse" {
		t.Errorf("hook event = %q, want PreToolUse", got)
	}
	if got := hookContent.GetHook().GetTargetFile(); got != ".claude/settings.json" {
		t.Errorf("hook target_file = %q, want .claude/settings.json", got)
	}
	if got := hookContent.GetHook().GetTargetPlacement(); got != promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_COPY_MERGE {
		t.Errorf("claude hook placement = %v, want COPY_MERGE", got)
	}
	if got := hookContent.GetHook().GetTargetTool(); got != commonv1.IdeType_IDE_TYPE_CLAUDE_CODE {
		t.Errorf("hook target_tool = %v, want the resolved CLAUDE_CODE", got)
	}

	// A stray-whitespace event/matcher passes the gate (which validates the TRIMMED value) but must be
	// canonicalized before synthesis + persistence — an untrimmed " PostToolUse " would become a JSON
	// event key the tool never fires on.
	spacedHook := GradedArtifact{
		ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK,
		Artifact:     DerivedArtifact{Body: `{}`, Slug: "run-tests", Event: "  PostToolUse  ", Matcher: " Edit|Write ", Command: "go test ./..."},
	}
	spaced := buildArtifactContent(context.Background(), spacedHook, nil, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE)
	if got := spaced.GetHook().GetEvent(); got != "PostToolUse" {
		t.Errorf("hook event = %q, want the trimmed PostToolUse", got)
	}
	if got := spaced.GetHook().GetMatcher(); got != "Edit|Write" {
		t.Errorf("hook matcher = %q, want the trimmed Edit|Write", got)
	}
	if strings.Contains(spaced.GetCurrent(), "  PostToolUse  ") {
		t.Errorf("synthesized body must not contain the untrimmed event key:\n%s", spaced.GetCurrent())
	}

	// Copilot's hook body carries no matcher, so the STORED matcher must be empty — else EditArtifact's
	// ValidateHookBody would require the absent matcher in the body and lock the admin out of editing.
	copilotHook := GradedArtifact{
		ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK,
		Artifact:     DerivedArtifact{Body: `{}`, Slug: "cop-hook", Event: "postToolUse", Matcher: "Edit|Write", Command: "npm test"},
	}
	cop := buildArtifactContent(context.Background(), copilotHook, nil, commonv1.IdeType_IDE_TYPE_COPILOT)
	if got := cop.GetHook().GetMatcher(); got != "" {
		t.Errorf("copilot stored matcher = %q, want empty (its body carries no matcher)", got)
	}
	if strings.Contains(cop.GetCurrent(), "matcher") {
		t.Errorf("copilot hook body must not contain a matcher:\n%s", cop.GetCurrent())
	}
	if !strings.Contains(cop.GetCurrent(), `"bash": "npm test"`) {
		t.Errorf("copilot hook body must carry the command in the bash field:\n%s", cop.GetCurrent())
	}

	// AGENT bucket, dominant with NO checked-in agent config: resolves to Codex's own agent
	// path/format. The generic archetype is mapped INTO's vocabulary in the body — the read-only
	// tool scope becomes sandbox_mode="read-only" (never silently write-capable), the "high" tier becomes
	// model_reasoning_effort="high", and the identifier is the kebab slug (not the "Code Reviewer" display
	// name), so the copied file is discoverable and honors the requested scope. AgentContent.Model
	// is empty (Codex carries the tier as effort in the body, not as a model name).
	agent := GradedArtifact{
		ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT,
		Artifact:     DerivedArtifact{Body: "agent body", Slug: "code-reviewer", AgentName: "Code Reviewer", Description: "reviews diffs", Tools: []string{"Read", "Grep"}, Model: "high"},
	}
	agentContent := buildArtifactContent(context.Background(), agent, &promptlibraryv1.RepoTree{}, commonv1.IdeType_IDE_TYPE_CODEX)
	if agentContent.GetAgent() == nil {
		t.Fatal("agent bucket must set the Agent oneof")
	}
	if got := agentContent.GetAgent().GetModel(); got != "" {
		t.Errorf("codex agent model = %q, want empty (tier lives in the body as reasoning_effort)", got)
	}
	// AgentContent.AgentName must be the normalized invocable identifier (matches the file's `name`),
	// not the display-style "Code Reviewer" the panel would otherwise show as an un-invocable name.
	if got := agentContent.GetAgent().GetAgentName(); got != "code-reviewer" {
		t.Errorf("agent name = %q, want the normalized identifier code-reviewer (matches the file name)", got)
	}
	agentBody := agentContent.GetCurrent()
	if !strings.Contains(agentBody, `name = "code-reviewer"`) {
		t.Errorf("codex agent body must key by the kebab slug, not the display name:\n%s", agentBody)
	}
	if !strings.Contains(agentBody, `sandbox_mode = "read-only"`) {
		t.Errorf("read-only tool scope must map to a read-only sandbox:\n%s", agentBody)
	}
	if !strings.Contains(agentBody, `model_reasoning_effort = "high"`) {
		t.Errorf("high tier must map to high reasoning effort:\n%s", agentBody)
	}
	if got := agentContent.GetAgent().GetTargetFile(); got != ".codex/agents/code-reviewer.toml" {
		t.Errorf("codex agent target_file = %q, want .codex/agents/code-reviewer.toml", got)
	}
	if got := agentContent.GetAgent().GetTargetPlacement(); got != promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE {
		t.Errorf("agent placement = %v, want NEW_REFERENCED_FILE", got)
	}
	if got := agentContent.GetAgent().GetTargetTool(); got != commonv1.IdeType_IDE_TYPE_CODEX {
		t.Errorf("agent target_tool = %v, want CODEX", got)
	}

	// A tool-less hook (UNSPECIFIED — the gate would have hidden it) resolves to an empty
	// target rather than crashing: the oneof is still set, the target just empty.
	toollessHook := GradedArtifact{
		ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK,
		Artifact:     DerivedArtifact{Body: `{}`, Slug: "x", Event: "PreToolUse"},
	}
	toolless := buildArtifactContent(context.Background(), toollessHook, nil, commonv1.IdeType_IDE_TYPE_UNSPECIFIED)
	if toolless.GetHook() == nil {
		t.Fatal("tool-less hook still sets the Hook oneof")
	}
	if got := toolless.GetHook().GetTargetFile(); got != "" {
		t.Errorf("tool-less hook target_file = %q, want empty", got)
	}
}

func TestBuildArtifactEvidence_GroundsAddressedIssues(t *testing.T) {
	aggCtx := &promptlibraryv1.AggregatedContext{
		SessionSummary: &promptlibraryv1.SessionSummary{
			TotalSessions:       200,
			QualityDistribution: map[string]int32{"low": 40},
		},
		HighVsLowGap: []*promptlibraryv1.IssueGap{
			{IssueSlug: "missing-tests", HighPct: 10, LowPct: 60, GapPp: 50},
			{IssueSlug: "vague-prompts", HighPct: 5, LowPct: 30, GapPp: 25},
		},
		ReviewThemes: []*promptlibraryv1.ReviewThemeSummary{
			{Theme: "testing", Count: 7, Examples: []string{"add a test"}},
		},
	}
	prCorr := &promptlibraryv1.PRCorrelation{DeltaPct: 12.5}
	ga := GradedArtifact{
		ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE,
		Artifact: DerivedArtifact{
			EvidenceSummary: "teams that skip tests merge slower",
			// Ungrounded slug FIRST: the projection's top_issue must come from the
			// grounded evidence (missing-tests), not the raw addresses_issues[0].
			AddressesIssues: []string{"unknown-slug", "missing-tests"},
		},
	}

	evidence := buildArtifactEvidence(ga, aggCtx, prCorr)

	// Only the slug present in the org-grain gap is grounded — the unknown slug is
	// dropped, never fabricated.
	if len(evidence.GetTaxonomyIssues()) != 1 {
		t.Fatalf("taxonomy_issues = %d, want 1 (unknown slug dropped)", len(evidence.GetTaxonomyIssues()))
	}
	issue := evidence.GetTaxonomyIssues()[0]
	if issue.GetSlug() != "missing-tests" || issue.GetGapPp() != 50 {
		t.Errorf("grounded issue = %+v, want slug=missing-tests gap_pp=50", issue)
	}
	// affected = round(60% * 40 low-bucket sessions) = 24.
	if issue.GetSessionsAffected() != 24 {
		t.Errorf("sessions_affected = %d, want 24", issue.GetSessionsAffected())
	}
	if evidence.GetTotalSessions() != 200 {
		t.Errorf("total_sessions = %d, want 200", evidence.GetTotalSessions())
	}
	if evidence.GetHighLowGapPp() != 50 || evidence.GetAffectedSessions() != 24 {
		t.Errorf("primary gap/affected = %v/%d, want 50/24", evidence.GetHighLowGapPp(), evidence.GetAffectedSessions())
	}
	if evidence.GetPrCorrelation().GetDeltaPct() != 12.5 {
		t.Errorf("pr_correlation delta = %v, want 12.5", evidence.GetPrCorrelation().GetDeltaPct())
	}
	if len(evidence.GetReviewThemes()) != 1 || evidence.GetReviewThemes()[0].GetTheme() != "testing" {
		t.Errorf("review_themes = %v, want one 'testing' theme", evidence.GetReviewThemes())
	}
	if evidence.GetSource() != evidenceSource {
		t.Errorf("source = %q, want %q", evidence.GetSource(), evidenceSource)
	}

	// The inline projection derives from the same evidence and points at the blob.
	projection := buildEvidenceProjection(ga, evidence, "prompt-library/org/artifacts/a1/evidence.json")
	if projection.GetTopIssue() != "missing-tests" {
		t.Errorf("projection top_issue = %q, want missing-tests", projection.GetTopIssue())
	}
	if projection.GetPrDeltaPct() != 12.5 {
		t.Errorf("projection pr_delta_pct = %v, want 12.5", projection.GetPrDeltaPct())
	}
	// The per-cohort shares are inlined from the primary grounded issue so the rule
	// card renders "X% low vs Y% high" with zero the content store reads.
	if projection.GetLowPct() != 60 || projection.GetHighPct() != 10 {
		t.Errorf("projection low/high pct = %v/%v, want 60/10", projection.GetLowPct(), projection.GetHighPct())
	}
	if projection.GetRef().GetKey() == "" {
		t.Error("projection must carry the evidence content ref")
	}
}

func TestBuildArtifactEvidence_ProjectsSecurityIncidents(t *testing.T) {
	// A surfaced security rule on a securityOnly run: the general signals
	// (SessionSummary, HighVsLowGap) are stripped, and security slugs never appear
	// in the quality gap, so the card's badge + count must be projected from the
	// SecuritySummary instead — otherwise the rule shows an empty top_issue and zero
	// counts despite a real incident.
	aggCtx := &promptlibraryv1.AggregatedContext{
		SecurityIncidents: &promptlibraryv1.SecuritySummary{ApiKeyLeak: 5},
	}
	ga := GradedArtifact{
		ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE,
		Artifact: DerivedArtifact{
			EvidenceSummary: "never hardcode API keys",
			// A case/whitespace variant: the gate accepts it, and the projection must
			// store the CANONICAL kebab slug so downstream badges/filters match.
			AddressesIssues: []string{"  API-Key-Leak "},
		},
	}

	evidence := buildArtifactEvidence(ga, aggCtx, nil)

	if len(evidence.GetTaxonomyIssues()) != 1 {
		t.Fatalf("taxonomy_issues = %d, want 1 (security incident projected)", len(evidence.GetTaxonomyIssues()))
	}
	issue := evidence.GetTaxonomyIssues()[0]
	// The incident count lands in incident_count (a discrete event count), NOT
	// sessions_affected — security incidents aren't sessions, and the slug is stored
	// canonical.
	if issue.GetSlug() != "api-key-leak" || issue.GetIncidentCount() != 5 {
		t.Errorf("security issue = %+v, want canonical slug=api-key-leak incident_count=5", issue)
	}
	if issue.GetSessionsAffected() != 0 {
		t.Errorf("security issue sessions_affected = %d, want 0 (not a session metric)", issue.GetSessionsAffected())
	}

	projection := buildEvidenceProjection(ga, evidence, "k")
	if projection.GetTopIssue() != "api-key-leak" {
		t.Errorf("projection top_issue = %q, want api-key-leak (not empty)", projection.GetTopIssue())
	}
	if projection.GetIncidentCount() != 5 {
		t.Errorf("projection incident_count = %d, want 5", projection.GetIncidentCount())
	}
	// Session fields stay consistent (0/0), no affected>total inconsistency.
	if projection.GetAffectedSessions() != 0 || projection.GetTotalSessions() != 0 {
		t.Errorf("projection sessions affected/total = %d/%d, want 0/0 for a security rule",
			projection.GetAffectedSessions(), projection.GetTotalSessions())
	}
}

// TestBuildArtifactEvidence_SecuritySlugFirstKeepsQualityGapPrimary guards the
// regression where a security slug (incident-counted, no gap) at taxonomy_issues[0]
// made the following quality gap fail to become the card headline — the old guard
// keyed on list position (len==1) instead of "first grounded quality gap".
func TestBuildArtifactEvidence_SecuritySlugFirstKeepsQualityGapPrimary(t *testing.T) {
	aggCtx := &promptlibraryv1.AggregatedContext{
		SessionSummary:    &promptlibraryv1.SessionSummary{TotalSessions: 200, QualityDistribution: map[string]int32{"low": 40}},
		HighVsLowGap:      []*promptlibraryv1.IssueGap{{IssueSlug: "missing-tests", HighPct: 10, LowPct: 60, GapPp: 50}},
		SecurityIncidents: &promptlibraryv1.SecuritySummary{ApiKeyLeak: 5},
	}
	ga := GradedArtifact{
		ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE,
		// Security slug FIRST, quality gap SECOND.
		Artifact: DerivedArtifact{EvidenceSummary: "x", AddressesIssues: []string{"api-key-leak", "missing-tests"}},
	}

	evidence := buildArtifactEvidence(ga, aggCtx, nil)

	if evidence.GetHighLowGapPp() != 50 {
		t.Errorf("HighLowGapPp = %v, want 50 (quality gap headline, not zeroed by a leading security slug)", evidence.GetHighLowGapPp())
	}
	// affected = round(60%% of 40 low-bucket sessions) = 24.
	if evidence.GetAffectedSessions() != 24 {
		t.Errorf("AffectedSessions = %d, want 24", evidence.GetAffectedSessions())
	}

	// The inline projection must lead with the QUALITY gap (missing-tests) and inline
	// its per-cohort shares — not the leading security slug (api-key-leak, no gap),
	// which would pair a non-zero HighLowGapPp with 0% low vs 0% high and make the
	// inline evidence line disappear for a security-first artifact.
	projection := buildEvidenceProjection(ga, evidence, "k")
	if projection.GetTopIssue() != "missing-tests" {
		t.Errorf("projection TopIssue = %q, want \"missing-tests\" (the quality gap, not the leading security slug)", projection.GetTopIssue())
	}
	if projection.GetLowPct() != 60 || projection.GetHighPct() != 10 {
		t.Errorf("projection low/high = %v/%v, want 60/10 (from the quality gap, not zeroed by the security slug)", projection.GetLowPct(), projection.GetHighPct())
	}
	if projection.GetHighLowGapPp() != 50 {
		t.Errorf("projection HighLowGapPp = %v, want 50", projection.GetHighLowGapPp())
	}
}

func TestBuildEvidenceProjection_CapsSummary(t *testing.T) {
	long := strings.Repeat("x", evidenceProjectionSummaryMaxChars+50)
	evidence := &promptlibraryv1.ArtifactEvidence{Summary: long}
	projection := buildEvidenceProjection(GradedArtifact{}, evidence, "k")
	if len([]rune(projection.GetSummary())) != evidenceProjectionSummaryMaxChars {
		t.Errorf("projection summary len = %d, want %d (capped)", len([]rune(projection.GetSummary())), evidenceProjectionSummaryMaxChars)
	}
}

func TestFirstRepoTree(t *testing.T) {
	if firstRepoTree(&promptlibraryv1.AggregatedContext{}) != nil {
		t.Error("no trees → nil")
	}
	tree := &promptlibraryv1.RepoTree{RepoFullName: "owner/repo"}
	got := firstRepoTree(&promptlibraryv1.AggregatedContext{RepoTrees: []*promptlibraryv1.RepoTree{tree}})
	if got.GetRepoFullName() != "owner/repo" {
		t.Errorf("firstRepoTree = %v, want the single tree", got)
	}
}

// TestResolveArtifactTarget pins target resolution: the LLM's target wins; when it
// omits one, resolution falls back to the nearest existing rule file above the cited
// paths (nested wins over root), then to defaultRuleFile; placement/reference_in_file
// are normalized and target_tool is derived from the resolved file.
func TestResolveArtifactTarget(t *testing.T) {
	t.Parallel()
	nested := &promptlibraryv1.RepoTree{RuleFiles: []string{"CLAUDE.md", "web/CLAUDE.md"}}
	rule := promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE
	skill := promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL
	hook := promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK
	agent := promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT
	tests := []struct {
		name          string
		artifact      DerivedArtifact
		artifactType  promptlibraryv1.ArtifactType
		tree          *promptlibraryv1.RepoTree
		resolvedTool  commonv1.IdeType
		wantFile      string
		wantPlacement promptlibraryv1.ArtifactTargetPlacement
		wantReference string
		wantTool      commonv1.IdeType
	}{
		{
			name:          "LLM target wins; new referenced file keeps reference",
			artifact:      DerivedArtifact{TargetFile: ".cursor/rules/x.mdc", TargetPlacement: "new_referenced_file", ReferenceInFile: ".cursorrules"},
			artifactType:  rule,
			tree:          nested,
			wantFile:      ".cursor/rules/x.mdc",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantReference: ".cursorrules",
			wantTool:      commonv1.IdeType_IDE_TYPE_CURSOR,
		},
		{
			name:          "append placement drops a stray reference_in_file",
			artifact:      DerivedArtifact{TargetFile: "AGENTS.md", TargetPlacement: "append_to_file", ReferenceInFile: "ignored"},
			artifactType:  rule,
			tree:          nested,
			wantFile:      "AGENTS.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_APPEND_TO_FILE,
			wantReference: "",
			wantTool:      commonv1.IdeType_IDE_TYPE_CODEX,
		},
		{
			name:          "no LLM target: nearest rule file to cited paths (nested wins)",
			artifact:      DerivedArtifact{CitedPaths: []string{"web/app/page.tsx"}},
			artifactType:  rule,
			tree:          nested,
			wantFile:      "web/CLAUDE.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_APPEND_TO_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			name:          "no LLM target, no repo rule files: repo-root default",
			artifact:      DerivedArtifact{CitedPaths: []string{"main.go"}},
			artifactType:  rule,
			tree:          &promptlibraryv1.RepoTree{},
			wantFile:      defaultRuleFile,
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_APPEND_TO_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// A skill with no LLM target must NOT fall back onto a rule file (CLAUDE.md)
			// — it lands in the skills dir as a to-be-created SKILL.md package (the Claude
			// Code skill layout, not a bare .md Claude never loads).
			name:          "skill with no target: skills-dir SKILL.md package, created not appended",
			artifact:      DerivedArtifact{Slug: "/spec-driven-dev"},
			artifactType:  skill,
			tree:          &promptlibraryv1.RepoTree{},
			wantFile:      ".claude/skills/spec-driven-dev/SKILL.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// A skill fallback lands in the skill directory the repo ALREADY uses
			// (Copilot prompts here) with that tool's required .prompt.md extension, so its
			// derived tool matches and the file actually loads — not a Claude default.
			name:          "skill fallback uses the repo's existing skill dir with its extension",
			artifact:      DerivedArtifact{Slug: "/review-pr"},
			artifactType:  skill,
			tree:          &promptlibraryv1.RepoTree{RuleFiles: []string{".github/prompts/existing.prompt.md"}},
			wantFile:      ".github/prompts/review-pr.prompt.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_COPILOT,
		},
		{
			// A skill fallback lands BESIDE an existing skill package at the skills-dir
			// root (.claude/skills/<slug>/SKILL.md), never nested inside the sibling package
			// (.claude/skills/onboard/<slug>/SKILL.md) — a layout no tool loads.
			name:          "skill fallback lands beside an existing skill package, not nested inside it",
			artifact:      DerivedArtifact{Slug: "/refactor"},
			artifactType:  skill,
			tree:          &promptlibraryv1.RepoTree{RuleFiles: []string{".claude/skills/onboard/SKILL.md"}},
			wantFile:      ".claude/skills/refactor/SKILL.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// A mixed repo with BOTH .cursorrules and an existing .claude/skills package:
			// the existing skill dir wins over the Cursor rule-file fallback, so a new skill
			// stays beside the package the repo already uses.
			name:          "skill fallback prefers an existing skill dir over a cursor rule file",
			artifact:      DerivedArtifact{Slug: "/refactor", CitedPaths: []string{"main.go"}},
			artifactType:  skill,
			tree:          &promptlibraryv1.RepoTree{RuleFiles: []string{".claude/skills/onboard/SKILL.md", ".cursorrules"}},
			wantFile:      ".claude/skills/refactor/SKILL.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// A Cursor repo authors skills as blocks in its rule file, so a skill with no
			// LLM target follows the RULE fallback into .cursorrules — NOT a .claude/skills
			// path Cursor never reads.
			name:          "skill fallback in a .cursorrules-only repo appends to the rule file",
			artifact:      DerivedArtifact{Slug: "/audit", CitedPaths: []string{"main.go"}},
			artifactType:  skill,
			tree:          &promptlibraryv1.RepoTree{RuleFiles: []string{".cursorrules"}},
			wantFile:      ".cursorrules",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_APPEND_TO_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CURSOR,
		},
		{
			// A Cursor repo using the per-rule .cursor/rules dir gets a NEW slug-named .mdc
			// (each .mdc auto-loads standalone), again not a Claude skills path.
			name:          "skill fallback in a .cursor/rules repo creates a new .mdc",
			artifact:      DerivedArtifact{Slug: "/audit", CitedPaths: []string{"main.go"}},
			artifactType:  skill,
			tree:          &promptlibraryv1.RepoTree{RuleFiles: []string{".cursor/rules/existing.mdc"}},
			wantFile:      ".cursor/rules/audit.mdc",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CURSOR,
		},
		{
			// A rule fallback into a per-rule dir (.claude/rules) creates a new standalone
			// file with an EMPTY reference — Claude auto-discovers and loads .claude/rules/*.md
			// at launch, so no CLAUDE.md import step is required (or shown).
			name:          "rule fallback into .claude/rules creates a new file with no reference",
			artifact:      DerivedArtifact{Slug: "error-wrapping", CitedPaths: []string{"main.go"}},
			artifactType:  rule,
			tree:          &promptlibraryv1.RepoTree{RuleFiles: []string{".claude/rules/existing.md"}},
			wantFile:      ".claude/rules/error-wrapping.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantReference: "",
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// An unsafe LLM path (traversal) is rejected and falls back, never persisted
			// or rendered as an instruction.
			name:          "unsafe traversal target is rejected and falls back",
			artifact:      DerivedArtifact{TargetFile: "../../etc/passwd", CitedPaths: []string{"main.go"}},
			artifactType:  rule,
			tree:          &promptlibraryv1.RepoTree{},
			wantFile:      defaultRuleFile,
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_APPEND_TO_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			name:          "absolute LLM path is rejected and falls back",
			artifact:      DerivedArtifact{TargetFile: "/etc/hosts", CitedPaths: []string{"main.go"}},
			artifactType:  rule,
			tree:          &promptlibraryv1.RepoTree{},
			wantFile:      defaultRuleFile,
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_APPEND_TO_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// A rule LLM target that echoes a cited SOURCE file (not a rule destination)
			// is rejected — never rendered as "copy this rule into main.go" — and falls back.
			name:          "rule target that is a source file is rejected and falls back",
			artifact:      DerivedArtifact{TargetFile: "backend/main.go", CitedPaths: []string{"backend/main.go"}},
			artifactType:  rule,
			tree:          &promptlibraryv1.RepoTree{RuleFiles: []string{"CLAUDE.md"}},
			wantFile:      "CLAUDE.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_APPEND_TO_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// The repo has only a SKILL file — a rule must not append into it; fall to default.
			name:          "rule fallback skips a skill-only tree",
			artifact:      DerivedArtifact{CitedPaths: []string{"main.go"}},
			artifactType:  rule,
			tree:          &promptlibraryv1.RepoTree{RuleFiles: []string{".claude/skills/onboard.md"}},
			wantFile:      defaultRuleFile,
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_APPEND_TO_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// A Claude rule kept in its own referenced file lands in .claude/rules/ (a
			// per-rule dir), not appended to CLAUDE.md — the new_referenced_file path.
			name:          "claude referenced-rule target in .claude/rules is honored",
			artifact:      DerivedArtifact{TargetFile: ".claude/rules/go-errors.md", TargetPlacement: "new_referenced_file", ReferenceInFile: "CLAUDE.md"},
			artifactType:  rule,
			tree:          nested,
			wantFile:      ".claude/rules/go-errors.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantReference: "CLAUDE.md",
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// A Cursor skill legitimately targets a Cursor rule destination (Cursor has
			// no dedicated skill file) — it must NOT be dropped to a Claude .claude/skills path.
			name:          "cursor skill target in .cursor/rules is accepted",
			artifact:      DerivedArtifact{TargetFile: ".cursor/rules/review.mdc", TargetPlacement: "new_referenced_file", ReferenceInFile: ".cursorrules", Slug: "/review"},
			artifactType:  skill,
			tree:          &promptlibraryv1.RepoTree{RuleFiles: []string{".cursorrules"}},
			wantFile:      ".cursor/rules/review.mdc",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantReference: ".cursorrules",
			wantTool:      commonv1.IdeType_IDE_TYPE_CURSOR,
		},
		{
			// A Cursor skill appended into the shared .cursorrules file — a skill-less tool's
			// rule file doubles as its skill target.
			name:          "cursor skill appended into .cursorrules is accepted",
			artifact:      DerivedArtifact{TargetFile: ".cursorrules", TargetPlacement: "append_to_file", Slug: "/audit"},
			artifactType:  skill,
			tree:          &promptlibraryv1.RepoTree{RuleFiles: []string{".cursorrules"}},
			wantFile:      ".cursorrules",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_APPEND_TO_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CURSOR,
		},
		{
			// new_referenced_file paired with a shared rules file is self-contradictory
			// ("create CLAUDE.md and reference it from CLAUDE.md") — coerced to append.
			name:          "new_referenced_file on a shared rules file is coerced to append",
			artifact:      DerivedArtifact{TargetFile: "CLAUDE.md", TargetPlacement: "new_referenced_file", ReferenceInFile: "CLAUDE.md"},
			artifactType:  rule,
			tree:          nested,
			wantFile:      "CLAUDE.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_APPEND_TO_FILE,
			wantReference: "",
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// The inverse coercion: a per-rule-dir target with the schema's DEFAULT append (the
			// LLM omitted target_placement) is a standalone file to CREATE, so it is coerced to
			// new_referenced_file — else the UI would say "Copy this into .cursor/rules/x.mdc"
			// for a file that does not exist yet.
			name:          "default-append on a per-rule-dir target is coerced to create",
			artifact:      DerivedArtifact{TargetFile: ".cursor/rules/go-errors.mdc"},
			artifactType:  rule,
			tree:          &promptlibraryv1.RepoTree{RuleFiles: []string{".cursor/rules/style.mdc"}},
			wantFile:      ".cursor/rules/go-errors.mdc",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantReference: "",
			wantTool:      commonv1.IdeType_IDE_TYPE_CURSOR,
		},
		{
			// A skill package target with the default append is likewise a file to create —
			// coerced to new_referenced_file so the UI says "Create .claude/skills/onboard/SKILL.md".
			name:          "default-append on a skill package target is coerced to create",
			artifact:      DerivedArtifact{TargetFile: ".claude/skills/onboard/SKILL.md", Slug: "/onboard"},
			artifactType:  skill,
			tree:          nested,
			wantFile:      ".claude/skills/onboard/SKILL.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// The append→create coercion is gated on non-existence: a target that names a per-rule
			// file the repo ALREADY has is a genuine append, so placement stays append (not flipped
			// to "create", which would misrepresent — and risk clobbering — the existing file). The
			// now-inert reference_in_file is cleared by the append guard.
			name:          "append on an existing per-rule-dir file stays append, not coerced to create",
			artifact:      DerivedArtifact{TargetFile: ".claude/rules/existing.md", TargetPlacement: "append_to_file", ReferenceInFile: "CLAUDE.md"},
			artifactType:  rule,
			tree:          &promptlibraryv1.RepoTree{RuleFiles: []string{".claude/rules/existing.md", "CLAUDE.md"}},
			wantFile:      ".claude/rules/existing.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_APPEND_TO_FILE,
			wantReference: "",
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// The inverse contradiction: the LLM asks to CREATE a per-rule file that already
			// exists. You cannot create a file the repo has, so placement resolves to append
			// regardless of the LLM's new_referenced_file, and the reference is cleared.
			name:          "new_referenced_file on an existing per-rule-dir file is coerced to append",
			artifact:      DerivedArtifact{TargetFile: ".claude/rules/existing.md", TargetPlacement: "new_referenced_file", ReferenceInFile: "CLAUDE.md"},
			artifactType:  rule,
			tree:          &promptlibraryv1.RepoTree{RuleFiles: []string{".claude/rules/existing.md", "CLAUDE.md"}},
			wantFile:      ".claude/rules/existing.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_APPEND_TO_FILE,
			wantReference: "",
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// A hallucinated reference_in_file that is not itself a rules file is dropped,
			// so the UI never says "reference it from README.md".
			name:          "non-rule reference_in_file is dropped",
			artifact:      DerivedArtifact{TargetFile: ".claude/rules/x.md", TargetPlacement: "new_referenced_file", ReferenceInFile: "README.md"},
			artifactType:  rule,
			tree:          nested,
			wantFile:      ".claude/rules/x.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantReference: "",
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// reference_in_file must belong to the SAME tool as target_file — a Cursor config
			// cannot import a Claude rule file. A cross-tool importer is dropped so the UI never
			// says "create .claude/rules/x.md and reference it from .cursorrules".
			name:          "cross-tool reference_in_file is dropped",
			artifact:      DerivedArtifact{TargetFile: ".claude/rules/x.md", TargetPlacement: "new_referenced_file", ReferenceInFile: ".cursorrules"},
			artifactType:  rule,
			tree:          nested,
			wantFile:      ".claude/rules/x.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantReference: "",
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// The fallback skill filename must use the normalized slug the gate stores,
			// not the raw LLM slug — no casing leak, no double slash.
			name:          "skill fallback normalizes a messy slug",
			artifact:      DerivedArtifact{Slug: "//Spec-Driven-Dev"},
			artifactType:  skill,
			tree:          &promptlibraryv1.RepoTree{},
			wantFile:      ".claude/skills/spec-driven-dev/SKILL.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// A "/"-traversal slug must NOT escape the skills dir via the fallback path
			// (the LLM target_file is sanitized; skillFileBasename guards the slug path).
			// A non-kebab slug yields the generic "skill" basename, not a leaked fragment.
			name:          "skill fallback neutralizes a forward-slash traversal slug",
			artifact:      DerivedArtifact{Slug: "/../../../etc/cron.d/x"},
			artifactType:  skill,
			tree:          &promptlibraryv1.RepoTree{},
			wantFile:      ".claude/skills/skill/SKILL.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// path.Base only splits on "/", so a backslash traversal must be caught by the
			// kebab-shape guard (not path.Base) — else ".claude/skills/..\..\etc\x.md".
			name:          "skill fallback neutralizes a backslash traversal slug",
			artifact:      DerivedArtifact{Slug: `/..\..\..\etc\passwd`},
			artifactType:  skill,
			tree:          &promptlibraryv1.RepoTree{},
			wantFile:      ".claude/skills/skill/SKILL.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// A skill authored as a standalone frontmatter file must NOT be spliced
			// into the shared AGENTS.md — only Cursor merges skills into its rule file. The
			// rule-file target is rejected and falls back to the skills package dir
			// (.agents/skills/<slug>/SKILL.md), NOT a Claude path the repo's tool can't load.
			name:          "codex skill target in AGENTS.md is rejected, falls back to codex skills dir",
			artifact:      DerivedArtifact{TargetFile: "AGENTS.md", TargetPlacement: "append_to_file", Slug: "/refactor"},
			artifactType:  skill,
			tree:          &promptlibraryv1.RepoTree{RuleFiles: []string{"AGENTS.md"}},
			wantFile:      ".agents/skills/refactor/SKILL.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CODEX,
		},
		{
			// A per-file Copilot instructions target (.github/instructions/*.instructions.md)
			// requires an applyTo frontmatter block the derivation does not author, so a derived
			// RULE is routed to the repo-wide .github/copilot-instructions.md (frontmatter-free,
			// always applied) — here from a target the LLM supplied directly. Shared file → append.
			name:          "copilot per-file instructions rule is routed to the repo-wide file",
			artifact:      DerivedArtifact{TargetFile: ".github/instructions/go.instructions.md", TargetPlacement: "new_referenced_file", ReferenceInFile: ".github/copilot-instructions.md", Slug: "go-errors"},
			artifactType:  rule,
			tree:          nested,
			wantFile:      ".github/copilot-instructions.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_APPEND_TO_FILE,
			wantReference: "",
			wantTool:      commonv1.IdeType_IDE_TYPE_COPILOT,
		},
		{
			// Same routing via the FALLBACK: a Copilot repo whose only rule file is a per-file
			// .github/instructions entry gets a new rule at the repo-wide file, not a new
			// per-file instructions file that would need unauthored applyTo frontmatter.
			name:          "copilot rule fallback routes to the repo-wide instructions file",
			artifact:      DerivedArtifact{Slug: "go-errors", CitedPaths: []string{"main.go"}},
			artifactType:  rule,
			tree:          &promptlibraryv1.RepoTree{RuleFiles: []string{".github/instructions/existing.instructions.md"}},
			wantFile:      ".github/copilot-instructions.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_APPEND_TO_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_COPILOT,
		},
		{
			// The repo's only rule destination is a per-rule dir (.cursor/rules) — a new rule
			// must be its own slug-named file, NOT appended into an existing rule file.
			name:          "rule fallback into a per-rule-dir-only repo creates a new file",
			artifact:      DerivedArtifact{Slug: "go-error-wrapping", CitedPaths: []string{"main.go"}},
			artifactType:  rule,
			tree:          &promptlibraryv1.RepoTree{RuleFiles: []string{".cursor/rules/style.mdc"}},
			wantFile:      ".cursor/rules/go-error-wrapping.mdc",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CURSOR,
		},
		{
			// A Claude skill target as a SKILL.md package (the layout Claude loads) is honored.
			name:          "claude skill package target is honored",
			artifact:      DerivedArtifact{TargetFile: ".claude/skills/onboard/SKILL.md", TargetPlacement: "new_referenced_file", Slug: "/onboard"},
			artifactType:  skill,
			tree:          nested,
			wantFile:      ".claude/skills/onboard/SKILL.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// A bare .claude/skills/<slug>.md (not a SKILL.md package) is one Claude won't
			// load — rejected, then the fallback rebuilds it as a package.
			name:          "claude bare-file skill target is rejected, falls back to a SKILL.md package",
			artifact:      DerivedArtifact{TargetFile: ".claude/skills/onboard.md", Slug: "/onboard"},
			artifactType:  skill,
			tree:          &promptlibraryv1.RepoTree{},
			wantFile:      ".claude/skills/onboard/SKILL.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// SKILL.md is case-sensitive — a lowercase skill.md is not loaded, so the target
			// is rejected and the fallback rebuilds the correctly-cased package.
			name:          "claude wrong-case skill.md target is rejected",
			artifact:      DerivedArtifact{TargetFile: ".claude/skills/onboard/skill.md", Slug: "/onboard"},
			artifactType:  skill,
			tree:          &promptlibraryv1.RepoTree{},
			wantFile:      ".claude/skills/onboard/SKILL.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// A SKILL.md directly under the skills dir (no skill-name folder) is not a valid
			// package — rejected, fallback builds the named-folder layout.
			name:          "claude skill SKILL.md without a name folder is rejected",
			artifact:      DerivedArtifact{TargetFile: ".claude/skills/SKILL.md", Slug: "/onboard"},
			artifactType:  skill,
			tree:          &promptlibraryv1.RepoTree{},
			wantFile:      ".claude/skills/onboard/SKILL.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// HOOK: no LLM target → the resolved tool's canonical hook config, COPY_MERGE
			// (Cursor merges the flat snippet under its .cursor/hooks.json hooks key).
			name:          "cursor hook: canonical hooks.json, copy-merge",
			artifact:      DerivedArtifact{Slug: "secret-guard", Event: "afterFileEdit"},
			artifactType:  hook,
			tree:          &promptlibraryv1.RepoTree{},
			resolvedTool:  commonv1.IdeType_IDE_TYPE_CURSOR,
			wantFile:      ".cursor/hooks.json",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_COPY_MERGE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CURSOR,
		},
		{
			// AGENT: Cursor has file-backed agents at .cursor/agents/<slug>.md, created new.
			name:          "cursor agent: .cursor/agents/<slug>.md, new referenced file",
			artifact:      DerivedArtifact{Slug: "code-reviewer", AgentName: "code-reviewer"},
			artifactType:  agent,
			tree:          &promptlibraryv1.RepoTree{},
			resolvedTool:  commonv1.IdeType_IDE_TYPE_CURSOR,
			wantFile:      ".cursor/agents/code-reviewer.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CURSOR,
		},
		{
			// Kiro hook is a standalone .kiro/hooks/<slug>.kiro.hook manifest → CREATED
			// (NEW_REFERENCED_FILE), never COPY_MERGE (design notes 22/35).
			name:          "kiro hook: .kiro.hook manifest, new referenced file",
			artifact:      DerivedArtifact{Slug: "format-on-save", Event: "PostFileSave"},
			artifactType:  hook,
			tree:          &promptlibraryv1.RepoTree{},
			resolvedTool:  commonv1.IdeType_IDE_TYPE_KIRO,
			wantFile:      ".kiro/hooks/format-on-save.kiro.hook",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_KIRO,
		},
		{
			// Kiro IDE agent format is Markdown at .kiro/agents/<slug>.md.
			name:          "kiro agent: .kiro/agents/<slug>.md, new referenced file",
			artifact:      DerivedArtifact{Slug: "spec-writer", AgentName: "spec-writer"},
			artifactType:  agent,
			tree:          &promptlibraryv1.RepoTree{},
			resolvedTool:  commonv1.IdeType_IDE_TYPE_KIRO,
			wantFile:      ".kiro/agents/spec-writer.md",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_KIRO,
		},
		{
			// Claude hook merges into .claude/settings.json (COPY_MERGE), keeping the
			// settings file intact rather than creating a new one.
			name:          "claude hook: settings.json, copy-merge",
			artifact:      DerivedArtifact{Slug: "secret-protect", Event: "PreToolUse"},
			artifactType:  hook,
			tree:          &promptlibraryv1.RepoTree{},
			resolvedTool:  commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			wantFile:      ".claude/settings.json",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_COPY_MERGE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		},
		{
			// A valid LLM-supplied agent path (the resolved tool's own convention)
			// is honored verbatim rather than recomputed from the slug.
			name:          "codex agent: valid LLM target honored",
			artifact:      DerivedArtifact{Slug: "reviewer", AgentName: "reviewer", TargetFile: ".codex/agents/custom-name.toml"},
			artifactType:  agent,
			tree:          &promptlibraryv1.RepoTree{},
			resolvedTool:  commonv1.IdeType_IDE_TYPE_CODEX,
			wantFile:      ".codex/agents/custom-name.toml",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CODEX,
		},
		{
			// A wrong-tool LLM target (a Claude agent path while the resolved tool is Codex)
			// is rejected; resolution falls back to Codex's own canonical agent path.
			name:          "agent: wrong-tool LLM target falls back to resolved tool",
			artifact:      DerivedArtifact{Slug: "reviewer", AgentName: "reviewer", TargetFile: ".claude/agents/reviewer.md"},
			artifactType:  agent,
			tree:          &promptlibraryv1.RepoTree{},
			resolvedTool:  commonv1.IdeType_IDE_TYPE_CODEX,
			wantFile:      ".codex/agents/reviewer.toml",
			wantPlacement: promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE,
			wantTool:      commonv1.IdeType_IDE_TYPE_CODEX,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			file, placement, reference, tool := resolveArtifactTarget(tc.artifact, tc.artifactType, tc.tree, tc.resolvedTool)
			assert.Equal(t, tc.wantFile, file)
			assert.Equal(t, tc.wantPlacement, placement)
			assert.Equal(t, tc.wantReference, reference)
			assert.Equal(t, tc.wantTool, tool)
		})
	}
}
