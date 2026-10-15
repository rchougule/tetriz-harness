// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations_test

import (
	"context"
	"testing"

	"go.uber.org/mock/gomock"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// dodDeriveResult seeds one artifact of every type: the existing visible rule (reused,
// cites main.go) plus a template, skill, hook, and agent shaped to clear the REAL gate for
// a CLAUDE_CODE-resolved run. The hook body is valid JSON that mentions its
// event/matcher/command (hookBodyAgrees); the agent cites no paths and its body has no
// path-shaped tokens, so agent path-grounding trivially passes.
func dodDeriveResult() *recommendations.DerivationResult {
	refactor := "refactor"
	return &recommendations.DerivationResult{
		Rules: deriveResult().Rules,
		Templates: []recommendations.DerivedArtifact{{
			Title: "Refactor prompt template", Slug: "refactor-prompt-template",
			Body:     "Use this prompt when refactoring a module for clarity and tests.",
			Scope:    "project",
			Priority: "medium", Confidence: 0.8, TaskType: &refactor,
		}},
		Skills: []recommendations.DerivedArtifact{{
			Title: "Onboarding skill", Slug: "onboarding-flow",
			Body:     "Step-by-step onboarding workflow that is long enough to clear the min.",
			Scope:    "project",
			Priority: "medium", Confidence: 0.8,
		}},
		Hooks: []recommendations.DerivedArtifact{{
			Title: "Run tests after edit", Slug: "test-runner",
			Body:     `{"hooks":{"PostToolUse":[{"matcher":"Edit|Write|MultiEdit","hooks":[{"type":"command","command":"go test ./..."}]}]}}`,
			Scope:    "project",
			Priority: "high", Confidence: 0.9,
			Event: "PostToolUse", Matcher: "Edit|Write|MultiEdit", Command: "go test ./...",
		}},
		Agents: []recommendations.DerivedArtifact{{
			Title: "Code reviewer", Slug: "code-reviewer",
			Body:     "A specialist subagent that reviews diffs for concurrency and clarity.",
			Scope:    "project",
			Priority: "high", Confidence: 0.9,
			AgentName:   "code-reviewer",
			Description: "Delegate when a diff needs a concurrency and clarity review.",
		}},
	}
}

// dodAggContext resolves the dominant tool to CLAUDE_CODE up front (the hook/agent target
// path is derived from it) and grounds the rule's cited main.go via the repo tree. Session
// low>=0/high>=0 clears the template session floor (the real gate's min buckets are 0).
func dodAggContext() *promptlibraryv1.AggregatedContext {
	return &promptlibraryv1.AggregatedContext{
		OrgId:              testOrgID,
		SessionSummary:     &promptlibraryv1.SessionSummary{TotalSessions: 100, QualityDistribution: map[string]int32{"low": 20}},
		RepoTrees:          []*promptlibraryv1.RepoTree{{RepoFullName: testProject, DirectoryStructure: ".: main.go"}},
		ResolvedTargetTool: commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		SecurityIncidents:  &promptlibraryv1.SecuritySummary{},
	}
}

func programSuppressionReads(h *harness) {
	h.library.EXPECT().ListDismissedSlugs(gomock.Any(), testOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, testRepoUUID).
		Return(nil, nil).AnyTimes()
	h.library.EXPECT().ListRecentHiddenFailedSlugs(gomock.Any(), testOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, testRepoUUID, gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()
	h.library.EXPECT().ListPinnedSlugs(gomock.Any(), testOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, testRepoUUID).
		Return(nil, nil).AnyTimes()
}

func itemByType(items []*model.PromptLibraryItem, t promptlibraryv1.ArtifactType) *model.PromptLibraryItem {
	for _, item := range items {
		if item.ArtifactType == t {
			return item
		}
	}
	return nil
}

func hookOrAgentContent(contents []*promptlibraryv1.ArtifactContent, wantHook bool) *promptlibraryv1.ArtifactContent {
	for _, c := range contents {
		if wantHook && c.GetHook() != nil {
			return c
		}
		if !wantHook && c.GetAgent() != nil {
			return c
		}
	}
	return nil
}

// TestEngine_Run_DoD_HookAndAgentVisible is the end-to-end DoD proof: a seeded derive run
// through the REAL QualityGate persists a VISIBLE hook + agent with correct tool-scoped
// targets, and the three existing types (rule/template/skill) stay regression-green.
func TestEngine_Run_DoD_HookAndAgentVisible(t *testing.T) {
	h := newRealGateHarness(t)
	h.program(stageNone, false, nil) // real gate is used; the mock grader stub goes uncalled
	h.deriveOverride = dodDeriveResult()
	h.aggOverride = dodAggContext()
	programSuppressionReads(h)

	// Repo admitted at admission (review floor met) + the feature enabled, so all five types are in scope.
	if err := h.engine.Run(context.Background(), testOrgID,
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, testExecID, false); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if h.terminalStatus != taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED {
		t.Fatalf("terminal status = %v, want COMPLETED", h.terminalStatus)
	}
	if len(h.persisted) != 5 {
		t.Fatalf("persisted %d rows, want 5 (rule/template/skill/hook/agent all VISIBLE)", len(h.persisted))
	}
	for _, item := range h.persisted {
		if item.SurfaceState != promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE {
			t.Errorf("row %s (type %v) = %v, want VISIBLE", item.Slug, item.ArtifactType, item.SurfaceState)
		}
		if item.ScopeValue != testRepoUUID {
			t.Errorf("row %s ScopeValue = %q, want %q", item.Slug, item.ScopeValue, testRepoUUID)
		}
	}

	// Hook row + its content blob (target derived from the resolved CLAUDE_CODE tool).
	hookRow := itemByType(h.persisted, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK)
	if hookRow == nil {
		t.Fatal("no HOOK row persisted")
	}
	if hookRow.Slug != "test-runner" || hookRow.Priority != 20 {
		t.Errorf("hook row slug/priority = %q/%d, want test-runner/20", hookRow.Slug, hookRow.Priority)
	}
	hook := hookOrAgentContent(h.capturedContents, true).GetHook()
	if hook == nil {
		t.Fatal("no HookContent captured")
	}
	if hook.GetTargetFile() != ".claude/settings.json" ||
		hook.GetTargetPlacement() != promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_COPY_MERGE ||
		hook.GetTargetTool() != commonv1.IdeType_IDE_TYPE_CLAUDE_CODE {
		t.Errorf("hook target = %q/%v/%v, want .claude/settings.json/COPY_MERGE/CLAUDE_CODE",
			hook.GetTargetFile(), hook.GetTargetPlacement(), hook.GetTargetTool())
	}
	if hook.GetEvent() != "PostToolUse" || hook.GetMatcher() != "Edit|Write|MultiEdit" || hook.GetCommand() != "go test ./..." {
		t.Errorf("hook event/matcher/command not carried verbatim: %q/%q/%q", hook.GetEvent(), hook.GetMatcher(), hook.GetCommand())
	}

	// Agent row + content: a new standalone file in the resolved tool's agent dir.
	agentRow := itemByType(h.persisted, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT)
	if agentRow == nil {
		t.Fatal("no AGENT row persisted")
	}
	if agentRow.Slug != "code-reviewer" {
		t.Errorf("agent row slug = %q, want code-reviewer", agentRow.Slug)
	}
	agent := hookOrAgentContent(h.capturedContents, false).GetAgent()
	if agent == nil {
		t.Fatal("no AgentContent captured")
	}
	if agent.GetTargetFile() != ".claude/agents/code-reviewer.md" ||
		agent.GetTargetPlacement() != promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE ||
		agent.GetTargetTool() != commonv1.IdeType_IDE_TYPE_CLAUDE_CODE {
		t.Errorf("agent target = %q/%v/%v, want .claude/agents/code-reviewer.md/NEW_REFERENCED_FILE/CLAUDE_CODE",
			agent.GetTargetFile(), agent.GetTargetPlacement(), agent.GetTargetTool())
	}
	if agent.GetAgentName() != "code-reviewer" {
		t.Errorf("agent name = %q, want code-reviewer", agent.GetAgentName())
	}

	// Regression: the three existing types are unchanged by adding the two new buckets.
	if r := itemByType(h.persisted, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE); r == nil || r.Slug != "always-add-a-test" {
		t.Errorf("rule row missing/renamed: %+v", r)
	}
	if tmpl := itemByType(h.persisted, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_TEMPLATE); tmpl == nil || tmpl.Slug != "refactor-prompt-template" {
		t.Errorf("template row missing/renamed: %+v", tmpl)
	}
	// Skills keep the "/" slug prefix (invoked as /slug); hooks/agents are bare kebab.
	if s := itemByType(h.persisted, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL); s == nil || s.Slug != "/onboarding-flow" {
		t.Errorf("skill row missing/renamed: %+v", s)
	}
}

// TestEngine_Run_DoD_NewTypesFlagOffDropsHookAndAgent proves the flag gate: with the
// new-types flag OFF the same seed persists ONLY the three existing types — the hook and
// agent are type-scope dropped (drop-not-persist), never persisted as HIDDEN.
func TestEngine_Run_DoD_NewTypesFlagOffDropsHookAndAgent(t *testing.T) {
	h := newRealGateHarness(t)
	h.program(stageNone, false, nil)
	h.deriveOverride = dodDeriveResult()
	h.aggOverride = dodAggContext()
	h.newTypesOff = true // flag OFF → gate drops the hook/agent buckets
	programSuppressionReads(h)

	if err := h.engine.Run(context.Background(), testOrgID,
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, testExecID, false); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if h.terminalStatus != taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED {
		t.Fatalf("terminal status = %v, want COMPLETED", h.terminalStatus)
	}
	if len(h.persisted) != 3 {
		t.Fatalf("persisted %d rows, want 3 (only rule/template/skill; hook+agent dropped)", len(h.persisted))
	}
	for _, item := range h.persisted {
		if item.ArtifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK ||
			item.ArtifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT {
			t.Errorf("a %v row was persisted with the new-types flag OFF; it must be dropped, not hidden", item.ArtifactType)
		}
		if item.SurfaceState != promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE {
			t.Errorf("existing-type row %s = %v, want VISIBLE (unaffected by the flag)", item.Slug, item.SurfaceState)
		}
	}
}
