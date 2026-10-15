// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/mock/gomock"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations"
	daomocks "github.com/tetriz-ai/tetriz-harness/recommendations/dao/mocks"
	"github.com/tetriz-ai/tetriz-harness/recommendations/mocks"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

const (
	testOrgID    = "org-1"
	testProject  = "owner/repo"
	testRepoUUID = "repo-uuid-1"
	testExecID   = "exec-1"
	testWindow   = 90
)

// TestMain registers the engine's Prometheus metrics on the default registry once for the whole test
// binary, because metric registration is opt-in (no import-time side effect) and the outcome-metrics
// test reads them back through prometheus.DefaultGatherer.
func TestMain(m *testing.M) {
	recommendations.RegisterMetrics(prometheus.DefaultRegisterer)
	os.Exit(m.Run())
}

// stage names a pipeline step that a failure test injects an error at.
type stage int

const (
	stageNone stage = iota
	stageGetByID
	stageEnrich
	stageBuildContext
	stagePRCorrelation
	stagePutInput
	stageDerive
	stageRecommend
	stagePutOutput
	stageGrade
	stagePersist
)

type emitCall struct {
	orgID, scopeValue, execID string
	trigger                   taskexecutionsv1.TaskTriggerType
}

type harness struct {
	tx              *daomocks.MockTransactor
	agg             *mocks.MockcontextAggregator
	deriver         *mocks.MockartifactDeriver
	grader          *mocks.MockqualityGrader
	store           *mocks.MockartifactStore
	library         *daomocks.MockIPromptLibraryDAO
	execution       *daomocks.MockITaskExecutionDAO
	harnessArtifact *daomocks.MockIHarnessArtifactDAO
	emitter         *mocks.MockAnalyticsEmitter
	engine          *recommendations.Engine

	// committedArtifacts is what ListVCSByRepo returns for the derive flow's additive-dedup
	//. Empty by default (no committed artifacts); an test sets it to exercise the
	// committed_artifact gate drop + the LLM avoid-hint.
	committedArtifacts []*model.HarnessArtifact
	deriveSawAggCtx    *promptlibraryv1.AggregatedContext        // the aggCtx DeriveArtifacts received (carries the committed-slug avoid-hint)
	gradeSawCommitted  map[recommendations.CommittedKey]struct{} // the committedSlugs arg Grade received (the gate-side dedup set)

	supersede                bool // when true, MarkTerminalIfRunning reports the row was already terminal (reaped)
	flagOff                  bool // when true, flagEnabled returns false → derive-time rollout-gate skip
	newTypesOff              bool // when true, newTypesEnabled returns false → the gate drops hook/agent buckets (slice-6 drop reads this)
	resumedWithInput         bool // true → GetByID returns a row whose InputPayload is already set, modelling a retry that already emitted past that point
	emits                    []emitCall
	buildCtxRepoID           string
	buildCtxFrom             time.Time
	buildCtxTo               time.Time
	buildCtxSecOnly          bool
	gradeScopeVal            string
	gradeSecOnly             bool
	gradeRepoTree            *promptlibraryv1.RepoTree
	gradeSecurity            *promptlibraryv1.SecuritySummary
	deriveOverride           *recommendations.DerivationResult  // when set, the deriver stub returns this instead of deriveResult()
	aggOverride              *promptlibraryv1.AggregatedContext // when set, BuildContext returns this instead of aggResult()
	capturedContents         []*promptlibraryv1.ArtifactContent // every *ArtifactContent passed to PutArtifactContent, in persist order
	persisted                []*model.PromptLibraryItem
	terminalStatus           taskexecutionsv1.TaskExecutionStatus
	terminalSubStatus        taskexecutionsv1.TaskExecutionSubStatus // captured substatus arg of MarkTerminalIfRunning
	enriched                 bool                                    // when true, BuildContext returns enriched=true → the engine applies the theme floor
	reviewCommentsClassified int                                     // the classified-comment base rate BuildContext returns; reaches the recommender context only, never the derive context
	themeFloor               int                                     // minReviewThemesPerRepo the engine reads (0 → no theme-floor gate)
	terminalErrMsg           *string
	inputWritten             bool // set when PutRunInput runs; the emit must happen before this
	emitSawInputWritten      bool // value of inputWritten captured at emit time (must be false)
}

// newHarnessMocks builds a harness with every collaborator mocked but no engine wired —
// the shared setup for both the mock-grader and real-gate constructors below.
func newHarnessMocks(t *testing.T) *harness {
	t.Helper()
	ctrl := gomock.NewController(t)
	h := &harness{
		tx:              daomocks.NewMockTransactor(ctrl),
		agg:             mocks.NewMockcontextAggregator(ctrl),
		deriver:         mocks.NewMockartifactDeriver(ctrl),
		grader:          mocks.NewMockqualityGrader(ctrl),
		store:           mocks.NewMockartifactStore(ctrl),
		library:         daomocks.NewMockIPromptLibraryDAO(ctrl),
		execution:       daomocks.NewMockITaskExecutionDAO(ctrl),
		harnessArtifact: daomocks.NewMockIHarnessArtifactDAO(ctrl),
		emitter:         mocks.NewMockAnalyticsEmitter(ctrl),
	}
	// The terminal write runs inside Transactor.InTx; a pass-through mock runs the fn in-line so the
	// terminal-write expectations the tests set observe the write.
	h.tx.EXPECT().InTx(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }).AnyTimes()
	// The derive flow reads committed VCS artifacts once per Run; default to none so a
	// test that does not exercise dedup needs no setup, and one that does sets h.committedArtifacts.
	h.harnessArtifact.EXPECT().ListVCSByRepo(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string) ([]*model.HarnessArtifact, error) {
			return h.committedArtifacts, nil
		}).AnyTimes()
	return h
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarnessMocks(t)
	h.engine = recommendations.NewEngine(h.tx, h.agg, h.deriver, h.grader, h.library, h.execution, h.harnessArtifact, h.store, h.emitter,
		func() int { return testWindow }, func() int { return h.themeFloor }, func(string) bool { return !h.flagOff }, func(string) bool { return !h.newTypesOff })
	return h
}

// newRealGateHarness mirrors newHarness but wires the REAL *QualityGate as the engine's
// grader instead of the mock, so an end-to-end test exercises the true validators. Buckets
// are uncapped (maxHooks/maxAgents → 0, which applyTypeCap treats as no-cap) and the session
// floors are 0 so a representative seed clears them; the DoD test seeds the run's shape.
// Callers must still program the gate's suppression reads (ListDismissedSlugs /
// ListRecentHiddenFailedSlugs) — the mock grader never touched them.
func newRealGateHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarnessMocks(t)
	zero := func() int { return 0 }
	realGate := recommendations.NewQualityGate(h.library, func() int { return 1 }, zero, zero, zero, zero)
	h.engine = recommendations.NewEngine(h.tx, h.agg, h.deriver, realGate, h.library, h.execution, h.harnessArtifact, h.store, h.emitter,
		func() int { return testWindow }, func() int { return h.themeFloor }, func(string) bool { return !h.flagOff }, func(string) bool { return !h.newTypesOff })
	return h
}

// aggResult is a representative aggregated context the build stage returns.
func aggResult() *promptlibraryv1.AggregatedContext {
	return &promptlibraryv1.AggregatedContext{
		OrgId:             testOrgID,
		SessionSummary:    &promptlibraryv1.SessionSummary{TotalSessions: 100, QualityDistribution: map[string]int32{"low": 20}},
		HighVsLowGap:      []*promptlibraryv1.IssueGap{{IssueSlug: "missing-tests", LowPct: 50, HighPct: 10, GapPp: 40}},
		RepoTrees:         []*promptlibraryv1.RepoTree{{RepoFullName: testProject}},
		SecurityIncidents: &promptlibraryv1.SecuritySummary{ApiKeyLeak: 3},
	}
}

// deriveResult is a representative derive output: one visible rule.
func deriveResult() *recommendations.DerivationResult {
	return &recommendations.DerivationResult{
		Rules: []recommendations.DerivedArtifact{{
			Title: "Always add a test", Slug: "always-add-a-test", Body: "rule body that is long enough", Scope: "project",
			Priority: "high", Confidence: 0.9, EvidenceSummary: "tests missing", AddressesIssues: []string{"missing-tests"}, CitedPaths: []string{"main.go"},
		}},
	}
}

func gradedVisible() *recommendations.GradedResult {
	return &recommendations.GradedResult{Artifacts: []recommendations.GradedArtifact{{
		Artifact:     deriveResult().Rules[0],
		ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE,
		SurfaceState: promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE,
	}}}
}

func gradedHidden() *recommendations.GradedResult {
	return &recommendations.GradedResult{Artifacts: []recommendations.GradedArtifact{{
		Artifact:        deriveResult().Rules[0],
		ArtifactType:    promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE,
		SurfaceState:    promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL,
		FailedValidator: "paths_exist",
	}}}
}

// program wires the full happy chain, then makes `failAt` (if not stageNone)
// return an error. securityOnly threads into the relevant stages. graded selects
// the grade result. The terminal status (COMPLETED via the success path, FAILED via
// fail, SKIPPED via skip) is recorded by the MarkTerminalIfRunning mock; the Update
// mock carries the input/output the content store-ref writes plus a STATUS-mask regression tripwire
// (see its comment) that records a blind status write so the no-clobber guards trip.
func (h *harness) program(failAt stage, securityOnly bool, graded *recommendations.GradedResult) {
	errAt := func(s stage) error {
		if failAt == s {
			return errors.New("boom")
		}
		return nil
	}

	h.execution.EXPECT().GetByID(gomock.Any(), testOrgID, testExecID).
		DoAndReturn(func(_ context.Context, _, _ string) (*model.TaskExecution, error) {
			if err := errAt(stageGetByID); err != nil {
				return nil, err
			}
			row := &model.TaskExecution{
				ID: testExecID, OrganizationID: testOrgID, DedupKey: testRepoUUID,
				Status: taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_RUNNING,
				// CreateIfNoneRunning seeds a non-nil empty ContentRef to satisfy the NOT
				// NULL column, so a fresh row's marker is the empty KEY, not a nil
				// payload — mirror that here so the emit guard is tested faithfully.
				InputPayload: &taskexecutionsv1.ContentRef{},
			}
			if h.resumedWithInput {
				// A prior attempt already recorded its input the content store key (and thus already
				// emitted derivation_triggered) — the emit guard keys on the key.
				row.InputPayload = &taskexecutionsv1.ContentRef{Key: "prior-input-key"}
			}
			return row, nil
		}).AnyTimes()

	// Atomic terminal compare-and-set — the single path for every terminal status
	// (COMPLETED from the success path, FAILED from fail, SKIPPED from skip). By default
	// it applies (records status + error_message); when supersede is set it reports the
	// row was already terminal (reaper won), so PersistArtifacts is never reached and no
	// status is recorded.
	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), testOrgID, testExecID, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, status taskexecutionsv1.TaskExecutionStatus, substatus taskexecutionsv1.TaskExecutionSubStatus, _ time.Time, errorMessage string) (bool, error) {
			if h.supersede {
				return false, nil
			}
			h.terminalStatus = status
			h.terminalSubStatus = substatus
			if errorMessage != "" {
				msg := errorMessage
				h.terminalErrMsg = &msg
			}
			return true, nil
		}).AnyTimes()

	h.emitter.EXPECT().EmitDerivationTriggered(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, orgID, scopeValue, execID string, trigger taskexecutionsv1.TaskTriggerType) {
			// Capture whether the input was already written when we emit. The once-per-run
			// guard (alreadyEmittedDerivationTriggered) keys on the input the content store key, which is
			// correct ONLY while the emit precedes the input-key write — assert that ordering
			// so a future reorder fails here instead of silently dropping every run's event.
			h.emitSawInputWritten = h.inputWritten
			h.emits = append(h.emits, emitCall{orgID, scopeValue, execID, trigger})
		}).AnyTimes()

	// In the current code only the input/output the content store-ref writes go through Update; the
	// terminal status goes exclusively through MarkTerminalIfRunning (compare-and-set),
	// so the STATUS branch below never fires today. It is a deliberate regression
	// tripwire, NOT dead code: if fail/skip ever revert to a blind by-id status Update
	// (no RUNNING guard), this records the written status so the no-clobber assertions
	// (TestEngine_Run_FailDoesNotClobberReapedRow and
	// TestEngine_Run_SkipsPersistWhenSuperseded) trip — without it the mock is blind to
	// an unguarded write and those guards pass vacuously.
	h.execution.EXPECT().Update(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, row *model.TaskExecution, masks []taskexecutionsv1.TaskExecutionFieldMask) error {
			for _, m := range masks {
				if m == taskexecutionsv1.TaskExecutionFieldMask_TASK_EXECUTION_FIELD_MASK_STATUS {
					h.terminalStatus = row.Status
					h.terminalErrMsg = row.ErrorMessage
				}
				if m == taskexecutionsv1.TaskExecutionFieldMask_TASK_EXECUTION_FIELD_MASK_INPUT_PAYLOAD {
					return errAt(stagePutInput) // model the input-ref Update failing alongside its stage
				}
			}
			return nil
		}).AnyTimes()

	h.agg.EXPECT().BuildContext(gomock.Any(), testOrgID, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, repoID string, from, to time.Time, secOnly, _ bool) (*promptlibraryv1.AggregatedContext, bool, int, error) {
			h.buildCtxRepoID = repoID
			h.buildCtxFrom, h.buildCtxTo, h.buildCtxSecOnly = from, to, secOnly
			if err := errAt(stageBuildContext); err != nil {
				return nil, false, 0, err
			}
			if h.aggOverride != nil {
				return h.aggOverride, h.enriched, h.reviewCommentsClassified, nil
			}
			return aggResult(), h.enriched, h.reviewCommentsClassified, nil
		}).AnyTimes()

	h.agg.EXPECT().ComputePRCorrelation(gomock.Any(), testOrgID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, _, _ time.Time) (*promptlibraryv1.PRCorrelation, error) {
			if err := errAt(stagePRCorrelation); err != nil {
				return nil, err
			}
			return &promptlibraryv1.PRCorrelation{DeltaPct: 12.5}, nil
		}).AnyTimes()

	h.store.EXPECT().PutRunInput(gomock.Any(), testOrgID, testExecID, gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, _ *promptlibraryv1.AggregatedContext) (string, error) {
			h.inputWritten = true // marks the start of the input-write step (the emit must precede this)
			return "inkey", nil
		}).AnyTimes()
	h.store.EXPECT().PutRunOutput(gomock.Any(), testOrgID, testExecID, gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, _ []byte) (string, error) {
			return "outkey", errAt(stagePutOutput)
		}).AnyTimes()
	h.store.EXPECT().PutArtifactContent(gomock.Any(), testOrgID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, content *promptlibraryv1.ArtifactContent) (string, error) {
			h.capturedContents = append(h.capturedContents, content)
			return "ckey", nil
		}).AnyTimes()
	h.store.EXPECT().PutArtifactEvidence(gomock.Any(), testOrgID, gomock.Any(), gomock.Any()).Return("ekey", nil).AnyTimes()

	h.deriver.EXPECT().DeriveArtifacts(gomock.Any(), gomock.Any(), testOrgID, testExecID).
		DoAndReturn(func(_ context.Context, agg *promptlibraryv1.AggregatedContext, _, _ string) (*recommendations.DerivationResult, []byte, error) {
			h.deriveSawAggCtx = agg
			if err := errAt(stageDerive); err != nil {
				return nil, nil, err
			}
			if h.deriveOverride != nil {
				return h.deriveOverride, []byte(`{"rules":[],"templates":[],"skills":[],"hooks":[],"agents":[]}`), nil
			}
			return deriveResult(), []byte(`{"rules":[],"templates":[],"skills":[]}`), nil
		}).AnyTimes()

	h.grader.EXPECT().Grade(gomock.Any(), testOrgID, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, scopeValue string, _ *recommendations.DerivationResult, repoTree *promptlibraryv1.RepoTree, security *promptlibraryv1.SecuritySummary, _, _ int, secOnly, _ bool, _ commonv1.IdeType, committed map[recommendations.CommittedKey]struct{}) (*recommendations.GradedResult, error) {
			h.gradeScopeVal, h.gradeRepoTree, h.gradeSecOnly, h.gradeSecurity = scopeValue, repoTree, secOnly, security
			h.gradeSawCommitted = committed
			if err := errAt(stageGrade); err != nil {
				return nil, err
			}
			return graded, nil
		}).AnyTimes()

	h.library.EXPECT().PersistArtifacts(gomock.Any(), testOrgID, testExecID, gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem) error {
			h.persisted = items
			return errAt(stagePersist)
		}).AnyTimes()
}

func (h *harness) run(securityOnly bool) error {
	return h.engine.Run(context.Background(), testOrgID,
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, testExecID, securityOnly)
}

func TestEngine_Run_HappyPath(t *testing.T) {
	h := newHarness(t)
	h.program(stageNone, false, gradedVisible())

	if err := h.run(false); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if h.terminalStatus != taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED {
		t.Errorf("terminal status = %v, want COMPLETED", h.terminalStatus)
	}
	if h.terminalErrMsg != nil {
		t.Errorf("error_message must be NULL on success, got %v", *h.terminalErrMsg)
	}

	// Exactly one derivation_triggered, at start, with the run's identity.
	// This is the emit case of the once-per-run guard (fresh row, empty input key); the
	// skip case (a resumed run with the key already set) is TestEngine_Run_RetryDoesNotReEmit.
	if len(h.emits) != 1 {
		t.Fatalf("emit count = %d, want exactly 1", len(h.emits))
	}
	// Regression guard for the emit-marker coupling (DESIGN): the once-per-run guard keys
	// on the input the content store key, so the emit MUST precede the input-key write. If a refactor
	// reorders the input-write before the emit, every first delivery would look
	// already-emitted and the run's only event would be dropped — this trips first.
	if h.emitSawInputWritten {
		t.Error("derivation_triggered emitted AFTER the input was written; the emit-once guard keys on the input key and must run before it")
	}
	got := h.emits[0]
	if got.orgID != testOrgID || got.scopeValue != testRepoUUID || got.execID != testExecID ||
		got.trigger != taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED {
		t.Errorf("emit args = %+v, want org/scopeValue/execID/SCHEDULED", got)
	}

	// The single-tenant engine computes the window from DataWindowDays trailing from now (no plan
	// enricher / OrgCreatedAt clamp): to≈now, from≈now−DataWindowDays. Assert the span and recency
	// rather than a fixed calendar date.
	if since := time.Since(h.buildCtxTo); since < 0 || since > time.Minute {
		t.Errorf("BuildContext to = %v, want ≈now (within a minute), delta=%v", h.buildCtxTo, since)
	}
	wantFrom := h.buildCtxTo.Truncate(24*time.Hour).AddDate(0, 0, -testWindow)
	if !h.buildCtxFrom.Equal(wantFrom) {
		t.Errorf("BuildContext from = %v, want to(day-truncated)−%dd = %v", h.buildCtxFrom, testWindow, wantFrom)
	}

	// BuildContext is grounded by the repo resolved from the run's id (Option B/C),
	// so a duplicate full_name can't divert grounding from the persisted scope_value.
	if h.buildCtxRepoID != testRepoUUID {
		t.Errorf("BuildContext repo id = %q, want %q (resolved from dedup_key)", h.buildCtxRepoID, testRepoUUID)
	}

	// scope_value (= dedup_key) + repoTree threaded into Grade.
	if h.gradeScopeVal != testRepoUUID {
		t.Errorf("Grade scopeValue = %q, want %q (the repo dedup_key)", h.gradeScopeVal, testRepoUUID)
	}
	if h.gradeRepoTree == nil || h.gradeRepoTree.GetRepoFullName() != testProject {
		t.Errorf("Grade repoTree = %v, want the project tree", h.gradeRepoTree)
	}
	// The security summary (the evidence a securityOnly run grades against) is the
	// aggregated context's, threaded through unchanged.
	if h.gradeSecurity.GetApiKeyLeak() != 3 {
		t.Errorf("Grade security.ApiKeyLeak = %d, want 3 (the aggregated context's summary)", h.gradeSecurity.GetApiKeyLeak())
	}

	// toModels mapping on the persisted rows.
	if len(h.persisted) != 1 {
		t.Fatalf("persisted %d items, want 1", len(h.persisted))
	}
	item := h.persisted[0]
	if item.Slug != "always-add-a-test" {
		t.Errorf("item.Slug = %q, want %q (slug round-trips through toModels)", item.Slug, "always-add-a-test")
	}
	if item.ScopeValue != testRepoUUID {
		t.Errorf("item.ScopeValue = %q, want %q", item.ScopeValue, testRepoUUID)
	}
	if item.TaskExecutionID != testExecID {
		t.Errorf("item.TaskExecutionID = %q, want %q", item.TaskExecutionID, testExecID)
	}
	if item.Scope != promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT {
		t.Errorf("item.Scope = %v, want PROJECT", item.Scope)
	}
	if item.ArtifactType != promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE {
		t.Errorf("item.ArtifactType = %v, want RULE", item.ArtifactType)
	}
	if item.SurfaceState != promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE {
		t.Errorf("item.SurfaceState = %v, want VISIBLE", item.SurfaceState)
	}
	if item.Priority != 20 { // "high" → 20 (schema scale: critical=30/high=20/medium=10)
		t.Errorf("item.Priority = %d, want 20 (high)", item.Priority)
	}
	if item.ArtifactContent.GetKey() != "ckey" || item.Evidence.GetRef().GetKey() != "ekey" {
		t.Errorf("item the content store refs = content %q / evidence %q, want ckey/ekey", item.ArtifactContent.GetKey(), item.Evidence.GetRef().GetKey())
	}
	if item.ID == "" {
		t.Error("item.ID must be pre-minted (the content store keys depend on it)")
	}
	// PR correlation woven into the inline projection.
	if item.Evidence.GetPrDeltaPct() != 12.5 {
		t.Errorf("evidence pr_delta_pct = %v, want 12.5", item.Evidence.GetPrDeltaPct())
	}
}

// TestEngine_Run_RetryDoesNotReEmit asserts the once-per-run derivation_triggered
// guarantee survives a retry: a run resumed with its InputPayload
// already set (a prior attempt got past the emit) proceeds through the full pipeline
// and completes, but must NOT re-emit the analytics event. The guard keys on the
// persisted input content key, not the runner attempt number — so a retry whose first
// delivery failed BEFORE the emit (input key still empty) would still emit; the
// happy-path test covers that emit case (a fresh row's seeded empty ContentRef).
func TestEngine_Run_RetryDoesNotReEmit(t *testing.T) {
	h := newHarness(t)
	h.resumedWithInput = true // a retry resuming a run that already recorded its input (and already emitted)
	h.program(stageNone, false, gradedVisible())

	if err := h.run(false); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// The retry still drives the run to terminal COMPLETED — suppression is only on
	// the emit, never on the derivation work itself.
	if h.terminalStatus != taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED {
		t.Errorf("terminal status = %v, want COMPLETED", h.terminalStatus)
	}
	if len(h.emits) != 0 {
		t.Fatalf("retry emit count = %d, want 0 (the first attempt already emitted)", len(h.emits))
	}
}

func TestEngine_Run_NoneVisible(t *testing.T) {
	h := newHarness(t)
	h.program(stageNone, false, gradedHidden())

	if err := h.run(false); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.terminalStatus != taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE {
		t.Errorf("terminal status = %v, want COMPLETED_NO_VISIBLE", h.terminalStatus)
	}
	if h.terminalErrMsg != nil {
		t.Error("COMPLETED_NO_VISIBLE must leave error_message NULL")
	}
	// All artifacts (incl. HIDDEN) are still persisted.
	if len(h.persisted) != 1 {
		t.Errorf("persisted %d, want 1 (hidden rows kept)", len(h.persisted))
	}
}

func TestEngine_Run_SecurityScopeHidesNotPersisted(t *testing.T) {
	// A security_scope hide is a scope filter on a securityOnly run, not a quality
	// failure, so toModels must NOT persist it — persisting it as HIDDEN_FAILED_EVAL
	// would poison the recent-hidden dedup and suppress the artifact's legitimate
	// re-derivation in a later full run. A genuine quality-failed hide on the same
	// run still persists.
	h := newHarness(t)
	graded := &recommendations.GradedResult{Artifacts: []recommendations.GradedArtifact{
		{
			Artifact:     deriveResult().Rules[0],
			ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE,
			SurfaceState: promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE,
		},
		{
			Artifact:        recommendations.DerivedArtifact{Title: "General boilerplate skill", Body: "a body long enough to clear the min", Scope: "project"},
			ArtifactType:    promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL,
			SurfaceState:    promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL,
			FailedValidator: "security_scope", // == validatorSecurityScope; literal mirrors gradedHidden's "paths_exist"
		},
		{
			Artifact:        recommendations.DerivedArtifact{Title: "Hallucinated-path rule", Body: "a body long enough to clear the min", Scope: "project"},
			ArtifactType:    promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE,
			SurfaceState:    promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL,
			FailedValidator: "paths_exist", // a genuine quality failure — still persisted
		},
	}}
	h.program(stageNone, true, graded)

	if err := h.run(true); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	// 2 persisted: the visible rule + the paths_exist quality hide. The security_scope
	// scope-filter hide is dropped.
	if len(h.persisted) != 2 {
		t.Fatalf("persisted %d items, want 2 (security_scope hide must not persist)", len(h.persisted))
	}
	for _, item := range h.persisted {
		if item.Title == "General boilerplate skill" {
			t.Errorf("a security_scope-hidden artifact was persisted; it must be dropped to avoid dedup poisoning")
		}
	}
}

func TestEngine_Run_TypeScopeDropsNotPersisted(t *testing.T) {
	// A transient drop (a feature-disabled org's hooks/agents; a low-session hook/agent/template
	// session-signal hide; the top-N cap; a tool-less/cross-tool hide) is a scope/output filter,
	// not a quality failure — so toModels must NOT persist it. Persisting it as HIDDEN_FAILED_EVAL
	// would poison the recent-hidden dedup and suppress those slugs forever once the flag/session/
	// cap/tool state changes. A genuine quality-failed hide on the same run still persists. Mirrors
	// the security_scope drop-not-persist contract.
	h := newHarness(t)
	graded := &recommendations.GradedResult{Artifacts: []recommendations.GradedArtifact{
		{
			Artifact:     deriveResult().Rules[0],
			ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK,
			SurfaceState: promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE,
		},
		{
			Artifact:        recommendations.DerivedArtifact{Title: "Low-session hook", Body: "a body long enough to clear the min", Scope: "project"},
			ArtifactType:    promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK,
			SurfaceState:    promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL,
			FailedValidator: "hook_session_signal", // == validatorHookSessionSignal (drop-not-persist)
		},
		{
			Artifact:        recommendations.DerivedArtifact{Title: "Feature-disabled agent", Body: "a body long enough to clear the min", Scope: "project"},
			ArtifactType:    promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT,
			SurfaceState:    promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL,
			FailedValidator: "new_types_disabled", // == validatorNewTypesDisabled
		},
		{
			Artifact:        recommendations.DerivedArtifact{Title: "Over-cap hook", Body: "a body long enough to clear the min", Scope: "project"},
			ArtifactType:    promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK,
			SurfaceState:    promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL,
			FailedValidator: "type_cap_exceeded", // == validatorTypeCapExceeded
		},
		{
			// A tool-less hide is context-transient (the resolved tool changes between
			// runs), so it must also be dropped — else a later run that resolves a tool
			// re-derives the slug into not_dismissed and it never surfaces.
			Artifact:        recommendations.DerivedArtifact{Title: "Tool-less hook", Body: "a body long enough to clear the min", Scope: "project"},
			ArtifactType:    promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK,
			SurfaceState:    promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL,
			FailedValidator: "hook_tool", // == validatorHookTool
		},
		{
			Artifact:        recommendations.DerivedArtifact{Title: "Tool-less agent", Body: "a body long enough to clear the min", Scope: "project"},
			ArtifactType:    promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT,
			SurfaceState:    promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL,
			FailedValidator: "agent_tool", // == validatorAgentTool
		},
		{
			// A cross-tool event hide (a Claude event on a Cursor-resolved hook) is equally
			// tool-dependent — a later run resolving the owning tool re-surfaces the slug — so
			// it must be dropped too, never persisted as HIDDEN_FAILED_EVAL.
			Artifact:        recommendations.DerivedArtifact{Title: "Cross-tool-event hook", Body: "a body long enough to clear the min", Scope: "project"},
			ArtifactType:    promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK,
			SurfaceState:    promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL,
			FailedValidator: "hook_event_tool_scoped", // == validatorHookEventToolScoped
		},
		{
			Artifact:        recommendations.DerivedArtifact{Title: "Hallucinated-path rule", Body: "a body long enough to clear the min", Scope: "project"},
			ArtifactType:    promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE,
			SurfaceState:    promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL,
			FailedValidator: "paths_exist", // a genuine quality failure — still persisted
		},
	}}
	h.program(stageNone, false, graded)

	if err := h.run(false); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	// 2 persisted: the visible hook + the paths_exist quality hide. The six transient
	// scope/cap/tool/event drops are dropped.
	if len(h.persisted) != 2 {
		t.Fatalf("persisted %d items, want 2 (transient scope/cap/tool drops must not persist)", len(h.persisted))
	}
	for _, item := range h.persisted {
		switch item.Title {
		case "Low-session hook", "Feature-disabled agent", "Over-cap hook", "Tool-less hook", "Tool-less agent", "Cross-tool-event hook":
			t.Errorf("a transient-dropped artifact (%q) was persisted; it must be dropped to avoid dedup poisoning", item.Title)
		}
	}
}

func TestEngine_Run_EmptyDeriveIsNoVisibleNotFailed(t *testing.T) {
	// A zero-artifact derive grades to an empty result (NoneVisible). PersistArtifacts
	// rejects an empty batch, so the engine must skip the persist and finish
	// COMPLETED_NO_VISIBLE — not FAILED.
	h := newHarness(t)
	h.program(stageNone, false, &recommendations.GradedResult{})

	if err := h.run(false); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if h.terminalStatus != taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE {
		t.Errorf("terminal status = %v, want COMPLETED_NO_VISIBLE", h.terminalStatus)
	}
	if h.persisted != nil {
		t.Errorf("PersistArtifacts must be skipped for an empty batch, got %d items", len(h.persisted))
	}
}

func TestEngine_Run_SkipsNonRunningRow(t *testing.T) {
	// redelivery / the stale-run reaper can resolve a task to an
	// already-terminal row; Run must ack it as a no-op — no emit, no derive, no
	// terminal write — so it can't re-run the pipeline or race a fresh run.
	terminal := []taskexecutionsv1.TaskExecutionStatus{
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_FAILED,
	}
	for _, status := range terminal {
		t.Run(status.String(), func(t *testing.T) {
			ctrl := gomock.NewController(t)
			execution := daomocks.NewMockITaskExecutionDAO(ctrl)
			emitter := mocks.NewMockAnalyticsEmitter(ctrl)
			// Only GetByID is expected; any emit / derive / Update is a failure.
			execution.EXPECT().GetByID(gomock.Any(), testOrgID, testExecID).Return(&model.TaskExecution{
				ID: testExecID, OrganizationID: testOrgID, DedupKey: testRepoUUID, Status: status,
			}, nil)

			engine := recommendations.NewEngine(
				daomocks.NewMockTransactor(ctrl), mocks.NewMockcontextAggregator(ctrl),
				mocks.NewMockartifactDeriver(ctrl), mocks.NewMockqualityGrader(ctrl),
				daomocks.NewMockIPromptLibraryDAO(ctrl), execution,
				daomocks.NewMockIHarnessArtifactDAO(ctrl),
				mocks.NewMockartifactStore(ctrl), emitter, func() int { return testWindow }, func() int { return 0 }, func(string) bool { return true }, func(string) bool { return true })

			if err := engine.Run(context.Background(), testOrgID,
				taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, testExecID, false); err != nil {
				t.Fatalf("Run on a %s row should be a no-op, got error: %v", status, err)
			}
			// gomock's controller (via t.Cleanup) fails if emitter/enricher/etc. were called.
		})
	}
}

// TestEngine_Run_SkipsWhenGatedAtDeriveTime asserts the derive-time rollout re-check:
// a derive task can sit behind the low-priority queue after it was enqueued, so if the org
// opts out (the derive flag off) in the meantime, Run marks the row SKIPPED (not FAILED) and acks
// without deriving — and emits no derivation_triggered event.
func TestEngine_Run_SkipsWhenGatedAtDeriveTime(t *testing.T) {
	// Single-tenant: only the rollout flag gates a derive-time run.
	tests := []struct {
		name    string
		flagOff bool
	}{
		{name: "flag disabled at derive time", flagOff: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.flagOff = tt.flagOff
			h.program(stageNone, false, gradedVisible())

			if err := h.engine.Run(context.Background(), testOrgID,
				taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, testExecID, false); err != nil {
				t.Fatalf("a gated run should be a no-op, got error: %v", err)
			}
			if h.terminalStatus != taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_SKIPPED {
				t.Errorf("a gated run must be marked SKIPPED (not FAILED), got %s", h.terminalStatus)
			}
			if len(h.persisted) != 0 {
				t.Errorf("no artifacts should persist for a gated run, got %d", len(h.persisted))
			}
			if len(h.emits) != 0 {
				t.Errorf("no derivation_triggered event for a gated run, got %d", len(h.emits))
			}
		})
	}
}

// TestEngine_Run_EnrichedThemeFloor asserts the enriched-path admission: a repo that
// BuildContext classified top-N (enriched=true) is admitted only if its theme-assignment
// count clears MinReviewThemesPerRepo. Below it the run terminates COMPLETED_NO_VISIBLE +
// REPO_FEW_THEMES with no derive/persist; at/above it derives normally. A standard
// (non-enriched) run is never theme-gated, however few its themes.
func TestEngine_Run_EnrichedThemeFloor(t *testing.T) {
	withThemes := func(count int32) *promptlibraryv1.AggregatedContext {
		c := aggResult()
		c.ReviewThemes = []*promptlibraryv1.ReviewThemeSummary{{Theme: "testing", Count: count}}
		return c
	}
	tests := []struct {
		name        string
		enriched    bool
		themeCount  int32
		wantStatus  taskexecutionsv1.TaskExecutionStatus
		wantSub     taskexecutionsv1.TaskExecutionSubStatus
		wantPersist bool
	}{
		{
			name:     "enriched below floor terminates REPO_FEW_THEMES, no artifacts",
			enriched: true, themeCount: 40,
			wantStatus: taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE,
			wantSub:    taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_REPO_FEW_THEMES,
		},
		{
			name:     "enriched at/above floor derives normally",
			enriched: true, themeCount: 120,
			wantStatus:  taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED,
			wantSub:     taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_UNSPECIFIED,
			wantPersist: true,
		},
		{
			name:     "standard (non-enriched) low-theme run is not theme-gated",
			enriched: false, themeCount: 1,
			wantStatus:  taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED,
			wantSub:     taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_UNSPECIFIED,
			wantPersist: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.themeFloor = 100
			h.enriched = tt.enriched
			h.aggOverride = withThemes(tt.themeCount)
			h.program(stageNone, false, gradedVisible())

			if err := h.run(false); err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			if h.terminalStatus != tt.wantStatus {
				t.Errorf("terminal status = %v, want %v", h.terminalStatus, tt.wantStatus)
			}
			if h.terminalSubStatus != tt.wantSub {
				t.Errorf("terminal substatus = %v, want %v", h.terminalSubStatus, tt.wantSub)
			}
			if tt.wantPersist && len(h.persisted) == 0 {
				t.Errorf("expected artifacts to persist, got none")
			}
			if !tt.wantPersist && len(h.persisted) != 0 {
				t.Errorf("no artifacts should persist below the theme floor, got %d", len(h.persisted))
			}
		})
	}
}

// TestEngine_Run_LeavesRowRunningOnCanceledCtx asserts fail/skip write NO terminal
// status when the handler ctx is canceled. A derive task's handler timeout (or a pod
// shutdown) cancels ctx and the runner immediately retries/redelivers THIS task (the runner:
// deadline → retry), which resumes the same still-RUNNING row. Writing FAILED/SKIPPED
// here would race that retry and, if it won, supersede the retry's work. So the row is
// left RUNNING (the retry owns the outcome; the StaleAfter reaper backstops) and Run
// still acks as a no-op. Regression guard: against the prior detached-write code this
// trips — terminalStatus would be FAILED/SKIPPED instead of UNSPECIFIED.
func TestEngine_Run_LeavesRowRunningOnCanceledCtx(t *testing.T) {
	tests := []struct {
		name    string
		flagOff bool // drives the skip() path (rollout gate), else a derive failure → fail()
		failAt  stage
	}{
		{name: "fail path writes nothing on canceled ctx", failAt: stageDerive},
		{name: "skip path writes nothing on canceled ctx", flagOff: true, failAt: stageNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.flagOff = tt.flagOff
			h.program(tt.failAt, false, gradedVisible())

			ctx, cancel := context.WithCancel(context.Background())
			cancel() // canceled before Run: simulates a fired handler timeout / pod shutdown

			if err := h.engine.Run(ctx, testOrgID,
				taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, testExecID, false); err != nil {
				t.Fatalf("Run must ack as a no-op on a canceled ctx, got error: %v", err)
			}
			if h.terminalStatus != taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_UNSPECIFIED {
				t.Errorf("wrote terminal status %s on a canceled ctx; must leave the row RUNNING for the runner-retry to own", h.terminalStatus)
			}
		})
	}
}

// TestEngine_Run_SkipsPersistWhenSuperseded asserts the atomic pre-persist
// compare-and-set: if the stale-run reaper failed this row mid-flight (freeing
// the slot for a fresh run), MarkTerminalIfRunning matches 0 rows, so Run aborts
// BEFORE persisting and acks as a no-op — never writing artifacts that would race
// the fresh run's is_latest reconcile, nor overwriting the now-foreign row.
func TestEngine_Run_SkipsPersistWhenSuperseded(t *testing.T) {
	h := newHarness(t)
	h.program(stageNone, false, gradedVisible())
	// The pipeline starts on a RUNNING row; the reaper fails it before the terminal
	// claim, so MarkTerminalIfRunning reports the row was already terminal.
	h.supersede = true

	if err := h.run(false); err != nil {
		t.Fatalf("a superseded run must ack as a no-op, got error: %v", err)
	}
	if h.persisted != nil {
		t.Error("must not persist artifacts when the terminal claim found the row reaped")
	}
	if h.terminalStatus != taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_UNSPECIFIED {
		t.Errorf("must not overwrite the reaped row's status, got %v", h.terminalStatus)
	}
}

// TestEngine_Run_FailDoesNotClobberReapedRow guards the fail-path compare-and-set on a
// live ctx: if the StaleAfter reaper failed this row mid-flight (freeing the slot for a
// fresh run) and then an in-band error reaches fail(), the status=RUNNING CAS must no-op
// rather than overwrite the reaper's outcome. Pre-fix fail() used a blind by-id Update
// with no status guard and would clobber; the Update STATUS-mask tripwire in the harness
// makes that regression trip here instead of passing vacuously.
func TestEngine_Run_FailDoesNotClobberReapedRow(t *testing.T) {
	h := newHarness(t)
	h.program(stageDerive, false, gradedVisible()) // live ctx, derive fails → fail() path
	// The reaper already failed this row mid-flight, so it is no longer RUNNING when
	// fail()'s compare-and-set runs.
	h.supersede = true

	if err := h.run(false); err != nil {
		t.Fatalf("fail() on a reaped row must ack as a no-op, got error: %v", err)
	}
	if h.terminalStatus != taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_UNSPECIFIED {
		t.Errorf("fail() clobbered the reaped row's terminal status (got %s); the status=RUNNING CAS must no-op", h.terminalStatus)
	}
}

func TestEngine_Run_SecurityOnlyThreadedVerbatim(t *testing.T) {
	h := newHarness(t)
	h.program(stageNone, true, gradedVisible())

	if err := h.run(true); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !h.buildCtxSecOnly {
		t.Error("securityOnly not threaded to BuildContext")
	}
	if !h.gradeSecOnly {
		t.Error("securityOnly not threaded to Grade")
	}
}

func TestEngine_Run_Failures(t *testing.T) {
	tests := []struct {
		name        string
		failAt      stage
		wantStatus  taskexecutionsv1.TaskExecutionStatus
		wantRunErr  bool // GetByID failure surfaces the error instead of marking FAILED
		wantMsgPart string
	}{
		{name: "load failure surfaces error (the runner retries)", failAt: stageGetByID, wantRunErr: true},
		{name: "build-context failure → FAILED", failAt: stageBuildContext, wantStatus: failed(), wantMsgPart: "building context"},
		{name: "pr-correlation failure → FAILED", failAt: stagePRCorrelation, wantStatus: failed(), wantMsgPart: "computing pr correlation"},
		{name: "input-ref persist failure → FAILED", failAt: stagePutInput, wantStatus: failed(), wantMsgPart: "persisting input ref"},
		{name: "derive failure → FAILED", failAt: stageDerive, wantStatus: failed(), wantMsgPart: "deriving artifacts"},
		{name: "output persist failure → FAILED", failAt: stagePutOutput, wantStatus: failed(), wantMsgPart: "persisting run output"},
		{name: "grade dedup-read failure → FAILED", failAt: stageGrade, wantStatus: failed(), wantMsgPart: "grading run"},
		{name: "persist failure → FAILED", failAt: stagePersist, wantStatus: failed(), wantMsgPart: "persisting run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.program(tt.failAt, false, gradedVisible())

			err := h.run(false)

			if tt.wantRunErr {
				if err == nil {
					t.Fatal("expected Run to return the error")
				}
				if h.terminalStatus != taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_UNSPECIFIED {
					t.Error("a load failure must not write a terminal status (no row to update)")
				}
				return
			}
			if err != nil {
				t.Fatalf("Run returned error (should mark FAILED instead): %v", err)
			}
			if h.terminalStatus != tt.wantStatus {
				t.Errorf("terminal status = %v, want %v", h.terminalStatus, tt.wantStatus)
			}
			if h.terminalErrMsg == nil {
				t.Fatal("FAILED run must set error_message")
			}
			if tt.wantMsgPart != "" && !strings.Contains(*h.terminalErrMsg, tt.wantMsgPart) {
				t.Errorf("error_message %q does not contain %q", *h.terminalErrMsg, tt.wantMsgPart)
			}
		})
	}
}

// TestEngine_Run_OutcomeMetrics asserts the observability contract: each terminal
// outcome increments recommendations_derivation_runs_total{result} exactly once for the result
// it owned and nothing else, and recommendations_derivation_duration_seconds is observed ONLY on
// a successful run (completed / no_visible) — never on a failure, gate-skip, supersede, or
// canceled ctx. A superseded run counts in runs_superseded_total, not runs_total — so the
// failure-rate SLO denominator never double-counts a reaper-failed row. Counters are
// process-global, so each case measures a delta around its own run.
func TestEngine_Run_OutcomeMetrics(t *testing.T) {
	const (
		runsTotal     = "recommendations_derivation_runs_total"
		durationCount = "recommendations_derivation_duration_seconds"
		supersededCtr = "recommendations_derivation_runs_superseded_total"
	)
	allResults := []string{"completed", "no_visible", "failed", "skipped"}

	tests := []struct {
		name           string
		flagOff        bool
		supersede      bool
		canceledCtx    bool
		failAt         stage
		graded         *recommendations.GradedResult
		wantResult     string // runs_total label expected to increment; "" → none
		wantDuration   bool
		wantSuperseded bool
	}{
		{name: "completed: runs_total{completed} + duration", graded: gradedVisible(), wantResult: "completed", wantDuration: true},
		{name: "no_visible: runs_total{no_visible} + duration", graded: gradedHidden(), wantResult: "no_visible", wantDuration: true},
		{name: "failed: runs_total{failed}, no duration", failAt: stageDerive, graded: gradedVisible(), wantResult: "failed"},
		{name: "skipped: runs_total{skipped}, no duration", flagOff: true, graded: gradedVisible(), wantResult: "skipped"},
		{name: "superseded: runs_superseded only", supersede: true, graded: gradedVisible(), wantSuperseded: true},
		{name: "canceled ctx: nothing counted (runner-retry owns it)", canceledCtx: true, failAt: stageDerive, graded: gradedVisible()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.flagOff = tt.flagOff
			h.supersede = tt.supersede
			h.program(tt.failAt, false, tt.graded)

			runsBefore := make(map[string]float64, len(allResults))
			for _, r := range allResults {
				runsBefore[r] = counterValue(t, runsTotal, map[string]string{"result": r})
			}
			durBefore := histogramCount(t, durationCount)
			supBefore := counterValue(t, supersededCtr, nil)

			ctx := context.Background()
			if tt.canceledCtx {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			if err := h.engine.Run(ctx, testOrgID,
				taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, testExecID, false); err != nil {
				t.Fatalf("Run returned error: %v", err)
			}

			for _, r := range allResults {
				wantDelta := 0.0
				if r == tt.wantResult {
					wantDelta = 1.0
				}
				if got := counterValue(t, runsTotal, map[string]string{"result": r}) - runsBefore[r]; got != wantDelta {
					t.Errorf("runs_total{result=%s} delta = %v, want %v", r, got, wantDelta)
				}
			}
			wantDur := uint64(0)
			if tt.wantDuration {
				wantDur = 1
			}
			if got := histogramCount(t, durationCount) - durBefore; got != wantDur {
				t.Errorf("duration_seconds sample delta = %d, want %d", got, wantDur)
			}
			wantSup := 0.0
			if tt.wantSuperseded {
				wantSup = 1.0
			}
			if got := counterValue(t, supersededCtr, nil) - supBefore; got != wantSup {
				t.Errorf("runs_superseded_total delta = %v, want %v", got, wantSup)
			}
		})
	}
}

// counterValue reads the current value of a counter series (by metric name and an
// exact label match) from the default Prometheus registry. Returns 0 if the series
// doesn't exist yet. nil labels match a no-label counter's single series.
func counterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if !labelsEqual(m.GetLabel(), labels) {
				continue
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

// histogramCount reads a histogram's cumulative sample count (the _count series)
// from the default registry. Returns 0 if the histogram has no observations yet.
func histogramCount(t *testing.T, name string) uint64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			return m.GetHistogram().GetSampleCount()
		}
	}
	return 0
}

// labelsEqual reports whether a gathered metric's labels exactly equal want
// (same keys and values). A nil/empty want matches only a label-free series.
func labelsEqual(got []*dto.LabelPair, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, lp := range got {
		if want[lp.GetName()] != lp.GetValue() {
			return false
		}
	}
	return true
}

func failed() taskexecutionsv1.TaskExecutionStatus {
	return taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_FAILED
}

// TestEngine_Run_CommittedArtifactDedup is the wired-path integration proof: a full
// derive Run reads the repo's committed VCS artifacts and threads their canonical slugs to BOTH
// the quality gate (the committed_artifact drop set) AND the LLM avoid-hint on the aggCtx that
// DeriveArtifacts and the audit blob receive. The gate drop and the hint helper are covered
// in isolation by validators_test.go; this proves committedArtifactSlugs actually wires them
// together through Run — the path that goes live on the shipped derive flow (it is not
// flag-gated), so it must be exercised end to end.
func TestEngine_Run_CommittedArtifactDedup(t *testing.T) {
	h := newHarness(t)
	// A committed skill "committed-skill" → canonical "/committed-skill" (skills are "/"-prefixed);
	// a committed rule "wrap-errors" → "wrap-errors" (bare kebab). Both are the derive-flow slug form.
	h.committedArtifacts = []*model.HarnessArtifact{
		{Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL, Name: "committed-skill"},
		{Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE, Name: "wrap-errors"},
	}
	h.program(stageNone, false, gradedVisible())

	if err := h.run(false); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	contains := func(s []string, v string) bool {
		for _, x := range s {
			if x == v {
				return true
			}
		}
		return false
	}
	// Gate side: the canonical committed (type, slug) keys reached Grade as the dedup set —
	// keyed by type, so a committed skill and a committed rule are distinct entries.
	if h.gradeSawCommitted == nil {
		t.Fatal("committed slugs must reach Grade")
	}
	wantKeys := []recommendations.CommittedKey{
		{Type: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL, Slug: "/committed-skill"},
		{Type: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE, Slug: "wrap-errors"},
	}
	for _, want := range wantKeys {
		if _, ok := h.gradeSawCommitted[want]; !ok {
			t.Errorf("Grade committed set missing %v; got %v", want, h.gradeSawCommitted)
		}
	}
	// Hint side: the SAME slugs are on the aggCtx avoid-list DeriveArtifacts received (and thus
	// the audit blob), so the LLM avoids re-suggesting them up front.
	if h.deriveSawAggCtx == nil {
		t.Fatal("DeriveArtifacts must have run")
	}
	dismissed := h.deriveSawAggCtx.GetAdminFeedback().GetDismissedRuleSlugs()
	for _, want := range []string{"/committed-skill", "wrap-errors"} {
		if !contains(dismissed, want) {
			t.Errorf("aggCtx avoid-list missing %q; got %v", want, dismissed)
		}
	}
}
