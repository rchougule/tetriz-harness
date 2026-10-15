// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations"
	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
	daomocks "github.com/tetriz-ai/tetriz-harness/recommendations/dao/mocks"
	promptmocks "github.com/tetriz-ai/tetriz-harness/recommendations/mocks"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

const (
	hiOrgID  = "11111111-1111-1111-1111-111111111111"
	hiExecID = "22222222-2222-2222-2222-222222222222"
	hiRepoID = "33333333-3333-3333-3333-333333333333"
)

type harnessImproveHarness struct {
	tx        *daomocks.MockTransactor
	execution *daomocks.MockITaskExecutionDAO
	artifacts *daomocks.MockIHarnessArtifactDAO
	repos     *daomocks.MockIRepositoryDAO
	security  *daomocks.MockISecurityFlagDAO
	library   *daomocks.MockIPromptLibraryDAO
	files     *daomocks.MockIHarnessFileDAO
	store     *promptmocks.MockartifactStore
	session   *daomocks.MockISessionInsightDAO
	usage     *daomocks.MockIHarnessUsageRecordsDAO
	classify  *promptmocks.MockharnessIntentClassifier
	fix       *promptmocks.MockharnessFixGenerator
	flagOff   bool // set before Run/RunOrgScan to simulate the detectors feature flag flipped off
	engine    *recommendations.HarnessImproveEngine
}

func newHarnessImproveHarness(t *testing.T) *harnessImproveHarness {
	t.Helper()
	ctrl := gomock.NewController(t)
	h := &harnessImproveHarness{
		tx:        daomocks.NewMockTransactor(ctrl),
		execution: daomocks.NewMockITaskExecutionDAO(ctrl),
		artifacts: daomocks.NewMockIHarnessArtifactDAO(ctrl),
		repos:     daomocks.NewMockIRepositoryDAO(ctrl),
		security:  daomocks.NewMockISecurityFlagDAO(ctrl),
		library:   daomocks.NewMockIPromptLibraryDAO(ctrl),
		files:     daomocks.NewMockIHarnessFileDAO(ctrl),
		store:     promptmocks.NewMockartifactStore(ctrl),
		session:   daomocks.NewMockISessionInsightDAO(ctrl),
		usage:     daomocks.NewMockIHarnessUsageRecordsDAO(ctrl),
		classify:  promptmocks.NewMockharnessIntentClassifier(ctrl),
		fix:       promptmocks.NewMockharnessFixGenerator(ctrl),
	}
	// The terminal write runs inside Transactor.InTx; a pass-through mock runs the fn in-line so each
	// test's terminal-write DAO expectations observe the write. AnyTimes: a run that fails or skips
	// before the terminal write opens no span.
	h.tx.EXPECT().InTx(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }).AnyTimes()
	// A nil DetectorConfig makes the engine fall back to the Go-seed defaults (defaultDetectorConfig).
	h.engine = recommendations.NewHarnessImproveEngine(h.tx, h.execution, h.artifacts, h.repos, h.security, h.library, h.files, h.store, h.session, h.usage, h.classify, h.fix, nil,
		func(string) bool { return !h.flagOff }) // read at call time so a test can flip flagOff before Run
	return h
}

// expectNoPersonalOwners defaults the personal lane's ownership read to "this repo is nobody's primary
// repo", so a test that does not exercise the personal lane issues no personal-artifact or invocation
// read (gomock fails if either is called). AnyTimes because a run that fails before the aggregate
// never reaches it. Called from expectEnrichOK; a personal-lane test overrides it.
func (h *harnessImproveHarness) expectNoPersonalOwners() {
	h.usage.EXPECT().ListPrimaryRepoPersons(gomock.Any(), hiOrgID, hiRepoID, gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()
}

// expectBehavioralReadsEmpty defaults the two behavioral-bridge reads to empty, so a test that does
// not exercise the behavioral lens sees no invoked-skill match (every committed skill reads as
// unmatched) and no cross-repo collision — and the classify client is never called (gomock fails if
// it is). AnyTimes because the reads are candidate-gated: a repo committing no SKILL/COMMAND/AGENT
// skips them entirely (proven separately by TestHarnessImproveEngine_Run_NoBehavioralCandidateSkipsBridgeReads),
// while a candidate-bearing test hits the empty default. Called from expectEnrichOK.
func (h *harnessImproveHarness) expectBehavioralReadsEmpty() {
	h.session.EXPECT().AggregateInvokedSkillBehavior(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(map[string]dao.InvokedSkillBehavior{}, nil).AnyTimes()
	h.artifacts.EXPECT().ListVCSArtifactContentByType(gomock.Any(), hiOrgID, gomock.Any()).
		Return(nil, nil).AnyTimes()
}

// expectEnrichOK defaults the standard reads an aggregate-reaching run makes (the behavioral-bridge
// reads and the gate's suppression reads) to empty, so a test opts into those lenses by overriding them.
func (h *harnessImproveHarness) expectEnrichOK() {
	// Every aggregate-reaching test enriches OK, and the aggregate scope always issues the two
	// behavioral-bridge reads; default them to empty so a test opts into the behavioral lens only by
	// overriding these (and adding a classify expectation).
	h.expectBehavioralReadsEmpty()
	// A run that reaches the findings gate issues the two suppression reads; default them to empty
	// (no dismissed / recently-hidden slug) so a test that does not exercise suppression sees every
	// finding surface. AnyTimes because a run that fails in the aggregate never reaches the gate.
	h.expectGateSuppressionEmpty()
	// The aggregate scope reads the org's live DETECTED partitions for the vanished-type auto-close;
	// default it to none so a test auto-closes nothing unless it seeds a prior finding.
	h.expectLiveDetectedEmpty()
	// The aggregate scope asks which engineers this repo run owns; default to none so a test that
	// does not exercise the personal lane never reads personal artifacts or invocations.
	h.expectNoPersonalOwners()
}

// expectLiveDetectedEmpty defaults the vanished-type auto-close read to no prior live DETECTED
// partitions, so a run auto-closes nothing unless a test overrides it. AnyTimes because a run that
// fails before the aggregate never reaches it.
func (h *harnessImproveHarness) expectLiveDetectedEmpty() {
	h.library.EXPECT().ListLiveDetectedPartitions(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()
}

// expectRepoNamesEmpty defaults the org aggregate's repo-name resolution (ListByIDs) to no rows, so a
// coverage-gap finding falls back to raw repo ids in its reference point. AnyTimes because a RunOrgScan
// that fails before the aggregate never issues it. The resolved-NAMES contract (and the id fallback) is
// covered exhaustively by the pure TestOrgCoverageGapFindings, so the engine tests just need the read
// stubbed rather than asserting names end to end.
func (h *harnessImproveHarness) expectRepoNamesEmpty() {
	h.repos.EXPECT().ListByIDs(gomock.Any(), hiOrgID, gomock.Any()).Return(nil, nil).AnyTimes()
}

// expectGateSuppressionEmpty defaults the findings gate's two suppression reads to empty, so no
// finding is flipped to HIDDEN_FAILED_EVAL unless a test overrides these.
func (h *harnessImproveHarness) expectGateSuppressionEmpty() {
	h.library.EXPECT().ListDismissedSlugs(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()
	h.library.EXPECT().ListRecentHiddenFailedSlugs(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()
}

func runningExec() *model.TaskExecution {
	return &model.TaskExecution{
		ID:             hiExecID,
		OrganizationID: hiOrgID,
		Status:         taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_RUNNING,
		DedupKey:       hiRepoID,
	}
}

// TestHarnessImproveEngine_Run_NotRunningIsNoOp: a redelivered terminal row is acked without any
// enrich, mark, or read (the RUNNING-guard) — at-least-once + the reaper make this safe.
func TestHarnessImproveEngine_Run_NotRunningIsNoOp(t *testing.T) {
	h := newHarnessImproveHarness(t)
	done := runningExec()
	done.Status = taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED
	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(done, nil)
	// No EnrichForDerivation, MarkTerminalIfRunning, or reads — gomock fails if any are called.

	require.NoError(t, h.engine.Run(context.Background(), hiOrgID,
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, hiExecID))
}

// NOTE: this engine is single-tenant — the only pre-read gate to assert is the flag re-check below,
// which gates a run.

// TestHarnessImproveEngine_Run_FlagOffSkipsBeforeReads: a scan task queued before
// the detectors feature flag was flipped off (a rollback + a data scrub) must NOT run the
// detectors and repopulate DETECTED rows after the scrub — the engine re-checks the flag at dequeue and
// SKIPs before enrichment or any read (the Stage-1 gate only protects the enqueue, before fan-out).
func TestHarnessImproveEngine_Run_FlagOffSkipsBeforeReads(t *testing.T) {
	h := newHarnessImproveHarness(t)
	h.flagOff = true
	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(runningExec(), nil)
	// No EnrichForDerivation, no reads — the flag skip fires before enrichment (gomock fails if any called).
	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_SKIPPED, gomock.Any(), gomock.Any(), "").
		Return(true, nil)

	require.NoError(t, h.engine.Run(context.Background(), hiOrgID,
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, hiExecID))
}

// TestHarnessImproveEngine_RunOrgScan_FlagOffSkipsBeforeReads mirrors the per-repo flag re-check for the
// once-per-org relational scan.
func TestHarnessImproveEngine_RunOrgScan_FlagOffSkipsBeforeReads(t *testing.T) {
	h := newHarnessImproveHarness(t)
	h.flagOff = true
	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(runningExec(), nil)
	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_SKIPPED, gomock.Any(), gomock.Any(), "").
		Return(true, nil)

	require.NoError(t, h.engine.RunOrgScan(context.Background(), hiOrgID, "scheduled", hiExecID))
}

// TestHarnessImproveEngine_Run_HappyPath: a RUNNING run enriches, reads the repo's committed VCS
// content (via the VCS-filtered ListVCSByRepo — never an unfiltered read that would surface a
// desktop row) plus the security signal, and — the skeleton has no detectors yet —
// stamps COMPLETED_NO_VISIBLE with no persist.
func TestHarnessImproveEngine_Run_HappyPath(t *testing.T) {
	h := newHarnessImproveHarness(t)
	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(runningExec(), nil)
	h.expectEnrichOK()
	// Tenant-grain committed content — asserted via the VCS-filtered method (the no-leak invariant).
	h.artifacts.EXPECT().ListVCSByRepo(gomock.Any(), hiOrgID, hiRepoID).
		Return([]*model.HarnessArtifact{
			{Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL, Name: "pr"},
		}, nil)
	// The repo row resolves with no tree_key, so the path lenses see an empty tree (no tree read).
	h.repos.EXPECT().GetByID(gomock.Any(), hiOrgID, hiRepoID).Return(&model.Repository{ID: hiRepoID}, nil)
	// Security signal read (over the behavioral window).
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(map[string]int{"api-key-leak": 3}, nil)
	// No findings from the stub → COMPLETED_NO_VISIBLE, no PersistArtifacts.
	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE, gomock.Any(), gomock.Any(), "").
		Return(true, nil)

	require.NoError(t, h.engine.Run(context.Background(), hiOrgID,
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, hiExecID))
}

// TestHarnessImproveEngine_Run_InstructionIntentFinding: a RUNNING run whose committed harness has
// a root CLAUDE.md over the token floor resolves the artifact's file, reads its body from object
// storage, and persists an Instruction-Intent Restructure finding — the whole PART 1 body-fetch +
// PART 3 detector path in one run. The run stamps COMPLETED (findings present).
func TestHarnessImproveEngine_Run_InstructionIntentFinding(t *testing.T) {
	h := newHarnessImproveHarness(t)
	const (
		artifactID  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		fileID      = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		contentHash = "a1b2c3d4"
	)
	// The stored key must be the content-addressed key the engine derives, or the body-fetch
	// validation skips the row (cross-org guard) and no finding is produced.
	// A body comfortably over the 12000-token floor (estimateTokens = len/4).
	overFloorBody := strings.Repeat("x", 13000*4)

	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(runningExec(), nil)
	h.expectEnrichOK()
	h.artifacts.EXPECT().ListVCSByRepo(gomock.Any(), hiOrgID, hiRepoID).
		Return([]*model.HarnessArtifact{
			{
				ID:            artifactID,
				Type:          commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD,
				Name:          "CLAUDE.md",
				SourcePath:    "CLAUDE.md",
				CurrentFileID: fileID,
			},
		}, nil)
	// The repo row resolves with no tree_key, so the path lenses see an empty tree (no tree read).
	h.repos.EXPECT().GetByID(gomock.Any(), hiOrgID, hiRepoID).Return(&model.Repository{ID: hiRepoID}, nil)
	// The artifact's current_file_id resolves to a harness_files row carrying the content store key.
	h.files.EXPECT().ListByIDs(gomock.Any(), hiOrgID, []string{fileID}).
		Return([]*model.HarnessFile{{ID: fileID, ContentHash: contentHash, Content: []byte(overFloorBody)}}, nil)
	// The body read happens OUTSIDE the tenant scope, by the resolved the content store key.
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(map[string]int{}, nil)
	// This test exercises detection, not fixes: the over-floor body's Restructure findings route to the
	// LLM fix lane, so degrade every fix draft (dropped, no body attached) to keep the run advisory.
	h.fix.EXPECT().GenerateHarnessFix(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(recommendations.HarnessFixResult{}, errors.New("fix not under test here")).AnyTimes()

	// Findings present → COMPLETED, then PersistArtifacts receives the Instruction-Intent items.
	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, _ []dao.EvaluatedPartition) error {
			require.NotEmpty(t, items, "expected at least one Instruction-Intent finding")
			for _, item := range items {
				require.NotNil(t, item.Dimension)
				require.Equal(t, promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_INSTRUCTION_INTENT, *item.Dimension)
				require.NotNil(t, item.RecommendationSource)
				require.Equal(t, promptlibraryv1.ArtifactRecommendationSource_ARTIFACT_RECOMMENDATION_SOURCE_DETECTED, *item.RecommendationSource)
				require.NotNil(t, item.HarnessArtifactID)
				require.Equal(t, artifactID, *item.HarnessArtifactID)
				require.Equal(t, hiRepoID, item.ScopeValue)
				require.NotEmpty(t, item.Slug)
				require.NotEmpty(t, item.Title)
				require.Equal(t, int32(5), item.Priority) // priorityLow
			}
			return nil
		})

	require.NoError(t, h.engine.Run(context.Background(), hiOrgID,
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, hiExecID))
}

// TestHarnessImproveEngine_Run_VanishedDetectedTypeAutoCloses: a repo that now commits NO artifact of a
// type that PREVIOUSLY had a live DETECTED finding (the artifact was deleted) must union that vanished
// partition into the evaluated set, so its reconcile-only PersistFindings auto-closes the stale finding.
func TestHarnessImproveEngine_Run_VanishedDetectedTypeAutoCloses(t *testing.T) {
	h := newHarnessImproveHarness(t)
	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(runningExec(), nil)
	// The repo now commits NO artifacts — the previously-detected RULE type has vanished.
	h.artifacts.EXPECT().ListVCSByRepo(gomock.Any(), hiOrgID, hiRepoID).Return(nil, nil)
	h.repos.EXPECT().GetByID(gomock.Any(), hiOrgID, hiRepoID).Return(&model.Repository{ID: hiRepoID}, nil)
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return(map[string]int{}, nil)
	// No behavioral candidate (empty repo) → the bridge reads are skipped; default them anyway.
	h.expectBehavioralReadsEmpty()
	h.expectGateSuppressionEmpty()
	h.expectNoPersonalOwners()
	// A prior live DETECTED RULE finding exists for this repo; its type has no committed artifact this
	// run, so the engine must union it into the evaluated set as a vanished partition.
	vanished := dao.EvaluatedPartition{
		Scope:        promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT,
		ScopeValue:   hiRepoID,
		ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE,
	}
	h.library.EXPECT().ListLiveDetectedPartitions(gomock.Any(), hiOrgID,
		promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, hiRepoID).
		Return([]dao.EvaluatedPartition{vanished}, nil)

	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	// The vanished partition drives a reconcile-only PersistFindings: no findings, evaluated=[RULE].
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, partitions []dao.EvaluatedPartition) error {
			require.Empty(t, items, "a vanished-type close-out persists no findings")
			require.Contains(t, partitions, vanished, "the vanished RULE partition must be in the evaluated set")
			return nil
		})

	require.NoError(t, h.engine.Run(context.Background(), hiOrgID,
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, hiExecID))
}

// TestHarnessImproveEngine_Run_SecurityPermissionFinding: a RUNNING run whose committed harness has
// a PERMISSIONS carrier granting an unbounded `Bash` allow resolves the artifact's file, reads its
// settings.json body from object storage, and persists a PROJECT-scoped HIGH Security
// permission-grant finding — the PART 1 body-fetch (widened to PERMISSIONS) + PART 2 permission-
// inventory detector path in one run. The run stamps COMPLETED (findings present).
func TestHarnessImproveEngine_Run_SecurityPermissionFinding(t *testing.T) {
	h := newHarnessImproveHarness(t)
	const (
		artifactID  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		fileID      = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		contentHash = "c0ffee12"
	)
	// The stored key must be the content-addressed key the engine derives, or the body-fetch
	// validation skips the row (cross-org guard) and no finding is produced.
	settingsBody := `{"permissions":{"allow":["Bash"],"deny":[]}}`

	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(runningExec(), nil)
	h.expectEnrichOK()
	h.artifacts.EXPECT().ListVCSByRepo(gomock.Any(), hiOrgID, hiRepoID).
		Return([]*model.HarnessArtifact{
			{
				ID:            artifactID,
				IdeType:       commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
				Type:          commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_PERMISSIONS,
				Name:          "permissions",
				SourcePath:    ".claude/settings.json#permissions",
				CurrentFileID: fileID,
			},
		}, nil)
	// The repo row resolves with no tree_key, so the path lenses see an empty tree (no tree read).
	h.repos.EXPECT().GetByID(gomock.Any(), hiOrgID, hiRepoID).Return(&model.Repository{ID: hiRepoID}, nil)
	// The PERMISSIONS body is fetched because bodyRequiredForDetection now covers it.
	h.files.EXPECT().ListByIDs(gomock.Any(), hiOrgID, []string{fileID}).
		Return([]*model.HarnessFile{{ID: fileID, ContentHash: contentHash, Content: []byte(settingsBody)}}, nil)
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(map[string]int{}, nil)
	// A permission finding stays ADVISORY — its anchor is the whole settings.json body (secret-density),
	// which is deliberately never sent to the LLM fix lane (see harnessFixLane's SECURITY case). So no
	// GenerateHarnessFix call and no the content store fix store: the finding surfaces with evidence only.

	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, _ []dao.EvaluatedPartition) error {
			var grant *model.PromptLibraryItem
			for _, item := range items {
				if strings.Contains(item.Slug, ":security:permission-grant:") {
					grant = item
				}
			}
			require.NotNil(t, grant, "expected a Security permission-grant finding")
			require.NotNil(t, grant.Dimension)
			require.Equal(t, promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_SECURITY, *grant.Dimension)
			require.Equal(t, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, grant.Scope)
			require.Equal(t, hiRepoID, grant.ScopeValue)
			require.Equal(t, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_PERMISSIONS, grant.ArtifactType)
			require.Equal(t, int32(20), grant.Priority) // priorityHigh
			require.NotNil(t, grant.HarnessArtifactID)
			require.Equal(t, artifactID, *grant.HarnessArtifactID)
			require.Equal(t, "allow Bash (unbounded)", grant.Evidence.GetReferencePoint())
			require.Nil(t, grant.ArtifactContent, "a permission finding is advisory — no suggested body attached")
			require.False(t, grant.Evidence.GetAutoFixable(), "an advisory permission finding is not auto-fixable")
			require.True(t, grant.Evidence.GetAdvisory(), "the permission finding stays advisory")
			return nil
		})

	require.NoError(t, h.engine.Run(context.Background(), hiOrgID,
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, hiExecID))
}

// TestHarnessImproveEngine_Run_NoOpFixStaysAdvisory: a GenerateHarnessFix that returns the committed
// body UNCHANGED is a no-op — enforcing it would be an empty diff that marks the still-unresolved
// finding FIXED, and next scan re-detects it. So the no-op is dropped and the finding stays advisory
// (no attached content), never a phantom applicable fix.
func TestHarnessImproveEngine_Run_NoOpFixStaysAdvisory(t *testing.T) {
	h := newHarnessImproveHarness(t)
	const (
		artifactID  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		fileID      = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		contentHash = "a1b2c3d4"
	)
	overFloorBody := strings.Repeat("- keep changes surgical and always add a regression test\n", 1000)

	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(runningExec(), nil)
	h.expectEnrichOK()
	h.artifacts.EXPECT().ListVCSByRepo(gomock.Any(), hiOrgID, hiRepoID).
		Return([]*model.HarnessArtifact{
			{ID: artifactID, Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD, Name: "CLAUDE.md", SourcePath: "CLAUDE.md", CurrentFileID: fileID},
		}, nil)
	h.repos.EXPECT().GetByID(gomock.Any(), hiOrgID, hiRepoID).Return(&model.Repository{ID: hiRepoID}, nil)
	h.files.EXPECT().ListByIDs(gomock.Any(), hiOrgID, []string{fileID}).
		Return([]*model.HarnessFile{{ID: fileID, ContentHash: contentHash, Content: []byte(overFloorBody)}}, nil)
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return(map[string]int{}, nil)
	// The model returns the body UNCHANGED — a no-op. It is dropped before attach, so the content store's
	// Put* are never called (a strict-mock unexpected-call would fail the test if they were).
	h.fix.EXPECT().GenerateHarnessFix(gomock.Any(), hiOrgID, artifactID, gomock.Any()).
		Return(recommendations.HarnessFixResult{SuggestedContent: overFloorBody, ChangeSummary: "no change", CitedPaths: nil}, nil)

	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, _ []dao.EvaluatedPartition) error {
			var tokenFloor *model.PromptLibraryItem
			for _, item := range items {
				if strings.Contains(item.Slug, ":instruction-intent:token-floor:") {
					tokenFloor = item
				}
			}
			require.NotNil(t, tokenFloor, "the finding survives a no-op fix draft")
			require.Nil(t, tokenFloor.ArtifactContent, "a no-op fix attaches no body")
			require.False(t, tokenFloor.Evidence.GetAutoFixable())
			require.True(t, tokenFloor.Evidence.GetAdvisory(), "a dropped no-op fix leaves the finding advisory")
			return nil
		})

	require.NoError(t, h.engine.Run(context.Background(), hiOrgID,
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, hiExecID))
}

// TestHarnessImproveEngine_Run_ConflictDeadGlobFinding: a RUNNING run whose committed harness has a
// RULE with a frontmatter scoping glob matching no committed file resolves the repo's tree_key,
// reads both the rule body and the repo tree from object storage, and persists a HIGH Conflict
// dead-glob finding — the whole PART 1 repo-tree-path foundation + PART 2 Conflict detector path in
// one run. The run stamps COMPLETED (findings present).
func TestHarnessImproveEngine_Run_ConflictDeadGlobFinding(t *testing.T) {
	h := newHarnessImproveHarness(t)
	const (
		artifactID  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		fileID      = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		contentHash = "ba5eba11"
	)
	// The rule body's stored key must be the content-addressed key the engine derives, or the
	// body-fetch validation skips the row (cross-org guard) and no finding is produced.
	ruleBody := "---\npaths:\n  - \"src/**/*.ts\"\n---\n# scoped rule\n"
	// The committed tree lists only README.md, so the src/**/*.ts glob matches nothing.
	treeBlob := `{"truncated":false,"entries":[{"path":"README.md","type":"blob","sha":"abc"}]}`

	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(runningExec(), nil)
	h.expectEnrichOK()
	h.artifacts.EXPECT().ListVCSByRepo(gomock.Any(), hiOrgID, hiRepoID).
		Return([]*model.HarnessArtifact{
			{
				ID:            artifactID,
				IdeType:       commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
				Type:          commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE,
				Name:          "scoped",
				SourcePath:    ".claude/rules/scoped.md",
				CurrentFileID: fileID,
			},
		}, nil)
	h.repos.EXPECT().GetByID(gomock.Any(), hiOrgID, hiRepoID).
		Return(&model.Repository{ID: hiRepoID, TreeContent: &treeBlob}, nil)
	h.files.EXPECT().ListByIDs(gomock.Any(), hiOrgID, []string{fileID}).
		Return([]*model.HarnessFile{{ID: fileID, ContentHash: contentHash, Content: []byte(ruleBody)}}, nil)
	// Two distinct object reads: the rule body and the repo tree blob.
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(map[string]int{}, nil)
	// dead-glob now routes to the LLM lane — the remove-line fix drops the dead scoping glob. The
	// suggested body references no repo path, so it grounds and attaches as the finding's fixable content.
	h.fix.EXPECT().GenerateHarnessFix(gomock.Any(), hiOrgID, artifactID, gomock.Any()).
		Return(recommendations.HarnessFixResult{SuggestedContent: "# scoped rule\n", ChangeSummary: "Remove the dead scoping glob", CitedPaths: nil}, nil)
	h.store.EXPECT().PutArtifactContent(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return("content/key", nil)
	h.store.EXPECT().PutArtifactEvidence(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return("evidence/key", nil)

	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, _ []dao.EvaluatedPartition) error {
			var deadGlob *model.PromptLibraryItem
			for _, item := range items {
				if strings.Contains(item.Slug, ":conflict:dead-glob:") {
					deadGlob = item
				}
			}
			require.NotNil(t, deadGlob, "expected a Conflict dead-glob finding")
			require.NotNil(t, deadGlob.Dimension)
			require.Equal(t, promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_CONFLICT, *deadGlob.Dimension)
			require.Equal(t, int32(20), deadGlob.Priority) // priorityHigh
			require.NotNil(t, deadGlob.HarnessArtifactID)
			require.Equal(t, artifactID, *deadGlob.HarnessArtifactID)
			require.Equal(t, "src/**/*.ts", deadGlob.Evidence.GetReferencePoint())
			require.NotNil(t, deadGlob.ArtifactContent, "a grounded remove-line fix attaches a suggested body")
			require.True(t, deadGlob.Evidence.GetAutoFixable(), "a finding with an attached fix is auto-fixable")
			return nil
		})

	require.NoError(t, h.engine.Run(context.Background(), hiOrgID,
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, hiExecID))
}

// TestHarnessImproveEngine_Run_BrokenPathDeterministicFix: a broken-path Conflict finding whose
// referenced file has an UNAMBIGUOUS rename target in the tree is fixed in Go (PatchBrokenPath) — the
// LLM fix generator is NEVER called, the grounded suggested body attaches as ArtifactContent, and the
// persisted finding is marked auto-fixable.
func TestHarnessImproveEngine_Run_BrokenPathDeterministicFix(t *testing.T) {
	h := newHarnessImproveHarness(t)
	const (
		artifactID  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		fileID      = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		contentHash = "deadbeef"
		treeKey     = "repo-trees/" + hiOrgID + "/conn/repo/sha.json"
	)
	// A root CLAUDE.md referencing docs/OLD.md, which the tree does not list — but docs/moved/OLD.md
	// (same basename) is the single, unambiguous rename target the deterministic patcher resolves to.
	body := "See docs/OLD.md for the workflow details.\n"
	treeBlob := `{"truncated":false,"entries":[{"path":"README.md","type":"blob","sha":"s"},{"path":"docs/moved/OLD.md","type":"blob","sha":"s"}]}`

	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(runningExec(), nil)
	h.expectEnrichOK()
	h.artifacts.EXPECT().ListVCSByRepo(gomock.Any(), hiOrgID, hiRepoID).
		Return([]*model.HarnessArtifact{
			{ID: artifactID, IdeType: commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD, Name: "CLAUDE.md", SourcePath: "CLAUDE.md", CurrentFileID: fileID},
		}, nil)
	h.repos.EXPECT().GetByID(gomock.Any(), hiOrgID, hiRepoID).Return(&model.Repository{ID: hiRepoID, TreeContent: &treeBlob}, nil)
	h.files.EXPECT().ListByIDs(gomock.Any(), hiOrgID, []string{fileID}).
		Return([]*model.HarnessFile{{ID: fileID, ContentHash: contentHash, Content: []byte(body)}}, nil)
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return(map[string]int{}, nil)
	// The deterministic lane resolves the rename in Go — the LLM fix generator is never invoked.
	h.fix.EXPECT().GenerateHarnessFix(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	// A grounded fix attaches an the content store content + evidence body (its change summary is non-empty).
	h.store.EXPECT().PutArtifactContent(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return("content/key", nil)
	h.store.EXPECT().PutArtifactEvidence(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return("evidence/key", nil)

	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, _ []dao.EvaluatedPartition) error {
			var brokenPath *model.PromptLibraryItem
			for _, item := range items {
				if strings.Contains(item.Slug, ":conflict:broken-path:") {
					brokenPath = item
				}
			}
			require.NotNil(t, brokenPath, "expected a Conflict broken-path finding")
			require.NotNil(t, brokenPath.ArtifactContent, "a grounded deterministic fix attaches a suggested body")
			require.NotEmpty(t, brokenPath.ArtifactContent.GetKey())
			require.NotNil(t, brokenPath.Evidence)
			require.True(t, brokenPath.Evidence.GetAutoFixable(), "a finding with an attached fix is auto-fixable")
			return nil
		})

	require.NoError(t, h.engine.Run(context.Background(), hiOrgID,
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, hiExecID))
}

// TestHarnessImproveEngine_Run_RestructureLLMFix: an Instruction-Intent Restructure finding (root
// CLAUDE.md over the token floor) routes to the LLM lane; the generated fix cites no path (grounds
// vacuously), so its suggested body attaches as ArtifactContent and the finding is auto-fixable.
func TestHarnessImproveEngine_Run_RestructureLLMFix(t *testing.T) {
	h := newHarnessImproveHarness(t)
	const (
		artifactID  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		fileID      = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		contentHash = "a1b2c3d4"
	)
	// A directive-heavy body over the token floor: token-floor fires, but its ~100% directive share
	// keeps the directive-share sub-lens silent, so the run has exactly ONE Restructure (LLM-lane) finding.
	overFloorBody := strings.Repeat("- keep changes surgical and always add a regression test\n", 1000)

	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(runningExec(), nil)
	h.expectEnrichOK()
	h.artifacts.EXPECT().ListVCSByRepo(gomock.Any(), hiOrgID, hiRepoID).
		Return([]*model.HarnessArtifact{
			{ID: artifactID, Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD, Name: "CLAUDE.md", SourcePath: "CLAUDE.md", CurrentFileID: fileID},
		}, nil)
	// No tree_key → empty repo paths; the fix cites none, so grounding is vacuous.
	h.repos.EXPECT().GetByID(gomock.Any(), hiOrgID, hiRepoID).Return(&model.Repository{ID: hiRepoID}, nil)
	h.files.EXPECT().ListByIDs(gomock.Any(), hiOrgID, []string{fileID}).
		Return([]*model.HarnessFile{{ID: fileID, ContentHash: contentHash, Content: []byte(overFloorBody)}}, nil)
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return(map[string]int{}, nil)
	h.fix.EXPECT().GenerateHarnessFix(gomock.Any(), hiOrgID, artifactID, gomock.Any()).
		Return(recommendations.HarnessFixResult{SuggestedContent: "a trimmed, scannable body", ChangeSummary: "Trimmed to essentials", CitedPaths: nil}, nil)
	h.store.EXPECT().PutArtifactContent(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return("content/key", nil)
	h.store.EXPECT().PutArtifactEvidence(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return("evidence/key", nil)

	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, _ []dao.EvaluatedPartition) error {
			var tokenFloor *model.PromptLibraryItem
			for _, item := range items {
				if strings.Contains(item.Slug, ":instruction-intent:token-floor:") {
					tokenFloor = item
				}
			}
			require.NotNil(t, tokenFloor, "expected a token-floor Restructure finding")
			require.NotNil(t, tokenFloor.ArtifactContent, "a grounded LLM fix attaches a suggested body")
			require.NotEmpty(t, tokenFloor.ArtifactContent.GetKey())
			require.True(t, tokenFloor.Evidence.GetAutoFixable(), "a finding with an attached fix is auto-fixable")
			return nil
		})

	require.NoError(t, h.engine.Run(context.Background(), hiOrgID,
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, hiExecID))
}

// TestHarnessImproveEngine_Run_UngroundedLLMFixDropped: an LLM fix that cites a path the repo tree does
// not contain is DROPPED by the gate — its hallucinated body never persists, and the finding stays
// VISIBLE as advisory (no ArtifactContent, not auto-fixable).
func TestHarnessImproveEngine_Run_UngroundedLLMFixDropped(t *testing.T) {
	h := newHarnessImproveHarness(t)
	const (
		artifactID  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		fileID      = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		contentHash = "a1b2c3d4"
	)
	// A directive-heavy body over the token floor: token-floor fires, but its ~100% directive share
	// keeps the directive-share sub-lens silent, so the run has exactly ONE Restructure (LLM-lane) finding.
	overFloorBody := strings.Repeat("- keep changes surgical and always add a regression test\n", 1000)

	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(runningExec(), nil)
	h.expectEnrichOK()
	h.artifacts.EXPECT().ListVCSByRepo(gomock.Any(), hiOrgID, hiRepoID).
		Return([]*model.HarnessArtifact{
			{ID: artifactID, Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD, Name: "CLAUDE.md", SourcePath: "CLAUDE.md", CurrentFileID: fileID},
		}, nil)
	h.repos.EXPECT().GetByID(gomock.Any(), hiOrgID, hiRepoID).Return(&model.Repository{ID: hiRepoID}, nil)
	h.files.EXPECT().ListByIDs(gomock.Any(), hiOrgID, []string{fileID}).
		Return([]*model.HarnessFile{{ID: fileID, ContentHash: contentHash, Content: []byte(overFloorBody)}}, nil)
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return(map[string]int{}, nil)
	// The fix cites a path absent from the (empty) repo tree → ungrounded, dropped by the gate.
	h.fix.EXPECT().GenerateHarnessFix(gomock.Any(), hiOrgID, artifactID, gomock.Any()).
		Return(recommendations.HarnessFixResult{SuggestedContent: "hallucinated body", ChangeSummary: "cites a ghost file", CitedPaths: []string{"docs/does-not-exist.md"}}, nil)
	// No fix persists → the content store's Put* are never called (gomock fails if they are).

	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, _ []dao.EvaluatedPartition) error {
			var tokenFloor *model.PromptLibraryItem
			for _, item := range items {
				if strings.Contains(item.Slug, ":instruction-intent:token-floor:") {
					tokenFloor = item
				}
			}
			require.NotNil(t, tokenFloor, "the finding itself survives even though its fix was dropped")
			require.Nil(t, tokenFloor.ArtifactContent, "an ungrounded fix body is never attached")
			require.Equal(t, promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE, tokenFloor.SurfaceState,
				"a dropped fix leaves the finding VISIBLE as advisory")
			require.False(t, tokenFloor.Evidence.GetAutoFixable(), "no attached fix → not auto-fixable")
			return nil
		})

	require.NoError(t, h.engine.Run(context.Background(), hiOrgID,
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, hiExecID))
}

// TestHarnessImproveEngine_Run_FixGenerateErrorStaysAdvisory: a GenerateHarnessFix error is logged and
// dropped — the finding stays advisory and the run completes successfully (a bad fix draft never fails
// the scan).
func TestHarnessImproveEngine_Run_FixGenerateErrorStaysAdvisory(t *testing.T) {
	h := newHarnessImproveHarness(t)
	const (
		artifactID  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		fileID      = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		contentHash = "a1b2c3d4"
	)
	// A directive-heavy body over the token floor: token-floor fires, but its ~100% directive share
	// keeps the directive-share sub-lens silent, so the run has exactly ONE Restructure (LLM-lane) finding.
	overFloorBody := strings.Repeat("- keep changes surgical and always add a regression test\n", 1000)

	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(runningExec(), nil)
	h.expectEnrichOK()
	h.artifacts.EXPECT().ListVCSByRepo(gomock.Any(), hiOrgID, hiRepoID).
		Return([]*model.HarnessArtifact{
			{ID: artifactID, Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD, Name: "CLAUDE.md", SourcePath: "CLAUDE.md", CurrentFileID: fileID},
		}, nil)
	h.repos.EXPECT().GetByID(gomock.Any(), hiOrgID, hiRepoID).Return(&model.Repository{ID: hiRepoID}, nil)
	h.files.EXPECT().ListByIDs(gomock.Any(), hiOrgID, []string{fileID}).
		Return([]*model.HarnessFile{{ID: fileID, ContentHash: contentHash, Content: []byte(overFloorBody)}}, nil)
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return(map[string]int{}, nil)
	h.fix.EXPECT().GenerateHarnessFix(gomock.Any(), hiOrgID, artifactID, gomock.Any()).
		Return(recommendations.HarnessFixResult{}, errors.New("tetriz-py down"))
	// No fix persists → the content store's Put* are never called.

	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, _ []dao.EvaluatedPartition) error {
			var tokenFloor *model.PromptLibraryItem
			for _, item := range items {
				if strings.Contains(item.Slug, ":instruction-intent:token-floor:") {
					tokenFloor = item
				}
			}
			require.NotNil(t, tokenFloor, "the finding survives a failed fix draft")
			require.Nil(t, tokenFloor.ArtifactContent, "a failed fix draft attaches no body")
			require.False(t, tokenFloor.Evidence.GetAutoFixable())
			return nil
		})

	// The run does NOT fail — a fix-generate error degrades to advisory.
	require.NoError(t, h.engine.Run(context.Background(), hiOrgID,
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, hiExecID))
}

// ------------------------------------------------------------------------------------------------
// RunOrgScan — the ONCE-per-org relational scan that runs the ORG-scoped Security detectors
// (OrgMcpFindings, OrgCoverageGapFindings) at ORG scope. These prove the org findings persist ONCE at
// Scope=ORG, ScopeValue="" (never duplicated per repo), the run is repeatable, a resolved HOOK or
// MCP_SERVER partition auto-closes a prior finding, and ORG-scope suppression hides a dismissed one.
// ------------------------------------------------------------------------------------------------

const (
	hiRepo1ID = "44444444-4444-4444-4444-444444444444"
	hiRepo2ID = "55555555-5555-5555-5555-555555555555"
)

// orgRunningExec is a RUNNING ORG-scan execution: its DedupKey holds the orgID (the single-in-flight
// key), NOT a repo scope value — the ORG findings scope on an EMPTY scope value, never the DedupKey.
func orgRunningExec() *model.TaskExecution {
	return &model.TaskExecution{
		ID:             hiExecID,
		OrganizationID: hiOrgID,
		Status:         taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_RUNNING,
		DedupKey:       hiOrgID,
	}
}

// expectOrgEnrichOK resolves the enricher and defaults the ORG gate's two suppression reads to empty.
// Unlike expectEnrichOK it does NOT stub the behavioral-bridge reads — the ORG scan issues its own
// ListVCSArtifactContentByType(HOOK) with a specific return, which a blanket stub would shadow.
func (h *harnessImproveHarness) expectOrgEnrichOK() {
	h.expectGateSuppressionEmpty()
	h.expectRepoNamesEmpty()
	h.expectOrgFixGenAny()
}

// expectOrgFixGenAny stubs the ORG fix-generate leg: every org finding (coverage-gap, mcp) now routes
// through produceOrgFixes → GenerateHarnessFix (a generate-from-scratch, so an empty repo tree grounds
// the empty cited set), and the grounded body attaches via the store. AnyTimes so a run that emits zero
// org findings (a fully-covered / auto-close case) still passes without any fix call.
func (h *harnessImproveHarness) expectOrgFixGenAny() {
	h.fix.EXPECT().GenerateHarnessFix(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(recommendations.HarnessFixResult{SuggestedContent: "# generated guard hook\n", ChangeSummary: "Add a UserPromptSubmit guard hook", CitedPaths: nil}, nil).AnyTimes()
	h.store.EXPECT().PutArtifactContent(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return("content/key", nil).AnyTimes()
	h.store.EXPECT().PutArtifactEvidence(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return("evidence/key", nil).AnyTimes()
}

// TestHarnessImproveEngine_RunOrgScan_PersistsOrgFindingsOnce: a multi-repo org with MCP sprawl +
// prod-pointed servers, an open security surface, and one repo lacking a UserPromptSubmit hook
// persists the MCP + coverage-gap findings ONCE at Scope=ORG, ScopeValue="" (NOT duplicated per repo),
// and reports both ORG partitions fully evaluated.
func TestHarnessImproveEngine_RunOrgScan_PersistsOrgFindingsOnce(t *testing.T) {
	h := newHarnessImproveHarness(t)
	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(orgRunningExec(), nil)
	h.expectOrgEnrichOK()
	// (b) an open security surface (positive count) — the coverage-gap precondition.
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(map[string]int{"api-key-leak": 2}, nil)
	// (a) MCP sprawl (gh base across dev+prod) + two prod-pointed servers (gh-prod, db-prod).
	h.session.EXPECT().DistinctMcpServerNames(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return([]string{"gh-prod", "gh-dev", "db-prod"}, nil)
	// (c) two eligible repos; repo1 commits a UserPromptSubmit hook, repo2 does not → repo2 uncovered.
	h.artifacts.EXPECT().ListRepoIDsWithLiveVCSArtifacts(gomock.Any(), hiOrgID).
		Return([]string{hiRepo1ID, hiRepo2ID}, nil)
	h.artifacts.EXPECT().ListVCSArtifactContentByType(gomock.Any(), hiOrgID,
		[]commonv1.HarnessArtifactType{commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_HOOK}).
		Return([]dao.VCSArtifactContent{
			{Name: "UserPromptSubmit", RepositoryID: hiRepo1ID, ContentHash: "h1"},
			{Name: "PostToolUse:Bash", RepositoryID: hiRepo1ID, ContentHash: "h2"},
		}, nil)

	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, evaluated []dao.EvaluatedPartition) error {
			require.NotEmpty(t, items)
			mcpCount, coverageGapCount := 0, 0
			for _, item := range items {
				// One org-wide row set: EVERY finding is Scope=ORG with an EMPTY scope value, never per-repo.
				require.Equal(t, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_ORG, item.Scope)
				require.Empty(t, item.ScopeValue)
				require.NotNil(t, item.Dimension)
				require.Equal(t, promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_SECURITY, *item.Dimension)
				switch item.ArtifactType {
				case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_MCP_SERVER:
					mcpCount++
				case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK:
					coverageGapCount++
				}
			}
			require.Equal(t, 3, mcpCount, "2 prod-pointed + 1 sprawl MCP findings, emitted once at ORG scope")
			require.Equal(t, 1, coverageGapCount, "1 coverage-gap finding for the single uncovered repo")
			require.ElementsMatch(t, []dao.EvaluatedPartition{
				{Scope: promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_ORG, ScopeValue: "", ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_MCP_SERVER},
				{Scope: promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_ORG, ScopeValue: "", ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK},
			}, evaluated, "exactly the two ORG partitions, both fully evaluated")
			return nil
		})

	require.NoError(t, h.engine.RunOrgScan(context.Background(), hiOrgID, "scheduled", hiExecID))
}

// TestHarnessImproveEngine_RunOrgScan_CoverageGapGeneratesNewContent locks the GENERATE contract for an
// ORG coverage-gap finding: the fix leg is called with an EMPTY CurrentContent (the create-from-scratch
// signal, never a rewrite), and the attached content keeps its "before" (Original) EMPTY so the panel
// renders all-added new content — not the fetch-failed fallback that copies the suggestion into before.
func TestHarnessImproveEngine_RunOrgScan_CoverageGapGeneratesNewContent(t *testing.T) {
	h := newHarnessImproveHarness(t)
	h.expectRepoNamesEmpty()
	const hookBody = "hooks:\n  UserPromptSubmit:\n    - command: guard.sh\n"

	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(orgRunningExec(), nil)
	h.expectGateSuppressionEmpty()
	// A security surface + one uncovered repo → exactly one coverage-gap finding, and no mcp servers.
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(map[string]int{"api-key-leak": 2}, nil)
	h.session.EXPECT().DistinctMcpServerNames(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return([]string{}, nil)
	h.artifacts.EXPECT().ListRepoIDsWithLiveVCSArtifacts(gomock.Any(), hiOrgID).
		Return([]string{hiRepo1ID}, nil)
	h.artifacts.EXPECT().ListVCSArtifactContentByType(gomock.Any(), hiOrgID, gomock.Any()).
		Return([]dao.VCSArtifactContent{}, nil)

	// The generate leg MUST receive an EMPTY CurrentContent (the create signal) for a non-empty entity type.
	h.fix.EXPECT().GenerateHarnessFix(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, input recommendations.HarnessFixInput) (recommendations.HarnessFixResult, error) {
			require.Empty(t, input.CurrentContent, "an org coverage-gap fix generates from scratch — no committed body")
			require.NotEmpty(t, input.EntityType)
			return recommendations.HarnessFixResult{SuggestedContent: hookBody, ChangeSummary: "Add a UserPromptSubmit guard hook", CitedPaths: nil}, nil
		})
	// The stored content's "before" MUST stay empty (all-added), and "after" is the generated hook.
	var stored *promptlibraryv1.ArtifactContent
	h.store.EXPECT().PutArtifactContent(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, content *promptlibraryv1.ArtifactContent) (string, error) {
			stored = content
			return "content/key", nil
		})
	h.store.EXPECT().PutArtifactEvidence(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return("evidence/key", nil)

	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	var gap *model.PromptLibraryItem
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, _ []dao.EvaluatedPartition) error {
			for _, item := range items {
				if item.ArtifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK {
					gap = item
				}
			}
			return nil
		})

	require.NoError(t, h.engine.RunOrgScan(context.Background(), hiOrgID, "scheduled", hiExecID))

	require.NotNil(t, stored, "the generated fix content was stored")
	require.Empty(t, stored.GetOriginal(), "a generated fix has no before — Original stays empty for an all-added diff")
	require.Equal(t, hookBody, stored.GetCurrent(), "the after is the generated hook body")
	require.NotNil(t, gap, "the coverage-gap finding persisted")
	require.NotNil(t, gap.ArtifactContent, "the generated fix attached to the finding")
	require.True(t, gap.Evidence.GetAutoFixable(), "an attached generated fix is auto-fixable")
	require.False(t, gap.Evidence.GetAdvisory(), "an attached fix flips the finding out of advisory")
}

// TestHarnessImproveEngine_RunOrgScan_FixGenerateErrorStaysAdvisory is the ORG-path counterpart of
// TestHarnessImproveEngine_Run_FixGenerateErrorStaysAdvisory: an org coverage-gap fix whose generate
// leg errors is logged and dropped by the shared runCappedFixGeneration degrade path — the finding still
// persists (advisory, no attached content) and the run completes, never failing the scan on a bad draft.
func TestHarnessImproveEngine_RunOrgScan_FixGenerateErrorStaysAdvisory(t *testing.T) {
	h := newHarnessImproveHarness(t)
	h.expectRepoNamesEmpty()

	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(orgRunningExec(), nil)
	h.expectGateSuppressionEmpty()
	// A security surface + one uncovered repo → exactly one coverage-gap finding, and no mcp servers.
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(map[string]int{"api-key-leak": 2}, nil)
	h.session.EXPECT().DistinctMcpServerNames(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return([]string{}, nil)
	h.artifacts.EXPECT().ListRepoIDsWithLiveVCSArtifacts(gomock.Any(), hiOrgID).
		Return([]string{hiRepo1ID}, nil)
	h.artifacts.EXPECT().ListVCSArtifactContentByType(gomock.Any(), hiOrgID, gomock.Any()).
		Return([]dao.VCSArtifactContent{}, nil)

	// The generate leg errors — the org fix degrades, nothing to store (the content store Put* are never called).
	h.fix.EXPECT().GenerateHarnessFix(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(recommendations.HarnessFixResult{}, errors.New("tetriz-py down"))

	// The advisory coverage-gap still SURFACES (evidence only), so the run completes.
	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	var gap *model.PromptLibraryItem
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, _ []dao.EvaluatedPartition) error {
			for _, item := range items {
				if item.ArtifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK {
					gap = item
				}
			}
			return nil
		})

	// The run does NOT fail — an org fix-generate error degrades to advisory.
	require.NoError(t, h.engine.RunOrgScan(context.Background(), hiOrgID, "scheduled", hiExecID))
	require.NotNil(t, gap, "the coverage-gap finding survives a failed fix draft")
	require.Nil(t, gap.ArtifactContent, "a failed org fix draft attaches no body")
	require.False(t, gap.Evidence.GetAutoFixable(), "no fix attached → not auto-fixable")
	require.True(t, gap.Evidence.GetAdvisory(), "the finding stays advisory when its fix draft fails")
}

// TestHarnessImproveEngine_RunOrgScan_Idempotent: two identical RunOrgScan invocations yield the
// identical ORG finding slug set — the run carries no accumulated state and is repeatable.
func TestHarnessImproveEngine_RunOrgScan_Idempotent(t *testing.T) {
	h := newHarnessImproveHarness(t)
	h.expectRepoNamesEmpty()
	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(orgRunningExec(), nil).Times(2)
	h.expectGateSuppressionEmpty()
	h.expectOrgFixGenAny()
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(map[string]int{"api-key-leak": 2}, nil).Times(2)
	h.session.EXPECT().DistinctMcpServerNames(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return([]string{"gh-prod", "gh-dev", "db-prod"}, nil).Times(2)
	h.artifacts.EXPECT().ListRepoIDsWithLiveVCSArtifacts(gomock.Any(), hiOrgID).
		Return([]string{hiRepo1ID, hiRepo2ID}, nil).Times(2)
	h.artifacts.EXPECT().ListVCSArtifactContentByType(gomock.Any(), hiOrgID, gomock.Any()).
		Return([]dao.VCSArtifactContent{{Name: "UserPromptSubmit", RepositoryID: hiRepo1ID, ContentHash: "h1"}}, nil).Times(2)
	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any(), gomock.Any(), "").
		Return(true, nil).Times(2)

	var runs [][]string
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, _ []dao.EvaluatedPartition) error {
			slugs := make([]string, 0, len(items))
			for _, item := range items {
				slugs = append(slugs, item.Slug)
			}
			sort.Strings(slugs)
			runs = append(runs, slugs)
			return nil
		}).Times(2)

	require.NoError(t, h.engine.RunOrgScan(context.Background(), hiOrgID, "scheduled", hiExecID))
	require.NoError(t, h.engine.RunOrgScan(context.Background(), hiOrgID, "scheduled", hiExecID))
	require.Len(t, runs, 2)
	require.NotEmpty(t, runs[0])
	require.Equal(t, runs[0], runs[1], "identical inputs yield the identical ORG finding slug set")
}

// TestHarnessImproveEngine_RunOrgScan_CoverageGapAutoCloses: once EVERY eligible repo commits a
// UserPromptSubmit hook the coverage-gap lens emits no finding, yet the HOOK ORG partition is still
// FULLY evaluated — so its stale is_latest auto-closes (the N→0 reconcile the persist consumes).
func TestHarnessImproveEngine_RunOrgScan_CoverageGapAutoCloses(t *testing.T) {
	h := newHarnessImproveHarness(t)
	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(orgRunningExec(), nil)
	h.expectOrgEnrichOK()
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(map[string]int{"api-key-leak": 2}, nil)
	h.session.EXPECT().DistinctMcpServerNames(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return([]string{"gh-prod", "gh-dev", "db-prod"}, nil)
	h.artifacts.EXPECT().ListRepoIDsWithLiveVCSArtifacts(gomock.Any(), hiOrgID).
		Return([]string{hiRepo1ID, hiRepo2ID}, nil)
	// BOTH repos now commit a UserPromptSubmit hook (repo2's is matcher-qualified, same event segment)
	// → no uncovered repo → no coverage-gap finding.
	h.artifacts.EXPECT().ListVCSArtifactContentByType(gomock.Any(), hiOrgID, gomock.Any()).
		Return([]dao.VCSArtifactContent{
			{Name: "UserPromptSubmit", RepositoryID: hiRepo1ID, ContentHash: "h1"},
			{Name: "UserPromptSubmit:*", RepositoryID: hiRepo2ID, ContentHash: "h2"},
		}, nil)

	// MCP findings still surface → COMPLETED.
	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, evaluated []dao.EvaluatedPartition) error {
			for _, item := range items {
				require.NotEqual(t, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK, item.ArtifactType,
					"no coverage-gap (HOOK) finding once every repo is covered")
			}
			require.Contains(t, evaluated, dao.EvaluatedPartition{
				Scope: promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_ORG, ScopeValue: "",
				ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK,
			}, "the HOOK partition evaluates even with zero findings → the auto-close reconcile input")
			return nil
		})

	require.NoError(t, h.engine.RunOrgScan(context.Background(), hiOrgID, "scheduled", hiExecID))
}

// TestHarnessImproveEngine_RunOrgScan_McpAutoCloses: once the org runs ZERO MCP servers the MCP lens
// emits no finding, yet the MCP_SERVER ORG partition is still FULLY evaluated — so its stale is_latest
// auto-closes (the symmetric N→0 reconcile the coverage-gap test proves for the HOOK partition).
func TestHarnessImproveEngine_RunOrgScan_McpAutoCloses(t *testing.T) {
	h := newHarnessImproveHarness(t)
	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(orgRunningExec(), nil)
	h.expectOrgEnrichOK()
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(map[string]int{"api-key-leak": 2}, nil)
	// No MCP servers used anywhere in the org → OrgMcpFindings emits nothing.
	h.session.EXPECT().DistinctMcpServerNames(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return([]string{}, nil)
	// One eligible repo, uncovered, on an open security surface → the coverage-gap lens still fires, so
	// the run has a visible finding (COMPLETED) while the MCP partition is the N→0 case under test.
	h.artifacts.EXPECT().ListRepoIDsWithLiveVCSArtifacts(gomock.Any(), hiOrgID).
		Return([]string{hiRepo1ID}, nil)
	h.artifacts.EXPECT().ListVCSArtifactContentByType(gomock.Any(), hiOrgID, gomock.Any()).
		Return([]dao.VCSArtifactContent{}, nil)

	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, evaluated []dao.EvaluatedPartition) error {
			for _, item := range items {
				require.NotEqual(t, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_MCP_SERVER, item.ArtifactType,
					"no MCP finding once the org runs zero MCP servers")
			}
			require.Contains(t, evaluated, dao.EvaluatedPartition{
				Scope: promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_ORG, ScopeValue: "",
				ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_MCP_SERVER,
			}, "the MCP_SERVER partition evaluates even with zero findings → the auto-close reconcile input")
			return nil
		})

	require.NoError(t, h.engine.RunOrgScan(context.Background(), hiOrgID, "scheduled", hiExecID))
}

// TestHarnessImproveEngine_RunOrgScan_DormantGapNotClosed: uncovered repos still exist but NO security
// flag surfaced this run, so the coverage-gap lens stays silent (no threat to surface). This DORMANT
// gap must NOT auto-close the standing coverage-gap: the {ORG, "", HOOK} partition is kept OUT of the
// eval set so PersistFindings does not reconcile it and the prior finding's is_latest survives. The MCP
// partition still evaluates (telemetry always concludes), so the run has an eval set and the persist
// guard is still reached.
func TestHarnessImproveEngine_RunOrgScan_DormantGapNotClosed(t *testing.T) {
	h := newHarnessImproveHarness(t)
	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(orgRunningExec(), nil)
	h.expectOrgEnrichOK()
	// NO security flags this run (no positive count) → the coverage-gap lens is dormant.
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(map[string]int{}, nil)
	// No MCP servers → no MCP finding, but the MCP partition still evaluates (always-concluded telemetry).
	h.session.EXPECT().DistinctMcpServerNames(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return([]string{}, nil)
	// One eligible repo with NO UserPromptSubmit hook → uncovered, so the gap PERSISTS (dormant).
	h.artifacts.EXPECT().ListRepoIDsWithLiveVCSArtifacts(gomock.Any(), hiOrgID).
		Return([]string{hiRepo1ID}, nil)
	h.artifacts.EXPECT().ListVCSArtifactContentByType(gomock.Any(), hiOrgID, gomock.Any()).
		Return([]dao.VCSArtifactContent{}, nil)

	// No visible finding this run → COMPLETED_NO_VISIBLE.
	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, evaluated []dao.EvaluatedPartition) error {
			for _, item := range items {
				require.NotEqual(t, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK, item.ArtifactType,
					"a dormant coverage-gap emits no HOOK finding")
			}
			require.NotContains(t, evaluated, dao.EvaluatedPartition{
				Scope: promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_ORG, ScopeValue: "",
				ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK,
			}, "the dormant HOOK partition is NOT evaluated → a standing coverage-gap's is_latest is not cleared")
			require.Contains(t, evaluated, dao.EvaluatedPartition{
				Scope: promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_ORG, ScopeValue: "",
				ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_MCP_SERVER,
			}, "the MCP partition still evaluates → the run always has an eval set")
			return nil
		})

	require.NoError(t, h.engine.RunOrgScan(context.Background(), hiOrgID, "scheduled", hiExecID))
}

// TestHarnessImproveEngine_RunOrgScan_OrgScopeSuppression: a dismissed ORG slug suppresses its matching
// ORG finding — proving the gate's scope arg is load-bearing for the (ORG, "") combination. The
// suppression reads must be issued at Scope=ORG with an EMPTY scope value; the sole coverage-gap finding
// flips VISIBLE→HIDDEN_FAILED_EVAL, so with no other visible finding the run stamps COMPLETED_NO_VISIBLE
// yet still persists the hidden row.
func TestHarnessImproveEngine_RunOrgScan_OrgScopeSuppression(t *testing.T) {
	h := newHarnessImproveHarness(t)
	h.expectRepoNamesEmpty()
	// The coverage-gap slug depends only on the org id (via orgSecuritySlug), so calling the exported
	// detector yields the exact slug the run will emit — no hand-replicated hashing.
	gapFindings := recommendations.OrgCoverageGapFindings(recommendations.OrgSecurityAggregate{
		OrgID:              hiOrgID,
		SecurityFlagCounts: map[string]int{"api-key-leak": 2},
		HookCoverageByRepo: map[string]bool{hiRepo1ID: false},
	}, hiExecID)
	require.Len(t, gapFindings, 1)
	dismissedSlug := gapFindings[0].Slug

	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(orgRunningExec(), nil)
	// produceOrgFixes runs BEFORE the gate (like the per-repo path), so the coverage-gap fix is generated
	// even though the finding is about to be suppressed; stub it (AnyTimes).
	h.expectOrgFixGenAny()
	// The suppression reads MUST carry Scope=ORG and an EMPTY scope value (the ORG-scope convention);
	// the dismissed set returns the coverage-gap slug so that finding is hidden.
	h.library.EXPECT().ListDismissedSlugs(gomock.Any(), hiOrgID,
		promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_ORG, "").
		Return([]string{dismissedSlug}, nil)
	h.library.EXPECT().ListRecentHiddenFailedSlugs(gomock.Any(), hiOrgID,
		promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_ORG, "", gomock.Any(), gomock.Any()).
		Return(nil, nil)
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return(map[string]int{"api-key-leak": 2}, nil)
	// No MCP servers → the coverage-gap finding is the ONLY finding, so suppressing it leaves the run
	// with zero visible findings.
	h.session.EXPECT().DistinctMcpServerNames(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).
		Return([]string{}, nil)
	h.artifacts.EXPECT().ListRepoIDsWithLiveVCSArtifacts(gomock.Any(), hiOrgID).
		Return([]string{hiRepo1ID}, nil)
	h.artifacts.EXPECT().ListVCSArtifactContentByType(gomock.Any(), hiOrgID, gomock.Any()).
		Return([]dao.VCSArtifactContent{}, nil)

	// The sole visible finding is suppressed → COMPLETED_NO_VISIBLE, yet the hidden row still persists.
	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, _ []dao.EvaluatedPartition) error {
			require.Len(t, items, 1, "the dismissed coverage-gap finding is still persisted, hidden")
			require.Equal(t, dismissedSlug, items[0].Slug)
			require.Equal(t, promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL,
				items[0].SurfaceState, "the dismissed ORG finding flips to HIDDEN_FAILED_EVAL")
			return nil
		})

	require.NoError(t, h.engine.RunOrgScan(context.Background(), hiOrgID, "scheduled", hiExecID))
}
