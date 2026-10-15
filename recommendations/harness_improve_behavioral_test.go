// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations"
	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

const (
	behavioralArtifactID  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	behavioralFileID      = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	behavioralContentHash = "5ec1a1de"
)

// expectBehavioralRunUpToClassify programs the enrich-then-detect Run of a repo whose committed
// harness is a single SKILL named "pr" with a body, an out-of-band ("mechanical" completion band
// [0.5,1.0]) org-wide behavioral match, and no cross-repo collision — everything up to but NOT
// including the classify call and the terminal write, which each test programs to drive a specific
// enrich outcome.
func expectBehavioralRunUpToClassify(h *harnessImproveHarness) {
	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(runningExec(), nil)
	h.artifacts.EXPECT().ListVCSByRepo(gomock.Any(), hiOrgID, hiRepoID).
		Return([]*model.HarnessArtifact{
			{
				ID:            behavioralArtifactID,
				Type:          commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
				Name:          "pr",
				SourcePath:    ".claude/skills/pr/SKILL.md",
				CurrentFileID: behavioralFileID,
			},
		}, nil)
	h.repos.EXPECT().GetByID(gomock.Any(), hiOrgID, hiRepoID).Return(&model.Repository{ID: hiRepoID}, nil)
	// The body is served from the harness_files.content column (bodies-as-column). A plain body with no
	// path references, so the Conflict broken-path lens contributes nothing — the behavioral lens is the
	// only possible finder.
	h.files.EXPECT().ListByIDs(gomock.Any(), hiOrgID, []string{behavioralFileID}).
		Return([]*model.HarnessFile{{ID: behavioralFileID, ContentHash: behavioralContentHash, Content: []byte("PR review helper skill")}}, nil)
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(map[string]int{}, nil)
	// Out-of-band behavior for "pr": completion 0.20 is below the mechanical band's [0.5,1.0].
	h.session.EXPECT().AggregateInvokedSkillBehavior(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(map[string]dao.InvokedSkillBehavior{"pr": {Invocations: 10, CompletionRate: 0.20}}, nil)
	h.artifacts.EXPECT().ListVCSArtifactContentByType(gomock.Any(), hiOrgID, gomock.Any()).
		Return(nil, nil)
	h.expectGateSuppressionEmpty()
	h.expectLiveDetectedEmpty()
	h.expectNoPersonalOwners()
	// behavioral-deviation now routes to the LLM lane (a skill rewrite toward its intent); stub the
	// generate + attach. AnyTimes so the below-floor / ambiguous / classify-error drop tests (which
	// persist no finding) still pass without any fix call.
	h.fix.EXPECT().GenerateHarnessFix(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(recommendations.HarnessFixResult{SuggestedContent: "PR review helper skill, scoped to code review\n", ChangeSummary: "Align the skill to its review intent", CitedPaths: nil}, nil).AnyTimes()
	h.store.EXPECT().PutArtifactContent(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return("content/key", nil).AnyTimes()
	h.store.EXPECT().PutArtifactEvidence(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return("evidence/key", nil).AnyTimes()
}

func runBehavioral(t *testing.T, h *harnessImproveHarness) error {
	t.Helper()
	return h.engine.Run(context.Background(), hiOrgID,
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, hiExecID)
}

// TestHarnessImproveEngine_Run_BehavioralDeviationFinding: a committed SKILL whose org-wide behavior
// is out of its classified intent class's band persists ONE Instruction-Intent behavioral-deviation
// finding carrying the classifier's confidence and intent_class.
func TestHarnessImproveEngine_Run_BehavioralDeviationFinding(t *testing.T) {
	h := newHarnessImproveHarness(t)
	expectBehavioralRunUpToClassify(h)
	h.classify.EXPECT().ClassifyHarnessIntent(gomock.Any(), hiOrgID, behavioralArtifactID, gomock.Any()).
		Return(recommendations.HarnessIntentResult{IntentClass: "mechanical", Confidence: 0.83}, nil)

	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, _ []dao.EvaluatedPartition) error {
			var behavioral *model.PromptLibraryItem
			for _, item := range items {
				if item.Evidence.GetIntentClass() != "" {
					behavioral = item
				}
			}
			require.NotNil(t, behavioral, "expected a behavioral-deviation finding")
			require.NotNil(t, behavioral.Dimension)
			require.Equal(t, promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_INSTRUCTION_INTENT, *behavioral.Dimension)
			require.Equal(t, int32(10), behavioral.Priority) // priorityMedium
			require.Equal(t, float32(0.83), behavioral.ConfidenceScore)
			require.Equal(t, "mechanical", behavioral.Evidence.GetIntentClass())
			require.Equal(t, behavioralArtifactID, *behavioral.HarnessArtifactID)
			require.Equal(t, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL, behavioral.ArtifactType)
			require.Contains(t, behavioral.Slug, ":instruction-intent:behavioral-deviation:")
			return nil
		})

	require.NoError(t, runBehavioral(t, h))
}

// TestHarnessImproveEngine_Run_BehavioralBelowFloorDropped: a classification below the confidence
// floor is dropped, so no behavioral finding is persisted (COMPLETED_NO_VISIBLE, no PersistArtifacts).
func TestHarnessImproveEngine_Run_BehavioralBelowFloorDropped(t *testing.T) {
	h := newHarnessImproveHarness(t)
	expectBehavioralRunUpToClassify(h)
	h.classify.EXPECT().ClassifyHarnessIntent(gomock.Any(), hiOrgID, behavioralArtifactID, gomock.Any()).
		Return(recommendations.HarnessIntentResult{IntentClass: "mechanical", Confidence: 0.30}, nil)
	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE, gomock.Any(), gomock.Any(), "").
		Return(true, nil)

	require.NoError(t, runBehavioral(t, h))
}

// TestHarnessImproveEngine_Run_BehavioralAmbiguousDropped: an ambiguous class is dropped regardless
// of confidence — no finding.
func TestHarnessImproveEngine_Run_BehavioralAmbiguousDropped(t *testing.T) {
	h := newHarnessImproveHarness(t)
	expectBehavioralRunUpToClassify(h)
	h.classify.EXPECT().ClassifyHarnessIntent(gomock.Any(), hiOrgID, behavioralArtifactID, gomock.Any()).
		Return(recommendations.HarnessIntentResult{IntentClass: "ambiguous", Confidence: 0.99}, nil)
	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE, gomock.Any(), gomock.Any(), "").
		Return(true, nil)

	require.NoError(t, runBehavioral(t, h))
}

// TestHarnessImproveEngine_Run_BehavioralClassifyErrorDroppedAndContinues: a classify error drops the
// one artifact and the run still completes (never fails) — one bad artifact never wedges the scan.
func TestHarnessImproveEngine_Run_BehavioralClassifyErrorDroppedAndContinues(t *testing.T) {
	h := newHarnessImproveHarness(t)
	expectBehavioralRunUpToClassify(h)
	h.classify.EXPECT().ClassifyHarnessIntent(gomock.Any(), hiOrgID, behavioralArtifactID, gomock.Any()).
		Return(recommendations.HarnessIntentResult{}, errors.New("tetriz-py 502"))
	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE, gomock.Any(), gomock.Any(), "").
		Return(true, nil)

	require.NoError(t, runBehavioral(t, h))
}

// TestHarnessImproveEngine_Run_NoBehavioralCandidateSkipsBridgeReads: a repo committing ZERO
// SKILL/COMMAND/AGENT (an instruction-only repo) can emit no behavioral finding, so the two org-wide
// behavioral-bridge reads (the full-window invoked-skill aggregate + the org collision inventory) are
// SKIPPED — proven by gomock .Times(0). The run still completes.
func TestHarnessImproveEngine_Run_NoBehavioralCandidateSkipsBridgeReads(t *testing.T) {
	h := newHarnessImproveHarness(t)
	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(runningExec(), nil)
	// A single RULE — no behavioral candidate. No current_file_id, so no body fetch either.
	h.artifacts.EXPECT().ListVCSByRepo(gomock.Any(), hiOrgID, hiRepoID).
		Return([]*model.HarnessArtifact{
			{ID: "r-1", Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE, Name: "scoped", SourcePath: ".claude/rules/scoped.md"},
		}, nil)
	h.repos.EXPECT().GetByID(gomock.Any(), hiOrgID, hiRepoID).Return(&model.Repository{ID: hiRepoID}, nil)
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return(map[string]int{}, nil)
	// The heavy org-wide bridge reads MUST be skipped — the candidate-less repo can emit no finding.
	h.session.EXPECT().AggregateInvokedSkillBehavior(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	h.artifacts.EXPECT().ListVCSArtifactContentByType(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	h.classify.EXPECT().ClassifyHarnessIntent(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	h.expectGateSuppressionEmpty()
	h.expectLiveDetectedEmpty()
	h.expectNoPersonalOwners()

	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE, gomock.Any(), gomock.Any(), "").
		Return(true, nil)

	require.NoError(t, runBehavioral(t, h))
}

// TestHarnessImproveEngine_Run_BehavioralDedupsClassifyByContent: three committed SKILLs sharing ONE
// body content are classified with EXACTLY ONE ClassifyHarnessIntent call (dedup by content), and the
// single resulting intent drives a behavioral-deviation finding for EVERY one of the three — no lost
// coverage, no per-artifact LLM blowup.
func TestHarnessImproveEngine_Run_BehavioralDedupsClassifyByContent(t *testing.T) {
	h := newHarnessImproveHarness(t)
	const (
		id1, id2, id3 = "a1111111-1111-1111-1111-111111111111", "a2222222-2222-2222-2222-222222222222", "a3333333-3333-3333-3333-333333333333"
		f1, f2, f3    = "f1111111-1111-1111-1111-111111111111", "f2222222-2222-2222-2222-222222222222", "f3333333-3333-3333-3333-333333333333"
		h1, h2, h3    = "c0deca11", "dec0de22", "fee1dead"
	)
	// Same name, same body — three copies of one skill across the repo, served from the content column.
	sharedBody := []byte("PR review helper skill")

	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(runningExec(), nil)
	h.artifacts.EXPECT().ListVCSByRepo(gomock.Any(), hiOrgID, hiRepoID).
		Return([]*model.HarnessArtifact{
			{ID: id1, Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL, Name: "pr", SourcePath: ".claude/skills/pr/SKILL.md", CurrentFileID: f1},
			{ID: id2, Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL, Name: "pr", SourcePath: "a/.claude/skills/pr/SKILL.md", CurrentFileID: f2},
			{ID: id3, Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL, Name: "pr", SourcePath: "b/.claude/skills/pr/SKILL.md", CurrentFileID: f3},
		}, nil)
	h.repos.EXPECT().GetByID(gomock.Any(), hiOrgID, hiRepoID).Return(&model.Repository{ID: hiRepoID}, nil)
	// Three files, distinct content hashes, each carrying the SAME body in its content column — so the
	// classify dedup (by content) is what collapses the three into one LLM call.
	h.files.EXPECT().ListByIDs(gomock.Any(), hiOrgID, []string{f1, f2, f3}).
		Return([]*model.HarnessFile{
			{ID: f1, ContentHash: h1, Content: sharedBody},
			{ID: f2, ContentHash: h2, Content: sharedBody},
			{ID: f3, ContentHash: h3, Content: sharedBody},
		}, nil)
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return(map[string]int{}, nil)
	// Out-of-band "pr" behavior (completion 0.20 below the mechanical band), no collision.
	h.session.EXPECT().AggregateInvokedSkillBehavior(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(map[string]dao.InvokedSkillBehavior{"pr": {Invocations: 10, CompletionRate: 0.20}}, nil)
	h.artifacts.EXPECT().ListVCSArtifactContentByType(gomock.Any(), hiOrgID, gomock.Any()).Return(nil, nil)

	// EXACTLY ONE classify call for the three same-content skills.
	h.classify.EXPECT().ClassifyHarnessIntent(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(recommendations.HarnessIntentResult{IntentClass: "mechanical", Confidence: 0.83}, nil).
		Times(1)
	h.expectGateSuppressionEmpty()
	h.expectLiveDetectedEmpty()
	h.expectNoPersonalOwners()
	// Each of the three same-content skills drives its own behavioral finding, each now routed to the
	// LLM lane — three generate + attach calls.
	h.fix.EXPECT().GenerateHarnessFix(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(recommendations.HarnessFixResult{SuggestedContent: "PR review helper skill, scoped to code review\n", ChangeSummary: "Align the skill to its review intent", CitedPaths: nil}, nil).AnyTimes()
	h.store.EXPECT().PutArtifactContent(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return("content/key", nil).AnyTimes()
	h.store.EXPECT().PutArtifactEvidence(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return("evidence/key", nil).AnyTimes()

	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, _ []dao.EvaluatedPartition) error {
			seen := map[string]bool{}
			for _, item := range items {
				if item.Evidence.GetIntentClass() != "" && item.HarnessArtifactID != nil {
					seen[*item.HarnessArtifactID] = true
				}
			}
			require.Len(t, seen, 3, "all three same-content skills get the one classification")
			require.True(t, seen[id1] && seen[id2] && seen[id3], "each artifact drives its own finding")
			return nil
		})

	require.NoError(t, runBehavioral(t, h))
}
