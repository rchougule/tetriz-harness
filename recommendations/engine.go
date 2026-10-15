// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

//go:generate go tool mockgen -source=engine.go -destination=mocks/mock_engine.go -package=mocks

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/artifactslug"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/logger"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// The engine's collaborators are behind interfaces so Run's orchestration —
// ordering, scope discipline, error→FAILED, status transitions, evidence
// assembly — is unit-testable with mocks; the concrete *Aggregator, *LLMClient,
// and *QualityGate satisfy them.

// errRunSuperseded marks that the run's row was no longer RUNNING when the
// terminal compare-and-set ran just before persist (the stale-run reaper failed
// it and a fresh run took the single-in-flight slot). Run treats it as a no-op
// ack, never a failure.
var errRunSuperseded = errors.New("derivation run superseded before persist")

// derivationRunsSupersededTotal counts runs that found their row already terminal
// at the pre-persist compare-and-set — i.e. the stale-run reaper failed them
// mid-flight. A non-zero rate signals workers running slower than StaleAfter and
// is otherwise invisible (the reaper, not this run, wrote the FAILED row).
var derivationRunsSupersededTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "recommendations_derivation_runs_superseded_total",
	Help: "Count of derivation runs aborted at persist because the stale-run reaper already failed the row.",
})

// The result label values for derivationRunsTotal — the complete set. The seed
// loop and the increment sites reference these consts so a renamed value can't
// drift on the Go side. The test asserts the same strings independently (from
// package promptlibrary_test), as does any operator PromQL that queries this
// metric across a cross-language boundary that can't const-share — so they double
// as a contract check, and adding a value still means updating them by hand.
const (
	resultCompleted = "completed"
	resultNoVisible = "no_visible"
	resultFailed    = "failed"
	resultSkipped   = "skipped"
)

var allDerivationResults = []string{resultCompleted, resultNoVisible, resultFailed, resultSkipped}

// derivationRunsTotal counts terminal runs, attributed only to the run that won
// the compare-and-set — so it excludes superseded runs (derivationRunsSupersededTotal)
// and reaper-failed rows (recommendations_derivation_runs_reaped_total), never double-counting.
// The failure-rate SLO is failed / (completed+failed); skipped and no_visible are
// excluded from it (a gated or all-hidden run is not a failure).
var derivationRunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "recommendations_derivation_runs_total",
	Help: "Count of derivation runs by terminal result (completed|no_visible|failed|skipped), attributed to the run that owned the terminal transition.",
}, []string{"result"})

// derivationDurationSeconds records wall-clock of SUCCESSFUL runs only (Run entry to
// the COMPLETED/COMPLETED_NO_VISIBLE write), so it reflects true derive cost: failures
// can be fast early errors, skips do no derive work, and a timed-out run never reaches
// a terminal write (the runner owns its retry). Buckets span the real shape — sequential
// ~3-min classify batches up to the 30m DeriveTaskTimeout ceiling.
var derivationDurationSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "recommendations_derivation_duration_seconds",
	Help:    "Wall-clock of a successful derivation run (Run entry to the COMPLETED/COMPLETED_NO_VISIBLE terminal write).",
	Buckets: []float64{10, 30, 60, 120, 300, 600, 900, 1200, 1800},
})

const (
	reviewClassifyPathEnriched = "enriched"
	reviewClassifyPathStandard = "standard"
)

var allReviewClassifyPaths = []string{reviewClassifyPathEnriched, reviewClassifyPathStandard}

// derivationReviewClassifyRunsTotal counts each derive run's review-classify path —
// enriched (below the comment floor, classified top-N and subject to the theme-floor
// admission) vs standard (1:1). It is the enriched population, i.e. the denominator the
// REPO_FEW_THEMES terminations (a no_visible substatus) are a numerator of, so the gate's
// terminate-rate and whether MinReviewThemesPerRepo is set right are measurable in prod.
var derivationReviewClassifyRunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "recommendations_derivation_review_classify_runs_total",
	Help: "Count of prompt-library derive runs by review-classify path (enriched=below-comment-floor top-N | standard=1:1).",
}, []string{"path"})

// registerEngineMetrics registers the derivation-engine metrics on reg. Opt-in (no
// import-time side effect): the package's RegisterMetrics calls it once at startup.
func registerEngineMetrics(reg prometheus.Registerer) {
	reg.MustRegister(derivationRunsSupersededTotal, derivationRunsTotal, derivationDurationSeconds, derivationReviewClassifyRunsTotal)
	for _, path := range allReviewClassifyPaths {
		derivationReviewClassifyRunsTotal.WithLabelValues(path).Add(0)
	}
	// Seed every result series at 0 so it exists before the first increment. A
	// CounterVec series is created lazily, so without this the first scrape could
	// already see the whole first weekly batch — rate()/increase() over [24h] would
	// then read a flat series and the failure-rate alert would miss that first batch
	// after a deploy/restart. Add(0) materializes the child at 0 explicitly.
	for _, result := range allDerivationResults {
		derivationRunsTotal.WithLabelValues(result).Add(0)
	}
}

// contextAggregator assembles the per-project derive input and the org-grain PR correlation evidence.
type contextAggregator interface {
	BuildContext(ctx context.Context, orgID, repoID string, from, to time.Time, securityOnly, newTypesEnabled bool) (*promptlibraryv1.AggregatedContext, bool, int, error)
	ComputePRCorrelation(ctx context.Context, orgID string, from, to time.Time) (*promptlibraryv1.PRCorrelation, error)
}

// artifactDeriver runs the derive LLM call. It returns the parsed result AND the raw normalized
// response bytes the LLM service returned, so the engine can persist the latter verbatim as the run's
// audit blob (a re-marshal of the parsed struct would silently drop any field the Go model does not
// yet carry).
type artifactDeriver interface {
	DeriveArtifacts(ctx context.Context, aggCtx *promptlibraryv1.AggregatedContext, orgID, sourceID string) (*DerivationResult, []byte, error)
}

// qualityGrader stamps a surface_state on each derived artifact (deterministic). newTypesEnabled is
// the org's hooks-and-agents feature flag, re-read live at derive time (fail-closed); it gates whether
// hook/agent types survive. Admission already required the review-comment floor, so every graded run
// is review-floor-met.
type qualityGrader interface {
	Grade(ctx context.Context, orgID, scopeValue string, result *DerivationResult, repoTree *promptlibraryv1.RepoTree, security *promptlibraryv1.SecuritySummary, sessionLow, sessionHigh int, securityOnly, newTypesEnabled bool, resolvedTool commonv1.IdeType, committedSlugs map[CommittedKey]struct{}) (*GradedResult, error)
}

// harnessIntentClassifier classifies one committed skill/command/subagent's intent via the LLM service
// — the LLM leg of the DETECTED behavioral-attribution lens. *LLMClient satisfies it; the interface
// exists only so the engine's enrich phase is mockable in a unit test.
type harnessIntentClassifier interface {
	ClassifyHarnessIntent(ctx context.Context, orgID, sourceID string, input HarnessIntentInput) (HarnessIntentResult, error)
}

// harnessFixGenerator drafts a corrected body for one content-rewrite finding via the LLM service —
// the LLM leg of the DETECTED fix producer. *LLMClient satisfies it; the interface exists only so the
// engine's fix-produce phase is mockable in a unit test.
type harnessFixGenerator interface {
	GenerateHarnessFix(ctx context.Context, orgID, sourceID string, input HarnessFixInput) (HarnessFixResult, error)
}

// artifactStore persists a run's input/output and each produced artifact's content/evidence to the
// content store. It returns an opaque content reference (the storage layer owns its form); callers
// treat the string as opaque. The single-tenant implementation is content-column-backed (Phase 4).
type artifactStore interface {
	PutRunInput(ctx context.Context, orgID, taskExecutionID string, aggregatedContext *promptlibraryv1.AggregatedContext) (string, error)
	PutRunOutput(ctx context.Context, orgID, taskExecutionID string, rawOutput []byte) (string, error)
	PutArtifactContent(ctx context.Context, orgID, artifactID string, content *promptlibraryv1.ArtifactContent) (string, error)
	PutArtifactEvidence(ctx context.Context, orgID, artifactID string, evidence *promptlibraryv1.ArtifactEvidence) (string, error)
}

const (
	// evidenceSource labels the primary evidence source on every artifact this
	// engine derives (session quality + VCS PR/review signals).
	evidenceSource = "session+vcs"
	// evidenceProjectionSummaryMaxChars caps the inline card summary so the
	// list/card view stays small (the full summary lives in the content store). ~500 chars.
	evidenceProjectionSummaryMaxChars = 500
)

// Priority column values — the schema's documented scale: higher INT = higher priority, critical=30/high=20/medium=10/low=5, so a
// serving query ordering priority DESC surfaces the most critical first. low is 5 (not 0)
// so it still outranks the column's 0 default (an ungraded/unset row).
const (
	priorityCritical int32 = 30
	priorityHigh     int32 = 20
	priorityMedium   int32 = 10
	priorityLow      int32 = 5
)

// Engine runs the per-project derivation pipeline for one ARTIFACT_SCOPE_PROJECT:
// build context → derive (LLM) → grade → persist, resuming a
// run row by task_execution_id. It is the only caller of the aggregator, the
// derive LLM client, and the quality gate (PROJECT-scope only). Fanned one project per call; the cron + per-project fan-out are
// the scheduler's.
type Engine struct {
	// tx runs the terminal-write compare-and-set + persist inside ONE transaction, so a persist
	// failure rolls back the status claim and no half-written is_latest partition can commit.
	tx           dao.Transactor
	aggregator   contextAggregator
	llmClient    artifactDeriver
	qualityGate  qualityGrader
	libraryDAO   dao.IPromptLibraryDAO
	executionDAO dao.ITaskExecutionDAO
	// harnessArtifactDAO reads the repo's committed (VCS) harness artifacts, whose
	// canonical slugs feed the additive-dedup gate drop + LLM avoid-hint so the
	// derive flow stops re-suggesting an artifact the repo already commits.
	harnessArtifactDAO dao.IHarnessArtifactDAO
	store              artifactStore
	emitter            AnalyticsEmitter
	dataWindowDays     func() int
	// minReviewThemesPerRepo is the enriched-path admission floor: a repo below the
	// comment floor (classified top-N) whose theme-assignment count is under this is
	// terminated COMPLETED_NO_VISIBLE + REPO_FEW_THEMES with no artifacts. Read live so
	// the floor can be tuned without a restart. nil → no theme-floor gate (admit).
	minReviewThemesPerRepo func() int
	// flagEnabled re-checks the rollout / no-train gate (the derive flag) at
	// derive time. A derive task can sit behind the low-priority queue after admission
	// fanned it out, so the org may have opted out in the meantime; re-checking here
	// stops already-queued paid work, not just future fan-out. nil → always allowed
	// (the gate is a scheduler concern; a nil func means "not gated here").
	flagEnabled func(orgID string) bool
	// newTypesEnabled re-checks the hooks-and-agents flag (the hook/agent
	// feature gate) live at derive time. A stale true carried from admission could let an
	// old queued task re-persist hook/agent rows a rollback just scrubbed, so the
	// authoritative read is here, not the payload. nil means not gated here (allowed).
	newTypesEnabled func(orgID string) bool
}

// NewEngine wires the derivation engine. dataWindowDays supplies the read-window
// length (the derivation config's DataWindowDays) re-read per run so a
// dynamic-config change takes effect without a restart. flagEnabled re-checks the
// rollout gate at derive time (see the field doc).
func NewEngine(
	tx dao.Transactor,
	aggregator contextAggregator,
	llmClient artifactDeriver,
	qualityGate qualityGrader,
	libraryDAO dao.IPromptLibraryDAO,
	executionDAO dao.ITaskExecutionDAO,
	harnessArtifactDAO dao.IHarnessArtifactDAO,
	store artifactStore,
	emitter AnalyticsEmitter,
	dataWindowDays func() int,
	minReviewThemesPerRepo func() int,
	flagEnabled func(orgID string) bool,
	newTypesEnabled func(orgID string) bool,
) *Engine {
	return &Engine{
		tx:                     tx,
		aggregator:             aggregator,
		llmClient:              llmClient,
		qualityGate:            qualityGate,
		libraryDAO:             libraryDAO,
		executionDAO:           executionDAO,
		harnessArtifactDAO:     harnessArtifactDAO,
		store:                  store,
		emitter:                emitter,
		dataWindowDays:         dataWindowDays,
		minReviewThemesPerRepo: minReviewThemesPerRepo,
		flagEnabled:            flagEnabled,
		newTypesEnabled:        newTypesEnabled,
	}
}

// committedArtifactSlugs reads the repo's committed (VCS) harness artifacts and canonicalizes
// each into a (type, slug) key in the derive-flow identity space: the name is kebab-normalized
// (artifactslug.Kebab — so an underscore/space filename like "error_wrapping" or "pr_review"
// matches the LLM's kebab slug "error-wrapping" / "pr-review") then run through normalizeSlug
// for the skill "/" prefix. It is keyed by (type, slug), NOT slug alone, so a committed COMMAND
// or RULE named "audit" can never suppress a derived HOOK/TEMPLATE slugged "audit" — mechanical
// identity is (type, slug) everywhere in this flow. NOT artifactslug.PromptLibraryForm, which
// appends a source-link sha256 digest and so could never equal a derived slug. A
// committed type with no derive equivalent (COMMAND, CLAUDE_MD, AGENTS_MD, PERMISSIONS, …) maps
// to UNSPECIFIED and is skipped — those are instruction/config files, not slug-keyed artifacts.
// Tenant-scoped read; an empty result (a repo with no VCS ingest) is the common case and yields
// an empty set, not an error. A nil DAO (never in production wiring) disables the dedup.
func (e *Engine) committedArtifactSlugs(ctx context.Context, orgID, repoID string) (map[CommittedKey]struct{}, error) {
	keys := map[CommittedKey]struct{}{}
	if e.harnessArtifactDAO == nil {
		return keys, nil
	}
	var rows []*model.HarnessArtifact
	if err := scopedRead(ctx, func(sCtx context.Context) error {
		out, readErr := e.harnessArtifactDAO.ListVCSByRepo(sCtx, orgID, repoID)
		if readErr != nil {
			return fmt.Errorf("listing committed VCS artifacts for repo %s: %w", repoID, readErr)
		}
		rows = out
		return nil
	}); err != nil {
		return nil, fmt.Errorf("reading committed artifacts for org %s: %w", orgID, err)
	}
	for _, row := range rows {
		artifactType := committedArtifactType(row.Type)
		if artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_UNSPECIFIED {
			continue // a committed type the derive flow never produces — cannot collide with a suggestion
		}
		slug := normalizeSlug(artifactslug.Kebab(row.Name), artifactType)
		if slug == "" {
			continue
		}
		keys[CommittedKey{Type: artifactType, Slug: slug}] = struct{}{}
	}
	return keys, nil
}

// committedArtifactType maps a committed harness artifact's type to the promptlibrary ArtifactType
// the derive flow uses for the same primitive, so the committed dedup keys line up type-for-type
// with a derived suggestion's (type, slug). A committed type the derive flow never produces
// (COMMAND, CLAUDE_MD, AGENTS_MD, PROMPT_TEMPLATE, PLUGIN, PERMISSIONS) maps to UNSPECIFIED and is
// excluded from the set — it can share a name with a derived artifact without being the same thing.
func committedArtifactType(t commonv1.HarnessArtifactType) promptlibraryv1.ArtifactType {
	switch t {
	case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL:
		return promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL
	case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE:
		return promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE
	case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_HOOK:
		return promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK
	case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT:
		return promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT
	default:
		return promptlibraryv1.ArtifactType_ARTIFACT_TYPE_UNSPECIFIED
	}
}

// addCommittedAvoidHint appends the already-committed slugs to the LLM avoid-list carried by
// AdminFeedback.DismissedRuleSlugs — the field the derive prompt reads as "do not (re)generate
// these slugs". That field feeds ONLY the prompt; the gate's dismiss-suppression reads the
// dismissed set straight from the DAO, so seeding committed slugs here steers the LLM away from
// re-suggesting a committed artifact without touching the real dismiss set. The gate's
// committed_artifact drop is the hard backstop; this is the token-saving hint. The slug strings
// are lifted off the (type, slug) keys, deduped, and appended sorted so the run's the content store audit input
// is deterministic (the prompt avoids the name; the gate's type-keyed drop is what stays precise).
func addCommittedAvoidHint(aggCtx *promptlibraryv1.AggregatedContext, committed map[CommittedKey]struct{}) {
	if aggCtx == nil || len(committed) == 0 {
		return
	}
	fb := aggCtx.GetAdminFeedback()
	if fb == nil {
		fb = &promptlibraryv1.AdminFeedback{}
		aggCtx.AdminFeedback = fb
	}
	existing := make(map[string]struct{}, len(fb.GetDismissedRuleSlugs()))
	for _, slug := range fb.GetDismissedRuleSlugs() {
		existing[slug] = struct{}{}
	}
	newSlugs := make([]string, 0, len(committed))
	for key := range committed {
		if _, ok := existing[key.Slug]; ok {
			continue
		}
		existing[key.Slug] = struct{}{} // dedupe slugs shared across types (e.g. a rule + hook "audit")
		newSlugs = append(newSlugs, key.Slug)
	}
	sort.Strings(newSlugs)
	fb.DismissedRuleSlugs = append(fb.GetDismissedRuleSlugs(), newSlugs...)
}

// Run executes the derivation pipeline for one project. The task_executions row
// was created (with dedup_key = the project's repo id) by the enqueue handler and
// its id rides in the task payload, so a retry resumes the SAME row —
// PersistArtifacts stays idempotent per task_execution_id. securityOnly is the
// admission verdict, passed in and NOT recomputed; it only steers the
// security-governance context + the gate's path-check skip.
//
// Reads run directly; the terminal write runs in one transaction so the status
// compare-and-set and PersistArtifacts commit atomically. The content-store writes
// and the LLM derive call run outside any transaction. On any failure past the
// initial row load the run is marked FAILED with error_message; a load failure
// returns the error so the caller retries.
func (e *Engine) Run(ctx context.Context, orgID string, trigger taskexecutionsv1.TaskTriggerType, taskExecutionID string, securityOnly bool) error {
	var exec *model.TaskExecution
	if err := scopedRead(ctx, func(sCtx context.Context) error {
		loaded, err := e.executionDAO.GetByID(sCtx, orgID, taskExecutionID)
		if err != nil {
			return fmt.Errorf("getting task execution %s: %w", taskExecutionID, err)
		}
		exec = loaded
		return nil
	}); err != nil {
		// The row can't be loaded (missing / DB error) — there's nothing to mark
		// FAILED, so surface the error and let runner-retry the delivery.
		return fmt.Errorf("loading task execution %s for org %s: %w", taskExecutionID, orgID, err)
	}

	// Resume only a RUNNING row. at-least-once delivery and the admission
	// single-in-flight reaper (ReapStaleRunning marks a stale RUNNING row FAILED to
	// free the slot) both mean a redelivered task can resolve to an
	// already-terminal row. Re-running it would re-emit the analytics event, repeat
	// the paid derive, and — once the reaper freed the slot for a fresh run — race
	// that run's PersistArtifacts for the same project (reconcileLatest is not
	// concurrency-safe). Ack the redelivery as a no-op instead. (A genuine
	// crash-resume leaves the row RUNNING and proceeds; StaleAfter ≫ pipeline time,
	// so a mid-flight reap is not a steady-state concern.)
	if exec.Status != taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_RUNNING {
		logger.Info(ctx, "prompt-library: derivation run not RUNNING, skipping redelivery",
			zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", exec.ID), zap.String("status", exec.Status.String()))
		return nil
	}

	// runStart anchors the success-duration histogram, taken after the RUNNING-guard
	// so a no-op redelivery is never measured.
	runStart := time.Now()

	// scopeValue is the project's immutable repo id — the run's dedup_key (decision
	// 57/58), the gate's scopeValue, and every artifact's scope_value.
	scopeValue := exec.DedupKey

	// Re-check the rollout gate at derive time. The derive task can sit behind the
	// low-priority queue after it was enqueued, so the org may have opted out
	// of the derive flag since. Mark SKIPPED (a deliberate gate, not a failure)
	// and ack, before any enrich / paid LLM work. Checked before the analytics emit
	// so a gated run records no derivation_triggered event.
	if e.flagEnabled != nil && !e.flagEnabled(orgID) {
		e.skip(ctx, orgID, exec.ID, "the derive flag disabled at derive time", derivationRunsTotal)
		return nil
	}

	// The read window is the trailing DeriveConfig.DataWindowDays, re-read per run so a
	// config change takes effect without a restart.
	from, to := DerivationWindow(e.dataWindowDays())

	// Build the per-project aggregated context and the org-grain PR correlation
	// evidence. BuildContext resolves the project's repo by the run's authoritative
	// id (scope_value = dedup_key) and grounds against it directly, so a full_name
	// shared across connectors can't divert grounding from the repo artifacts
	// persist under; a missing repo (deleted mid-window) fails the run. It
	// self-scopes its reads, then runs the content store + classify HTTP unscoped.
	// Re-check the hooks-and-agents feature flag live (fail-closed): a stale true carried from
	// admission must not re-persist hook/agent rows a rollback scrubbed, and it gates the
	// hook/agent grounding reads so an org without the feature pays for none of them. A nil
	// resolver (never in production — wire always injects one) resolves to false, matching
	// the enqueue gate and stubs closure so no wiring path can silently enable the feature.
	newTypesEnabled := e.newTypesEnabled != nil && e.newTypesEnabled(orgID)
	aggCtx, enriched, _, err := e.aggregator.BuildContext(ctx, orgID, scopeValue, from, to, securityOnly, newTypesEnabled)
	if err != nil {
		e.fail(ctx, orgID, exec.ID, fmt.Errorf("building context for org %s: %w", orgID, err), derivationRunsTotal)
		return nil
	}

	// Additive-dedup: read the repo's already-committed VCS artifacts once and
	// canonicalize their names to the derive-flow slug form. The set feeds two places — the
	// LLM avoid-hint appended to aggCtx below (so DeriveArtifacts AND the audit blob see
	// it) and the quality gate's committed_artifact drop (threaded to Grade) — so the derive
	// flow stops re-suggesting an artifact the repo already commits. Empty for a repo with no
	// VCS ingest (the common case); only a real read error fails the run, matching the gate's
	// own dedup-read-error contract.
	committedSlugs, err := e.committedArtifactSlugs(ctx, orgID, scopeValue)
	if err != nil {
		e.fail(ctx, orgID, exec.ID, fmt.Errorf("reading committed artifacts for org %s: %w", orgID, err), derivationRunsTotal)
		return nil
	}
	addCommittedAvoidHint(aggCtx, committedSlugs)
	// Record the classify path so the enriched population (the theme-floor gate's
	// denominator) is measurable in prod. securityOnly runs never classify review themes.
	if !securityOnly {
		path := reviewClassifyPathStandard
		if enriched {
			path = reviewClassifyPathEnriched
		}
		derivationReviewClassifyRunsTotal.WithLabelValues(path).Inc()
	}

	// Enriched-path theme-floor admission (the review-comment floor's one-step-later
	// counterpart): a repo below the comment floor was classified into top-N themes; if its
	// theme-assignment count is still under the floor there is too little signal to ground
	// artifacts, so terminate with no derive — before the paid DeriveArtifacts + the content store writes.
	// A standard (≥comment-floor, 1:1) or securityOnly run is never enriched and skips this.
	if enriched && e.minReviewThemesPerRepo != nil {
		if assignments := reviewThemeAssignments(aggCtx); assignments < e.minReviewThemesPerRepo() {
			e.terminateFewThemes(ctx, orgID, exec.ID, assignments, runStart, derivationRunsTotal, derivationDurationSeconds)
			return nil
		}
	}

	// One derivation_triggered analytics event per run, emitted once EVERY
	// admission gate has cleared — the rollout gate above and the enriched
	// theme-floor gate just above — so a run that exits at any gate without deriving
	// records no event (else sparse-repo REPO_FEW_THEMES no-ops would inflate the triggered
	// funnel). Best-effort: a failed emit never fails the run. alreadyEmittedDerivationTriggered
	// is the once-per-run guard (it infers prior emission from a persisted marker, not the
	// runner-retry count — see its doc); the guard still holds because the emit stays BEFORE
	// the input-key write below. The emit ctx is detached from cancellation so a graceful
	// shutdown between here and the input-key write still records the run's only event.
	if !alreadyEmittedDerivationTriggered(exec) {
		e.emitter.EmitDerivationTriggered(context.WithoutCancel(ctx), orgID, scopeValue, exec.ID, trigger)
	}

	prCorrelation, err := e.aggregator.ComputePRCorrelation(ctx, orgID, from, to)
	if err != nil {
		e.fail(ctx, orgID, exec.ID, fmt.Errorf("computing pr correlation for org %s: %w", orgID, err), derivationRunsTotal)
		return nil
	}

	// Persist the run input to the content store (audit/lineage) and store only the KEY on the row
	//. Check the Update error (I3) — a dropped ref silently loses the
	// audit pointer.
	inputKey, err := e.store.PutRunInput(ctx, orgID, exec.ID, aggCtx)
	if err != nil {
		e.fail(ctx, orgID, exec.ID, fmt.Errorf("persisting run input to the content store for org %s: %w", orgID, err), derivationRunsTotal)
		return nil
	}
	if err := e.updateExec(ctx, orgID, &model.TaskExecution{ID: exec.ID, InputPayload: &taskexecutionsv1.ContentRef{Key: inputKey}},
		taskexecutionsv1.TaskExecutionFieldMask_TASK_EXECUTION_FIELD_MASK_INPUT_PAYLOAD); err != nil {
		e.fail(ctx, orgID, exec.ID, fmt.Errorf("persisting input ref for org %s: %w", orgID, err), derivationRunsTotal)
		return nil
	}

	// Derive (tetriz-py), then persist the raw response to the content store + its ref. rawOutput
	// is the exact normalized JSON tetriz-py returned (not a re-marshal of the
	// parsed struct), so the audit blob stays faithful even if Python adds a field
	// the Go model does not yet carry.
	result, rawOutput, err := e.llmClient.DeriveArtifacts(ctx, aggCtx, orgID, exec.ID)
	if err != nil {
		e.fail(ctx, orgID, exec.ID, fmt.Errorf("deriving artifacts for org %s: %w", orgID, err), derivationRunsTotal)
		return nil
	}
	outputKey, err := e.store.PutRunOutput(ctx, orgID, exec.ID, rawOutput)
	if err != nil {
		e.fail(ctx, orgID, exec.ID, fmt.Errorf("persisting run output to the content store for org %s: %w", orgID, err), derivationRunsTotal)
		return nil
	}
	if err := e.updateExec(ctx, orgID, &model.TaskExecution{ID: exec.ID, OutputPayload: &taskexecutionsv1.ContentRef{Key: outputKey}},
		taskexecutionsv1.TaskExecutionFieldMask_TASK_EXECUTION_FIELD_MASK_OUTPUT_PAYLOAD); err != nil {
		e.fail(ctx, orgID, exec.ID, fmt.Errorf("persisting output ref for org %s: %w", orgID, err), derivationRunsTotal)
		return nil
	}

	// Grade (deterministic Go validators — no LLM judge). Grade reads the project's
	// dismissed/recent-hidden titles, so it MUST run inside a tenant scope; a dedup
	// read error fails the run (the deterministic equivalent of the former
	// judge-error→FAILED). repoTree (nil-safe) grounds cited paths; the security
	// summary is the evidence a securityOnly run grades artifacts against.
	if securityOnly && len(securitySlugsWithEvidence(aggCtx.GetSecurityIncidents())) == 0 {
		// securityOnly was set at admission on a non-zero incident count, yet the
		// summary the gate grades against is empty, so every artifact will be hidden
		// (COMPLETED_NO_VISIBLE). Two skews produce this: a window/scope skew between
		// the admission gate read and the derive read, or a taxonomy skew —
		// admission counts every flag_type while SecuritySummary maps only the known
		// slugs, so a new/unmapped security slug trips the gate yet carries no
		// gradeable evidence (buildSecuritySummary's "unmapped security flag_type"
		// warn is the precise signal for that case). Loud-log so it is observable
		// rather than silently producing an empty library.
		logger.WarnWithCtx(ctx, "prompt-library: securityOnly run has no security evidence in context; all artifacts will be hidden (window/scope or unmapped-slug skew)",
			zap.String(logger.ORG_ID, orgID), zap.String("scope_value", scopeValue))
	}
	// The repo's low/high session-bucket counts gate TEMPLATE visibility only (Rules/Skills
	// are VCS-derived). They live on the session summary the aggregator already built for
	// this project, so no extra read.
	sessionDist := aggCtx.GetSessionSummary().GetQualityDistribution()
	sessionLow, sessionHigh := int(sessionDist["low"]), int(sessionDist["high"])
	var graded *GradedResult
	if err := scopedRead(ctx, func(sCtx context.Context) error {
		g, gradeErr := e.qualityGate.Grade(sCtx, orgID, scopeValue, result, firstRepoTree(aggCtx), aggCtx.GetSecurityIncidents(), sessionLow, sessionHigh, securityOnly, newTypesEnabled, aggCtx.GetResolvedTargetTool(), committedSlugs)
		if gradeErr != nil {
			return fmt.Errorf("quality gate for org %s: %w", orgID, gradeErr)
		}
		graded = g
		return nil
	}); err != nil {
		e.fail(ctx, orgID, exec.ID, fmt.Errorf("grading run for org %s: %w", orgID, err), derivationRunsTotal)
		return nil
	}

	// Convert to rows — writes each artifact's full content + evidence to the content store here,
	// BEFORE the persist scope opens (no tx across the content store).
	items, err := e.toModels(ctx, orgID, exec.ID, scopeValue, graded, aggCtx, prCorrelation)
	if err != nil {
		e.fail(ctx, orgID, exec.ID, fmt.Errorf("assembling artifacts for org %s: %w", orgID, err), derivationRunsTotal)
		return nil
	}

	// Persist the graded artifacts (incl. quality-failed HIDDEN_FAILED_EVAL — decision
	// 3; security_scope hides are excluded in toModels, see there) and stamp
	// the terminal status in ONE scope, so they commit atomically: there is no window
	// where artifacts are committed but the row is still RUNNING for a retry to re-run
	// and then fail over already-visible rows. COMPLETED_NO_VISIBLE is distinct from
	// COMPLETED (observable, excluded from the failure SLO); the prior visible
	// generation stays latest. NoneVisible covers TWO cases: an
	// all-hidden run (items > 0 — those HIDDEN rows ARE persisted for dedup/audit, and
	// reconcileLatest re-promotes the prior visible generation) and a zero-artifact
	// derive (no items — PersistArtifacts rejects an empty batch, so the persist is
	// skipped below). Both stamp COMPLETED_NO_VISIBLE and leave the prior visible
	// generation untouched; only the persist call is conditional on len(items) > 0.
	status := taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED
	if graded.NoneVisible() {
		status = taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE
	}
	now := time.Now().UTC()
	if err := e.tx.InTx(ctx, func(sCtx context.Context) error {
		// Claim the terminal transition FIRST with an atomic compare-and-set: stamp
		// the status only while the row is still RUNNING. The stale-run reaper
		// (ReapStaleRunning) can fail this row mid-flight if an unscoped step
		// (classify / derive / the content store) overran StaleAfter, freeing the
		// single-in-flight slot for a fresh run; if it did, the guarded UPDATE
		// matches 0 rows (marked=false) and we abort BEFORE persisting, so this stale
		// worker never writes artifacts that would race the fresh run's is_latest
		// reconcile. The UPDATE's row lock is held for the rest of the tx, so the
		// reaper can't flip the row after we win the claim — closing the race that a
		// read-then-write check would leave open under READ COMMITTED.
		// COMPLETED / COMPLETED_NO_VISIBLE is a success transition — no error_message.
		marked, markErr := e.executionDAO.MarkTerminalIfRunning(sCtx, orgID, exec.ID, status,
			taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_UNSPECIFIED, now, "")
		if markErr != nil {
			return fmt.Errorf("stamping terminal status %s: %w", status, markErr)
		}
		if !marked {
			return errRunSuperseded
		}
		// A zero-artifact derive (NoneVisible) yields no items; PersistArtifacts
		// rejects an empty batch, so skip it — the terminal status is already
		// COMPLETED_NO_VISIBLE and the prior visible generation is untouched. A
		// persist failure rolls back the whole tx (including the status claim above).
		if len(items) > 0 {
			if persistErr := e.libraryDAO.PersistArtifacts(sCtx, orgID, exec.ID, items); persistErr != nil {
				return fmt.Errorf("persisting artifacts: %w", persistErr)
			}
		}
		return nil
	}); err != nil {
		// Superseded: the reaper failed this row and a fresh run owns the work. Ack as
		// a no-op — the guarded UPDATE wrote nothing, so there is no foreign-row state
		// to undo beyond this tx's own rollback.
		if errors.Is(err, errRunSuperseded) {
			derivationRunsSupersededTotal.Inc()
			logger.Info(ctx, "prompt-library: derivation run superseded before persist, skipping",
				zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", exec.ID))
			return nil
		}
		// Persist + status claim roll back together, so nothing was committed — mark
		// FAILED (the weekly cron retries) rather than leave a RUNNING row a retry re-runs.
		e.fail(ctx, orgID, exec.ID, fmt.Errorf("persisting run for org %s: %w", orgID, err), derivationRunsTotal)
		return nil
	}

	// Success owned: the terminal write committed (a persist-rollback fell through to
	// e.fail above, so this is unreachable then). Count it — no_visible kept distinct
	// from completed so it stays out of the failure SLO — and observe the duration.
	resultLabel := resultCompleted
	if status == taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE {
		resultLabel = resultNoVisible
	}
	derivationRunsTotal.WithLabelValues(resultLabel).Inc()
	derivationDurationSeconds.Observe(time.Since(runStart).Seconds())
	return nil
}

// alreadyEmittedDerivationTriggered reports whether a prior delivery of this run
// already emitted the once-per-run derivation_triggered event, so a
// crash/timeout retry resuming the same RUNNING row does not re-emit.
//
// It infers prior emission from a PERSISTED marker, not the runner-retry count: the
// only path that returns an error to the runner (→ retry) is the initial row-load failure,
// which is BEFORE the emit, so a retry reaching the emit at retryCount>0 is still the
// FIRST delivery to get there — a retry-count guard would wrongly suppress the run's
// only event. The marker is the run input's the content store key: CreateIfNoneRunning seeds
// InputPayload as a non-nil empty ContentRef{} (NOT NULL column), and Run writes the key
// only AFTER emitting, so a non-empty key means a prior attempt got past the emit.
//
// COUPLING (the one place it lives): this ties "already emitted" to "input written".
// They are ordered correctly today (emit precedes the input-key write in Run, asserted
// by TestEngine_Run_HappyPath). If that ordering ever changes — the input written
// earlier (e.g. at run creation, or before the emit) — this predicate breaks silently
// (every first delivery would look already-emitted → no event); that test trips first.
// The durable fix (a dedicated emitted-at marker, to do when the feature un-dormants) is
// . The narrow crash window between the emit and the input-key write (a
// retry re-emits) is closed by the warehouse de-duping on task_execution_id.
func alreadyEmittedDerivationTriggered(exec *model.TaskExecution) bool {
	return exec.InputPayload.GetKey() != ""
}

// fail marks the run FAILED with cause's message and logs the failure (
// error_message holds the cause). Run then returns nil and the runner acks, so
// an in-band failure does NOT retry — the weekly cron retries the failed project.
//
// It persists ONLY while the handler ctx is live. A canceled ctx means a DeriveTaskTimeout
// fired or the pod is shutting down — and on a timeout the runner immediately schedules a retry
// of THIS task (the runner processor: deadline → retry, scheduled while the handler
// goroutine is still unwinding). That retry resumes the same still-RUNNING row, so writing
// FAILED here would race it and, if this write won the compare-and-set, supersede the
// retry and drop its paid derive. So on a canceled ctx fail writes nothing and leaves the
// row RUNNING for the retry to own; the StaleAfter reaper backstops retry-exhaustion or a
// dead worker. On a live ctx it stamps FAILED via the status=RUNNING compare-and-set
// (MarkTerminalIfRunning, as skip and the success path use), which no-ops — never
// clobbers — if the reaper already failed the row mid-flight.
func (e *Engine) fail(ctx context.Context, orgID, execID string, cause error, runsTotal *prometheus.CounterVec) {
	message := cause.Error()
	logger.Error(ctx, "prompt-library: derivation run failed",
		zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", execID), zap.String("error", message))
	if ctxErr := ctx.Err(); ctxErr != nil {
		// Timeout / shutdown: the runner is retrying this task, which resumes the RUNNING row.
		// Writing a terminal status here would race that retry and could supersede it, so
		// leave the row RUNNING and let the retry own the outcome (the reaper backstops).
		// The ctx error distinguishes a fired Timeout (DeadlineExceeded) from a pod
		// shutdown (Canceled).
		logger.Info(ctx, "prompt-library: skipping FAILED write, handler ctx canceled (runner-retry owns the run)",
			zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", execID), zap.Error(ctxErr))
		return
	}
	var marked bool
	if err := e.tx.InTx(ctx, func(sCtx context.Context) error {
		var markErr error
		marked, markErr = e.executionDAO.MarkTerminalIfRunning(sCtx, orgID, execID,
			taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_FAILED,
			taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_UNSPECIFIED, time.Now().UTC(), message)
		if markErr != nil {
			return fmt.Errorf("marking run failed: %w", markErr)
		}
		if !marked {
			// Reaper already failed this row mid-flight — don't clobber its outcome.
			logger.Info(sCtx, "prompt-library: FAILED write skipped, run no longer RUNNING (reaped)",
				zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", execID))
		}
		return nil
	}); err != nil {
		logger.Error(ctx, "prompt-library: failed to persist FAILED status",
			zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", execID), zap.Error(err))
		return
	}
	// Count only a committed FAILED transition this run owned. A reaper-failed row
	// (!marked) is the reaper's (runs_reaped_total), and a failed commit returned above.
	if marked {
		runsTotal.WithLabelValues(resultFailed).Inc()
	}
}

// skip marks the run SKIPPED (a deliberate gate outcome — opted out at derive
// time — not a failure, so no error_message) and logs the reason, via the status=RUNNING
// compare-and-set so it no-ops rather than clobbering a row the reaper already failed.
// Like fail it persists only while the handler ctx is live: a canceled ctx means the runner is
// retrying/redelivering this task, and the redelivery re-checks the same gate and will
// SKIP again (idempotent), so writing here would only race it. The gate checks run before
// any long work, so a canceled ctx at skip() is a pod-shutdown edge, not the derive Timeout.
func (e *Engine) skip(ctx context.Context, orgID, execID, reason string, runsTotal *prometheus.CounterVec) {
	logger.Info(ctx, "prompt-library: derivation run skipped",
		zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", execID), zap.String("reason", reason))
	if ctxErr := ctx.Err(); ctxErr != nil {
		logger.Info(ctx, "prompt-library: skipping SKIPPED write, handler ctx canceled (redelivery re-gates)",
			zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", execID), zap.Error(ctxErr))
		return
	}
	var marked bool
	if err := e.tx.InTx(ctx, func(sCtx context.Context) error {
		var markErr error
		marked, markErr = e.executionDAO.MarkTerminalIfRunning(sCtx, orgID, execID,
			taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_SKIPPED,
			taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_UNSPECIFIED, time.Now().UTC(), "")
		if markErr != nil {
			return fmt.Errorf("marking run skipped: %w", markErr)
		}
		return nil
	}); err != nil {
		logger.Error(ctx, "prompt-library: failed to persist SKIPPED status",
			zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", execID), zap.Error(err))
		return
	}
	// Count only a committed SKIPPED transition this run owned (a reaper-failed row
	// no-ops the compare-and-set: !marked → not a skip). runsTotal is the caller's
	// pipeline counter, so a recommend skip lands on recommendationRunsTotal, not the
	// derive series.
	if marked {
		runsTotal.WithLabelValues(resultSkipped).Inc()
	}
}

// reviewThemeAssignments sums the classified theme assignments in the aggregated
// context (Σ ReviewThemeSummary.Count). "other" is already excluded upstream in
// bucketThemes, so this counts only engineering-theme assignments — the enriched-path
// admission signal. On the enriched path one comment can add to several themes, so
// this exceeds the comment count for a multi-topic corpus (that is the intended credit).
func reviewThemeAssignments(aggCtx *promptlibraryv1.AggregatedContext) int {
	total := 0
	for _, theme := range aggCtx.GetReviewThemes() {
		total += int(theme.GetCount())
	}
	return total
}

// terminateFewThemes stamps an enriched run that cleared the comment floor but not the
// theme floor as terminal COMPLETED_NO_VISIBLE + REPO_FEW_THEMES with no artifacts — the
// symmetric counterpart to skip()'s SKIPPED write, one derivation step later. It persists
// nothing, counts as a no-visible completion, and follows the same ctx-cancel guard +
// compare-and-set (no-op if the row is no longer RUNNING) as skip/fail.
func (e *Engine) terminateFewThemes(ctx context.Context, orgID, execID string, assignments int, runStart time.Time, runsTotal *prometheus.CounterVec, duration prometheus.Histogram) {
	logger.Info(ctx, "prompt-library: enriched run below theme floor, no artifacts",
		zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", execID),
		zap.Int("theme_assignments", assignments))
	if ctxErr := ctx.Err(); ctxErr != nil {
		logger.Info(ctx, "prompt-library: skipping REPO_FEW_THEMES write, handler ctx canceled (redelivery re-gates)",
			zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", execID), zap.Error(ctxErr))
		return
	}
	var marked bool
	if err := e.tx.InTx(ctx, func(sCtx context.Context) error {
		var markErr error
		marked, markErr = e.executionDAO.MarkTerminalIfRunning(sCtx, orgID, execID,
			taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE,
			taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_REPO_FEW_THEMES, time.Now().UTC(), "")
		if markErr != nil {
			return fmt.Errorf("marking run repo_few_themes: %w", markErr)
		}
		return nil
	}); err != nil {
		logger.Error(ctx, "prompt-library: failed to persist REPO_FEW_THEMES status",
			zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", execID), zap.Error(err))
		return
	}
	// Count only a committed transition this run owned; observe duration like the other
	// no-visible completions so the success-duration histogram stays representative.
	if marked {
		runsTotal.WithLabelValues(resultNoVisible).Inc()
		duration.Observe(time.Since(runStart).Seconds())
	}
}

// updateExec applies a masked task_executions Update.
func (e *Engine) updateExec(ctx context.Context, orgID string, row *model.TaskExecution, masks ...taskexecutionsv1.TaskExecutionFieldMask) error {
	if err := e.executionDAO.Update(ctx, row, masks); err != nil {
		return fmt.Errorf("updating task execution %s for org %s: %w", row.ID, orgID, err)
	}
	return nil
}

// DerivationWindow returns the [from, to] derivation read window: to=now, from anchored
// on now minus days (a trailing window). A non-positive day count means the whole
// available history (zero `from`). The enqueue handler and the Engine share this so their
// windows cannot drift.
func DerivationWindow(days int) (time.Time, time.Time) {
	to := time.Now().UTC()
	if days <= 0 {
		return time.Time{}, to
	}
	from := to.Truncate(24*time.Hour).AddDate(0, 0, -days)
	return from, to
}

// ReviewClassifyWindow narrows the review-body classify read to the trailing
// lookbackDays, clamped so it never widens past the derivation `from` floor
// . Applied ONLY to the review read — the org-grain quality/security
// aggregations keep the broad [from, to] window. A non-positive lookbackDays leaves
// the window at `from` (no narrowing). The per-repo admission gate and the Engine's
// BuildContext share this so the gate's review-count and the corpus the derive
// classifies cannot drift.
func ReviewClassifyWindow(from, to time.Time, lookbackDays int) time.Time {
	if lookbackDays <= 0 {
		return from
	}
	if clamped := to.AddDate(0, 0, -lookbackDays); clamped.After(from) {
		return clamped
	}
	return from
}

// firstRepoTree returns the project's single repo tree (one tree per call), or
// nil when the project had no mapped/available tree — the gate then grounds no
// cited path and path-citing artifacts are hidden.
func firstRepoTree(aggCtx *promptlibraryv1.AggregatedContext) *promptlibraryv1.RepoTree {
	trees := aggCtx.GetRepoTrees()
	if len(trees) == 0 {
		return nil
	}
	return trees[0]
}
