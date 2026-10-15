// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations_test

import (
	"context"
	"testing"

	"go.uber.org/mock/gomock"

	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations"
	daomocks "github.com/tetriz-ai/tetriz-harness/recommendations/dao/mocks"
	"github.com/tetriz-ai/tetriz-harness/recommendations/mocks"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// txSpanTracker records, across one Run, whether the terminal MarkTerminalIfRunning and
// PersistArtifacts executed inside a dao.Transactor.InTx span and which span each ran in. The
// atomicity guarantee is that both ran inside the SAME single InTx span, so a persist
// failure rolls back the status claim and no half-written is_latest partition can commit.
type txSpanTracker struct {
	inTxCalls     int // how many InTx spans were opened across the run
	inTxDepth     int // >0 while executing inside an InTx span
	markCalls     int
	markInside    bool
	markSpan      int
	persistCalls  int
	persistInside bool
	persistSpan   int
}

// wireHappyPathEngine builds an Engine whose collaborators all succeed up to the terminal write,
// returning the engine, the mocks whose terminal behaviour each test configures (the execution and
// library DAOs), and the tracker the Transactor/terminal mocks record into. The task_executions row
// loads RUNNING so Run proceeds past the single-in-flight guard, and the quality gate returns one
// VISIBLE artifact so there is a row for PersistArtifacts to write.
func wireHappyPathEngine(t *testing.T, ctrl *gomock.Controller, tracker *txSpanTracker) (*recommendations.Engine, *daomocks.MockITaskExecutionDAO, *daomocks.MockIPromptLibraryDAO) {
	t.Helper()

	tx := daomocks.NewMockTransactor(ctrl)
	tx.EXPECT().InTx(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, fn func(context.Context) error) error {
			tracker.inTxCalls++
			tracker.inTxDepth++
			defer func() { tracker.inTxDepth-- }()
			return fn(ctx)
		}).AnyTimes()

	aggregator := mocks.NewMockcontextAggregator(ctrl)
	aggregator.EXPECT().BuildContext(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&promptlibraryv1.AggregatedContext{}, false, 0, nil).AnyTimes()
	aggregator.EXPECT().ComputePRCorrelation(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()

	deriver := mocks.NewMockartifactDeriver(ctrl)
	deriver.EXPECT().DeriveArtifacts(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&recommendations.DerivationResult{}, []byte("{}"), nil).AnyTimes()

	grader := mocks.NewMockqualityGrader(ctrl)
	grader.EXPECT().Grade(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(),
		gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&recommendations.GradedResult{Artifacts: []recommendations.GradedArtifact{{
			Artifact: recommendations.DerivedArtifact{
				Title: "Test Rule",
				Slug:  "test-rule",
				Body:  "A deterministic rule body long enough to pass the min-length floor.",
				Scope: "project",
			},
			ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE,
			SurfaceState: promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE,
		}}}, nil).AnyTimes()

	libraryDAO := daomocks.NewMockIPromptLibraryDAO(ctrl)

	executionDAO := daomocks.NewMockITaskExecutionDAO(ctrl)
	executionDAO.EXPECT().GetByID(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&model.TaskExecution{
			ID:       "exec-1",
			Status:   taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_RUNNING,
			DedupKey: "repo-1",
		}, nil).AnyTimes()
	executionDAO.EXPECT().Update(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	harnessArtifactDAO := daomocks.NewMockIHarnessArtifactDAO(ctrl)
	harnessArtifactDAO.EXPECT().ListVCSByRepo(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, nil).AnyTimes()

	store := mocks.NewMockartifactStore(ctrl)
	store.EXPECT().PutRunInput(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("input-key", nil).AnyTimes()
	store.EXPECT().PutRunOutput(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("output-key", nil).AnyTimes()
	store.EXPECT().PutArtifactContent(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("content-key", nil).AnyTimes()
	store.EXPECT().PutArtifactEvidence(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("evidence-key", nil).AnyTimes()

	engine := recommendations.NewEngine(
		tx, aggregator, deriver, grader, libraryDAO, executionDAO, harnessArtifactDAO, store,
		recommendations.NewNoopAnalyticsEmitter(),
		func() int { return 30 }, // dataWindowDays
		nil,                      // minReviewThemesPerRepo
		nil,                      // flagEnabled
		nil,                      // newTypesEnabled
	)
	return engine, executionDAO, libraryDAO
}

// TestEngineRun_TerminalWriteIsSingleTransaction proves the atomicity guarantee: on a successful run
// the terminal status claim (MarkTerminalIfRunning) and the is_latest reconcile (PersistArtifacts)
// both execute inside ONE dao.Transactor.InTx span. If either ran outside a transaction, or they ran
// in two separate spans, a persist failure could leave a COMPLETED row with no promoted artifacts.
func TestEngineRun_TerminalWriteIsSingleTransaction(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tracker := &txSpanTracker{}
	engine, executionDAO, libraryDAO := wireHappyPathEngine(t, ctrl, tracker)

	executionDAO.EXPECT().MarkTerminalIfRunning(gomock.Any(), gomock.Any(), gomock.Any(),
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED, gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(context.Context, string, string, taskexecutionsv1.TaskExecutionStatus,
			taskexecutionsv1.TaskExecutionSubStatus, interface{}, string) (bool, error) {
			tracker.markCalls++
			tracker.markInside = tracker.inTxDepth > 0
			tracker.markSpan = tracker.inTxCalls
			return true, nil
		}).Times(1)

	libraryDAO.EXPECT().PersistArtifacts(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(context.Context, string, string, []*model.PromptLibraryItem) error {
			tracker.persistCalls++
			tracker.persistInside = tracker.inTxDepth > 0
			tracker.persistSpan = tracker.inTxCalls
			return nil
		}).Times(1)

	if err := engine.Run(context.Background(), "org-1",
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_UNSPECIFIED, "exec-1", false); err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}

	if tracker.markCalls != 1 || tracker.persistCalls != 1 {
		t.Fatalf("expected MarkTerminalIfRunning and PersistArtifacts to each run once, got mark=%d persist=%d",
			tracker.markCalls, tracker.persistCalls)
	}
	if !tracker.markInside {
		t.Error("MarkTerminalIfRunning ran OUTSIDE an InTx span — the status claim is not transactional")
	}
	if !tracker.persistInside {
		t.Error("PersistArtifacts ran OUTSIDE an InTx span — the is_latest reconcile is not transactional")
	}
	if tracker.markSpan != tracker.persistSpan {
		t.Errorf("MarkTerminalIfRunning (span %d) and PersistArtifacts (span %d) ran in DIFFERENT InTx spans — not atomic",
			tracker.markSpan, tracker.persistSpan)
	}
	if tracker.inTxCalls != 1 {
		t.Errorf("expected exactly ONE InTx span for the terminal write, got %d", tracker.inTxCalls)
	}
}

// TestEngineRun_SupersededAbortsBeforePersist proves the errRunSuperseded branch: when the terminal
// compare-and-set finds the row no longer RUNNING (the stale-run reaper failed it mid-flight and a
// fresh run took the single-in-flight slot), MarkTerminalIfRunning returns marked=false and the run
// MUST abort before PersistArtifacts — so a superseded stale worker never races the fresh run's
// is_latest reconcile. Run acks the redelivery as a no-op (returns nil).
func TestEngineRun_SupersededAbortsBeforePersist(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tracker := &txSpanTracker{}
	engine, executionDAO, libraryDAO := wireHappyPathEngine(t, ctrl, tracker)

	executionDAO.EXPECT().MarkTerminalIfRunning(gomock.Any(), gomock.Any(), gomock.Any(),
		gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(context.Context, string, string, taskexecutionsv1.TaskExecutionStatus,
			taskexecutionsv1.TaskExecutionSubStatus, interface{}, string) (bool, error) {
			tracker.markCalls++
			return false, nil // reaper already failed the row — this worker is superseded
		}).Times(1)

	// The abort-before-persist guarantee: PersistArtifacts must NEVER run once the CAS lost.
	libraryDAO.EXPECT().PersistArtifacts(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	if err := engine.Run(context.Background(), "org-1",
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_UNSPECIFIED, "exec-1", false); err != nil {
		t.Fatalf("a superseded run must ack as a no-op (nil error), got: %v", err)
	}
	if tracker.markCalls != 1 {
		t.Fatalf("expected MarkTerminalIfRunning to run once, got %d", tracker.markCalls)
	}
}

// TestEngineRun_SingleInFlightGuardBlocksNonRunningRow proves the task_executions single-in-flight
// guard: a redelivered task whose row is no longer RUNNING (an at-least-once redelivery, or a row the
// reaper already freed) is acked as a no-op without re-emitting analytics, repeating the paid derive,
// or racing another run's persist. Only GetByID is expected; the strict controller fails the test if
// Run touches any other collaborator.
func TestEngineRun_SingleInFlightGuardBlocksNonRunningRow(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tx := daomocks.NewMockTransactor(ctrl)
	aggregator := mocks.NewMockcontextAggregator(ctrl)
	deriver := mocks.NewMockartifactDeriver(ctrl)
	grader := mocks.NewMockqualityGrader(ctrl)
	libraryDAO := daomocks.NewMockIPromptLibraryDAO(ctrl)
	harnessArtifactDAO := daomocks.NewMockIHarnessArtifactDAO(ctrl)
	store := mocks.NewMockartifactStore(ctrl)

	executionDAO := daomocks.NewMockITaskExecutionDAO(ctrl)
	executionDAO.EXPECT().GetByID(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&model.TaskExecution{
			ID:       "exec-1",
			Status:   taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED,
			DedupKey: "repo-1",
		}, nil).Times(1)

	engine := recommendations.NewEngine(
		tx, aggregator, deriver, grader, libraryDAO, executionDAO, harnessArtifactDAO, store,
		recommendations.NewNoopAnalyticsEmitter(),
		func() int { return 30 }, nil, nil, nil,
	)

	if err := engine.Run(context.Background(), "org-1",
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_UNSPECIFIED, "exec-1", false); err != nil {
		t.Fatalf("a non-RUNNING redelivery must ack as a no-op (nil error), got: %v", err)
	}
}
