// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/errgroup"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/logger"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// harnessImproveScanRunsTotal is the harness-improve scan's own run-outcome counter — an
// INDEPENDENT lane from the derive flow's derivationRunsTotal, so its reaped/failed rate
// is measurable on its own. result ∈ {ok, failed, reaped, skipped}.
var harnessImproveScanRunsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "recommendations",
	Subsystem: "harness_improve",
	Name:      "scan_runs_total",
	Help:      "Harness-improve scan run outcomes, by result.",
}, []string{"result"})

// registerHarnessImproveMetrics registers this file's collector with the given registerer and seeds
// the full result label set so every outcome shows from the first scrape. Called by RegisterMetrics
// (opt-in); nothing is registered at import (no import side effects).
func registerHarnessImproveMetrics(reg prometheus.Registerer) {
	reg.MustRegister(harnessImproveScanRunsTotal)
	for _, result := range []string{"ok", "failed", "reaped", "skipped"} {
		harnessImproveScanRunsTotal.WithLabelValues(result).Add(0)
	}
}

// The three consts below are Go-seed DEFAULTS for config-tunable detector calibration: each has a
// DetectorConfig field that overrides it, and the engine reads the resolved value from detectorCfg —
// the const is only the fallback used when the field is unset.

// harnessImproveBehavioralWindowDays is the trailing window the behavioral signals are read over.
const harnessImproveBehavioralWindowDays = 90

// maxDetectorBodiesPerRun caps how many DISTINCT artifact bodies one scan fetches from object
// storage. A repo with more distinct instruction bodies than the cap has the excess deferred (the
// keys are sorted so the truncation is deterministic and the same subset is fetched across runs),
// keeping one scan's the content store round-trips bounded rather than unbounded in repo size.
const maxDetectorBodiesPerRun = 200

// maxBehavioralClassifyPerRun caps how many DISTINCT-content intent classifications one scan issues.
// The enrich phase already dedups by body content (one classify per distinct body, fanned to every
// candidate sharing it), so this is the backstop for a repo with many DISTINCT candidate bodies —
// the content keys are sorted so a truncation drops the same tail across runs, mirroring the
// body-fetch cap.
const maxBehavioralClassifyPerRun = 200

// detectorFanoutLimit is the max in-flight concurrency for every one of the engine's I/O fan-outs —
// the content store body fetch, the intent-classify LLM leg, and both fix-generate LLM legs. One knob so the
// engine's blast radius on the content store/tetriz-py is a single number, not four scattered literals.
const detectorFanoutLimit = 8

// maxRepoPathsEntries caps how many committed file paths one scan ingests from the repo tree blob.
// A tree larger than this is clipped and the aggregate is marked treeTruncated, so the path-asserting
// lenses (dead-glob, broken-path) degrade safely — they never assert non-existence against a partial
// listing — rather than building an unbounded path set from a huge repo.
const maxRepoPathsEntries = 50000

// HarnessImproveEngine scans one tracked repo's committed harness and persists detector findings
// dark at both org-facing serving surfaces. It is a SIBLING of
// Engine — it reuses the run lifecycle (RUNNING-guard, MarkTerminalIfRunning, fail), but replaces the single-LLM derive middle with a
// detector-first pipeline: deterministic Go checkers plus conditional per-artifact LLM legs
// (intent classification, fix drafting).
type HarnessImproveEngine struct {
	// tx runs the terminal-write compare-and-set + persist inside ONE transaction, so a persist
	// failure rolls back the status claim and no half-written is_latest partition can commit.
	tx           dao.Transactor
	executionDAO dao.ITaskExecutionDAO
	// harnessArtifactDAO reads the repo's committed (VCS, person-less) harness content — the input
	// every read filters to artifact_source = VCS (ListVCSByRepo).
	harnessArtifactDAO dao.IHarnessArtifactDAO
	// repositoryDAO resolves the tracked repo's row to its tree_content — the committed repo file-path
	// listing the Conflict detector's glob/path lenses resolve against.
	repositoryDAO dao.IRepositoryDAO
	// securityDAO is a signal source (open flags by kind), clamped to the config window.
	securityDAO dao.ISecurityFlagDAO
	libraryDAO  dao.IPromptLibraryDAO
	// fileDAO resolves a committed artifact's current_file_id to its harness_files row, whose content
	// column holds the body — the detectors measure artifact CONTENT, not just its identity.
	fileDAO dao.IHarnessFileDAO
	// store persists a produced fix's artifact content/evidence (the write-side storage seam; the
	// single-tenant implementation is content-column-backed, wired by the caller).
	store artifactStore
	// sessionDAO is the behavioral-attribution bridge source: per-invoked-skill behavior over the
	// trailing window (AggregateInvokedSkillBehavior), read inside the aggregate scope.
	sessionDAO dao.ISessionInsightDAO
	// usageDAO is the personal lane's two reads: which engineers this repo run owns (their primary
	// repo) and their in-window invocation totals — the never-used lens's signal (see personal.go).
	usageDAO dao.IHarnessUsageRecordsDAO
	// classify is the LLM leg of the behavioral lens — the enrich phase's per-artifact intent
	// classifier, called OUTSIDE the tenant transaction (a tenant tx must not span an LLM call).
	classify harnessIntentClassifier
	// fix is the LLM leg of the fix producer — the per-finding body drafter for every content-fix lane
	// (the Instruction-Intent rewrites, behavioral-deviation, the Conflict remove-line families, and the
	// org generate-from-scratch path), called OUTSIDE the tenant transaction (same position as classify).
	// The Conflict broken-path lane is deterministic (PatchBrokenPath) and needs no collaborator.
	fix harnessFixGenerator
	// detectorCfg is the resolved behavioral-attribution calibration this engine runs on — the
	// per-intent-class bands, the classifier confidence floor, the trailing window, and the
	// name-alias map. Resolved once at construction from the injected config; the aggregate,
	// enrich, and behavioral-deviation consumers read it (never the package-const seeds directly),
	// so a tuned DetectorConfig value takes effect.
	detectorCfg detectorSettings
	// flagEnabled re-checks the detectors feature flag at scan time (dequeue). Stage-1 gates the
	// fan-out, but a scan task queued before a rollback flipped the flag off (a rollback +
	// a data scrub) must NOT run the detectors and repopulate DETECTED rows after the scrub;
	// Run/RunOrgScan skip (mark SKIPPED) when it reports false. Nil ⇒ no gate (never nil in assembly).
	flagEnabled func(orgID string) bool
}

// NewHarnessImproveEngine builds the sibling engine over the reused lifecycle DAOs, the
// committed-content and behavioral readers, and the content store. detectorCfg tunes the detectors
// (start from DefaultDetectorConfig and override the fields you need); a nil value, and every field a
// non-nil value leaves unset, falls back to the built-in bands/floor/window/aliases/caps/markers.
func NewHarnessImproveEngine(
	tx dao.Transactor,
	executionDAO dao.ITaskExecutionDAO,
	harnessArtifactDAO dao.IHarnessArtifactDAO,
	repositoryDAO dao.IRepositoryDAO,
	securityDAO dao.ISecurityFlagDAO,
	libraryDAO dao.IPromptLibraryDAO,
	fileDAO dao.IHarnessFileDAO,
	store artifactStore,
	sessionDAO dao.ISessionInsightDAO,
	usageDAO dao.IHarnessUsageRecordsDAO,
	classify harnessIntentClassifier,
	fix harnessFixGenerator,
	detectorCfg *DetectorConfig,
	flagEnabled func(orgID string) bool,
) *HarnessImproveEngine {
	return &HarnessImproveEngine{
		tx:                 tx,
		executionDAO:       executionDAO,
		harnessArtifactDAO: harnessArtifactDAO,
		repositoryDAO:      repositoryDAO,
		securityDAO:        securityDAO,
		libraryDAO:         libraryDAO,
		fileDAO:            fileDAO,
		store:              store,
		sessionDAO:         sessionDAO,
		usageDAO:           usageDAO,
		classify:           classify,
		fix:                fix,
		detectorCfg:        detectorSettingsFromConfig(detectorCfg),
		flagEnabled:        flagEnabled,
	}
}

// harnessImproveAggregate is the typed output of the direct-DAO aggregate read (NOT BuildContext —
// that assembles an LLM-prompt payload). The detectors consume these typed rows.
type harnessImproveAggregate struct {
	// committed is the repo's committed VCS harness artifacts (person-less, artifact_source = VCS).
	committed []*model.HarnessArtifact
	// securityByFlagType is the open-flag counts over the behavioral window.
	securityByFlagType map[string]int
	// bodies maps a committed artifact's ID to its committed body text. An artifact is absent
	// when its body is unavailable (no resolvable file, a missing the content store object, or a transient read
	// error) — the detectors then simply produce no finding for it.
	bodies map[string]string
	// repoPaths is the set of every committed file path in the repo tree (blob entries only,
	// UNSTRIPPED — the raw tree, not the derivation strip), the ground truth the Conflict
	// detector's glob and path-reference lenses resolve against. Empty when the repo has no
	// tree_key, the tree object is missing, or the read/parse degraded (never a hard fail).
	repoPaths map[string]struct{}
	// treeTruncated is true when the repo tree blob arrived vendor-truncated (clipped at fetch).
	// The path/glob lenses that assert non-existence (dead-glob, broken-path) must skip a
	// truncated tree — a path absent from a clipped listing may exist in the unlisted remainder.
	treeTruncated bool
	// behavioralByName is the org-wide per-invoked-skill behavior over the trailing window, keyed by
	// the CANONICAL name (resolveCanonicalName), so the behavioral lens compares by the same key the
	// committed artifact resolves to. Rows whose raw names canonicalize to one key are pooled.
	behavioralByName map[string]dao.InvokedSkillBehavior
	// nameCollisionDemoted is the set of canonical names that map to ≥2 distinct committed contents
	// (≥2 distinct ContentHash), so the behavioral lens must NOT attribute org-wide behavior to any
	// single artifact with that name — the pooled behavior is not attributable to one content.
	nameCollisionDemoted map[string]struct{}
	// behavioralDegraded is true when any behavioral candidate's intent classification degraded this
	// run (an LLM error, an ambiguous class, a below-floor confidence, or a classify-cap truncation),
	// so the findings gate learns behavioralComplete=false and treats the behavioral partition as only
	// partially evaluated. Set from enrichIntents' return AFTER the aggregate reads, since the classify
	// leg runs outside the read scope.
	behavioralDegraded bool
	// liveDetectedPartitions is the org's current live (is_latest, VISIBLE) DETECTED partitions for this
	// repo scope. A live-detected partition whose ArtifactType has NO committed artifact this run (a
	// vanished type — the artifact was deleted) is unioned into the run's evaluated set so its stale
	// finding auto-closes; the committed list is the authoritative "does this type still exist".
	liveDetectedPartitions []evaluatedPartition
	// personal is the personal lane's input: the engineers whose primary repo this is, their own
	// (DESKTOP, USER_GLOBAL/REPO_LOCAL) artifacts, and their in-window invocation totals. Read in the
	// same short scope as committed; consumed by personalFindings (see personal.go).
	personal personalAggregate
}

// Run scans one repo's committed harness for a task_executions run and persists findings dark.
//
// Lifecycle (mirrors Engine.Run): a RUNNING-guard resumes only a RUNNING row (at-least-once delivery
// + the reaper make redelivery of a terminal row a no-op); the direct DAO reads build the aggregate,
// the detect/LLM middle runs with no open transaction, and the terminal transition claims an atomic
// compare-and-set and persists inside ONE transaction (dao.Transactor.InTx), so a persist failure
// rolls back the status claim. Single-tenant: every record is processed.
//
// Every committed-harness read filters artifact_source = VCS (the identity index excludes the
// discriminator, so an unfiltered read surfaces desktop/person rows); the one per-person read is the
// personal lane's ListPersonalByPersons, which filters the inverse (DESKTOP + person_id IN) and feeds
// only PERSON-scoped findings (see personal.go).
func (e *HarnessImproveEngine) Run(ctx context.Context, orgID string, trigger taskexecutionsv1.TaskTriggerType, taskExecutionID string) error {
	// Wall-clock start for scan_duration_seconds, observed only on the success terminal below.
	start := time.Now()
	exec, err := e.executionDAO.GetByID(ctx, orgID, taskExecutionID)
	if err != nil {
		// A load failure returns the error so the caller retries; no row to mark FAILED yet.
		return fmt.Errorf("loading harness-improve run %s for org %s: %w", taskExecutionID, orgID, err)
	}
	if exec.Status != taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_RUNNING {
		// A redelivery of an already-terminal row: ack as a no-op (the reaper or a prior run owns it).
		logger.Info(ctx, "harness-improve: run not RUNNING, skipping redelivery",
			zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", taskExecutionID),
			zap.String("status", exec.Status.String()))
		return nil
	}
	// Re-check the rollout flag at dequeue: Stage-1 gates the fan-out, but a scan task queued before
	// the detectors feature flag was flipped off (a rollback + a data scrub) must not run
	// the detectors and repopulate DETECTED rows after the scrub. Mark SKIPPED, before any reads, and ack.
	if e.flagEnabled != nil && !e.flagEnabled(orgID) {
		e.skipRun(ctx, orgID, exec.ID, "the detectors feature flag disabled at scan time")
		return nil
	}

	scopeValue := exec.DedupKey // the repo's vcs_repositories.id

	// Run-start telemetry — records the trigger (scheduled vs a manual re-scan) so an operator can tell
	// an ad-hoc run from the regular cadence in the logs.
	logger.Info(ctx, "harness-improve: per-repo scan started",
		zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", taskExecutionID),
		zap.String(logger.REPO_ID, scopeValue), zap.String("trigger", trigger.String()))

	// Single-tenant: the window comes from config, and every record in it is read.

	// The direct typed DAO reads (NOT BuildContext) for the committed content + signals.
	aggregate, err := e.aggregate(ctx, orgID, scopeValue, exec.StartedAt)
	if err != nil {
		e.failRun(ctx, orgID, exec.ID, err)
		return nil
	}

	// OUTSIDE any tenant transaction: the LLM enrich phase (per-artifact intent classify) runs here,
	// exactly like the content store body-fetch fan-out — a tenant tx must not span the LLM calls. Its output
	// feeds the deterministic detect/grade middle, which stays PURE (no I/O of its own).
	intentByArtifactID, behavioralDegraded := e.enrichIntents(ctx, orgID, &aggregate)
	aggregate.behavioralDegraded = behavioralDegraded
	findings := e.detectAndGrade(ctx, orgID, scopeValue, exec.ID, aggregate, intentByArtifactID)

	// FIX PRODUCER (OUTSIDE any tenant transaction, same position as enrichIntents): route each
	// finding to its fix lane and produce a suggested body — deterministic for a broken-path rename,
	// an LLM draft for every content-fix family (the Instruction-Intent rewrites, behavioral-deviation,
	// the Conflict remove-line families), none for the advisory rest. The gate below grounds each fix's
	// cited paths and drops an ungrounded one to advisory, so a hallucinated body never persists.
	fixes := e.produceFixes(ctx, orgID, aggregate, findings)

	// FINDINGS GATE (OUTSIDE any tenant transaction): suppress, ground fixes, and compute the
	// fully-evaluated partition set. gateFindings grounds each produced fix's cited paths (an
	// ungrounded fix is dropped while its finding stays advisory), and its grounded-fix set is what
	// attachGroundedFixes persists. evaluated is the fully-evaluated partition set the auto-close
	// reconcile consumes.
	evalStatus := aggregate.deriveEvaluationStatus(promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, scopeValue)
	gated, evaluated, groundedFixes, hiddenReasons, err := e.gateFindings(ctx, orgID, scopeValue,
		promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, findings, fixes, aggregate.repoPaths, evalStatus)
	if err != nil {
		e.failRun(ctx, orgID, exec.ID, err)
		return nil
	}
	// Union previously-live DETECTED partitions whose ArtifactType has NO committed artifact this run (a
	// vanished type — the artifact was deleted) into the evaluated set, so the deleted artifact's stale
	// finding auto-closes. The committed-artifact list is the authoritative "does this type still exist"
	// (a DB read, not a truncatable tree), so a vanished type is safely auto-closable with no
	// tree-completeness gating. A live-detected partition whose type still has a committed artifact is a
	// scanned partition already handled by evaluated with its normal partial/full logic, so it is excluded.
	evaluated = append(evaluated, vanishedDetectedPartitions(aggregate.committed, aggregate.liveDetectedPartitions)...)

	// PERSONAL LANE (OUTSIDE any tenant transaction; the gate opens its own short suppression scopes
	// per person): the deterministic personal lenses over the owned engineers' own harness, gated per
	// PERSON partition and merged into the same persist set. No fix lane feeds these (advisory only),
	// so they bypass produceFixes and attachGroundedFixes leaves them untouched (no grounded slug).
	personalGated, personalEvaluated, personalHidden, err := e.gatePersonalFindings(ctx, orgID, aggregate,
		personalFindings(orgID, scopeValue, exec.ID, aggregate, e.detectorCfg.aliases))
	if err != nil {
		e.failRun(ctx, orgID, exec.ID, err)
		return nil
	}
	gated = append(gated, personalGated...)
	evaluated = append(evaluated, personalEvaluated...)
	hiddenReasons = append(hiddenReasons, personalHidden...)
	// evaluated is the fully-evaluated partition set the auto-close reconcile consumes (auto-close over
	// N→0 partitions); logged as run telemetry. Log the partition TYPE set (bounded, small) beside the
	// count so the observe-dark signal can distinguish a correct partition set from a wrong one of equal
	// cardinality. grounded_fixes counts the produced fixes whose cited paths all resolved (a run that
	// produced no fix logs 0).
	logger.Info(ctx, "harness-improve: findings gated",
		zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", exec.ID),
		zap.Int("gated", len(gated)), zap.Int("evaluated_partitions", len(evaluated)),
		zap.Int("grounded_fixes", len(groundedFixes)),
		zap.Int("personal_persons", len(aggregate.personal.persons)),
		zap.Int("personal_gated", len(personalGated)),
		zap.Strings("evaluated_partition_types", evaluatedPartitionTypeNames(evaluated)))

	// Attach any grounded fix's suggested body to its finding as an the content store-backed ArtifactContent. This
	// is the content store I/O, so it runs OUTSIDE the write transaction (a tenant tx must not span an the content store round-trip),
	// in the middle between the gate and the persist tail. It attaches an the content store body for each finding
	// present in groundedFixes and is a no-op for an empty map.
	if err := e.attachGroundedFixes(ctx, orgID, gated, groundedFixes); err != nil {
		e.failRun(ctx, orgID, exec.ID, err)
		return nil
	}

	// SHORT WRITE SCOPE: claim the terminal transition FIRST with the atomic compare-and-set so a
	// reaped row (StaleAfter overrun during the middle) does not write over a fresh run, then persist.
	// The two decisions split (mirroring the derive engine's NoneVisible + len(items)): the terminal
	// status keys on whether any finding actually SURFACES (a suppressed finding is flipped to
	// HIDDEN_FAILED_EVAL but kept in gated), while the persist keys on len(gated) — an all-suppressed
	// run persists its HIDDEN rows (they carry the dismissal state and let the auto-close reconcile
	// re-promote the prior visible run) yet stamps COMPLETED_NO_VISIBLE.
	status := taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE
	if visibleFindingCount(gated) > 0 {
		status = taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED
	}
	now := time.Now().UTC()
	// ONE transaction around the compare-and-set + persist (the single-tenant replacement for the
	// tenant-scoped transaction): a persist failure rolls back the terminal stamp, and a reaped row
	// (marked=false → errRunSuperseded) aborts before the persist, so a superseded run never writes.
	if err := e.tx.InTx(ctx, func(sCtx context.Context) error {
		marked, markErr := e.executionDAO.MarkTerminalIfRunning(sCtx, orgID, exec.ID, status,
			taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_UNSPECIFIED, now, "")
		if markErr != nil {
			return fmt.Errorf("stamping terminal status %s: %w", status, markErr)
		}
		if !marked {
			return errRunSuperseded
		}
		// Persist when the run produced ANY finding OR fully evaluated ANY partition: an N→0 run that
		// closed a prior finding has empty gated but a non-empty evaluated set, and its reconcile-only
		// PersistFindings is what auto-closes the stale is_latest. PersistFindings maps the evaluated
		// partitions to the DETECTED-source reconcile set.
		if len(gated) > 0 || len(evaluated) > 0 {
			if persistErr := e.libraryDAO.PersistFindings(sCtx, orgID, exec.ID, gated, toDAOPartitions(evaluated)); persistErr != nil {
				return fmt.Errorf("persisting findings: %w", persistErr)
			}
		}
		return nil
	}); err != nil {
		if errors.Is(err, errRunSuperseded) {
			// The reaper failed this row mid-flight and a fresh run owns the work — ack the redelivery.
			harnessImproveScanRunsTotal.WithLabelValues("reaped").Inc()
			return nil
		}
		e.failRun(ctx, orgID, exec.ID, err)
		return nil
	}
	harnessImproveScanRunsTotal.WithLabelValues("ok").Inc()
	// Success terminal committed: meter this committed run's VISIBLE findings, its gate-hidden findings
	// by reason, and its wall-clock — all attributed only to the run that won the transition.
	meterFindingsEmitted(gated)
	meterGateHidden(hiddenReasons)
	observeScanDuration(start)
	return nil
}

// aggregate does the direct-DAO reads and returns typed data. Every committed read is VCS-filtered
// via ListVCSByRepo; the personal lane's ListPersonalByPersons is the one DESKTOP-filtered,
// person-bounded read.
// monthlyRotationSeed maps a run's start time to a month index (year*12 + month). The detector
// body-fetch cap uses it to PAGE a repo's over-cap body set one chunk per monthly scan, so the tail
// is eventually scored instead of a fixed prefix leaving it permanently unscanned; a retry of the
// SAME run (same StartedAt) re-selects the SAME chunk, keeping the run's partitions idempotent.
func monthlyRotationSeed(t time.Time) int64 {
	return int64(t.Year())*12 + int64(t.Month())
}

// rotateBodyWindow returns the bodyCap-sized slice of a SORTED key list to fetch this run when the
// list exceeds bodyCap. It PAGES one chunk per run (chunk index = seed mod chunk-count), so a repo
// with more than bodyCap distinct bodies is fully covered over ceil(total/bodyCap) runs instead of a
// fixed prefix stranding the tail; the last chunk is short when total is not a multiple of bodyCap.
// seed is the run's month, so consecutive months advance one chunk while a retry of the SAME run
// (same seed) re-selects the SAME chunk — the run's partitions stay idempotent. Returns the whole
// list unchanged when it fits. seed is non-negative (monthlyRotationSeed), so the modulo is in range.
func rotateBodyWindow(sortedKeys []string, bodyCap int, seed int64) []string {
	total := len(sortedKeys)
	if bodyCap <= 0 || total <= bodyCap {
		return sortedKeys
	}
	chunks := (total + bodyCap - 1) / bodyCap
	start := int(seed%int64(chunks)) * bodyCap
	end := start + bodyCap
	if end > total {
		end = total
	}
	return sortedKeys[start:end]
}

// startedAt is the run's StartedAt: it seeds the body-fetch rotation and anchors the trailing window,
// so a retry of the same run reads the same inputs.
func (e *HarnessImproveEngine) aggregate(ctx context.Context, orgID, repoID string, startedAt time.Time) (harnessImproveAggregate, error) {
	var out harnessImproveAggregate
	out.bodies = map[string]string{}
	out.repoPaths = map[string]struct{}{}
	out.behavioralByName = map[string]dao.InvokedSkillBehavior{}
	out.nameCollisionDemoted = map[string]struct{}{}
	// Class-A calendar window: conversation_start_date / security flag_date are UTC calendar DATE
	// columns compared inclusively (>= from AND <= to), so anchor both bounds on a UTC calendar-date
	// midnight — a time-of-day edge would make the window fuzzy across the day boundary. Anchored on
	// the run's own StartedAt (stamped per repo exec at fan-out time, not once per cycle), so a
	// redelivered run recomputes its original window; it is shared across a cycle's repo runs only
	// when the fan-out completes within one UTC day and no scope is retried — see the ownership
	// precondition in personal.go.
	start := startedAt.UTC()
	to := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	from := to.AddDate(0, 0, -e.detectorCfg.windowDays)
	// behavioralRaw and collisionContents resolve inside the tenant scope but are canonicalized into
	// out.behavioralByName / out.nameCollisionDemoted AFTER it returns — the folding is pure Go, so it
	// need not hold the tenant transaction open.
	var behavioralRaw map[string]dao.InvokedSkillBehavior
	var collisionContents []dao.VCSArtifactContent
	// filesByID resolves inside the tenant scope but is read AFTER it returns, so the slow
	// the content store body reads never hold the tenant transaction open (GO-029 split-scope).
	filesByID := map[string]*model.HarnessFile{}
	// treeContent is the paths-only repo tree body read from the repo row. Empty when the repo row is
	// unavailable or carries no tree — the path lenses then see no paths.
	var treeContent string
	// detectable is the subset of committed artifacts a wired detector reads; only their bodies
	// are resolved and fetched, so types no detector consumes cost no the content store round-trip.
	var detectable []*model.HarnessArtifact
	// hasBehavioralCandidate is true when the repo commits ANY SKILL/COMMAND/AGENT — the only
	// artifacts the behavioral lens can flag. When false, the two org-wide behavioral-bridge reads
	// below (the full-window invoked-skill aggregate + the org collision inventory) are pure waste,
	// so they are skipped and behavioralByName/nameCollisionDemoted stay empty (downstream reads
	// empty as insufficient-data → no finding).
	var hasBehavioralCandidate bool
	if err := func(sCtx context.Context) error {
		committed, readErr := e.harnessArtifactDAO.ListVCSByRepo(sCtx, orgID, repoID)
		if readErr != nil {
			return fmt.Errorf("reading committed VCS artifacts for repo %s: %w", repoID, readErr)
		}
		out.committed = committed
		detectable = make([]*model.HarnessArtifact, 0, len(committed))
		fileIDs := make([]string, 0, len(committed))
		for _, artifact := range committed {
			if isBehavioralCandidate(artifact) {
				hasBehavioralCandidate = true
			}
			// A truncated primary body was clipped at the fetch cap, so its stored bytes are
			// incomplete; scoring content metrics on it would mis-measure partial content. Leave
			// it unevaluated (no body → no finding) rather than fetch-and-mis-score it.
			if !bodyRequiredForDetection(artifact) || isPrimaryTruncated(artifact) {
				continue
			}
			detectable = append(detectable, artifact)
			if artifact.CurrentFileID != "" {
				fileIDs = append(fileIDs, artifact.CurrentFileID)
			}
		}
		if len(fileIDs) > 0 {
			files, fileErr := e.fileDAO.ListByIDs(sCtx, orgID, fileIDs)
			if fileErr != nil {
				return fmt.Errorf("reading harness files for repo %s: %w", repoID, fileErr)
			}
			for _, file := range files {
				filesByID[file.ID] = file
			}
		}
		securityByType, secErr := e.securityDAO.CountByFlagType(sCtx, orgID, from, to)
		if secErr != nil {
			return fmt.Errorf("reading security flags for org %s: %w", orgID, secErr)
		}
		out.securityByFlagType = securityByType

		// Behavioral-attribution bridge: org-wide per-invoked-skill behavior over the trailing
		// window, plus the org-wide SKILL/COMMAND/AGENT content set the collision guard folds.
		// Both feed the behavioral lens —
		// but only a repo committing at least one behavioral candidate can emit a behavioral finding,
		// so an instruction-only/permissions-only repo skips both (the full-window JSONB detoast and
		// the org collision inventory are pure waste otherwise).
		if hasBehavioralCandidate {
			behavior, behErr := e.sessionDAO.AggregateInvokedSkillBehavior(sCtx, orgID, from, to)
			if behErr != nil {
				return fmt.Errorf("reading invoked-skill behavior for org %s: %w", orgID, behErr)
			}
			behavioralRaw = behavior
			contents, colErr := e.harnessArtifactDAO.ListVCSArtifactContentByType(sCtx, orgID, behavioralCollisionTypes)
			if colErr != nil {
				return fmt.Errorf("reading VCS artifact content for org %s collision guard: %w", orgID, colErr)
			}
			collisionContents = contents
		}

		// The repo row carries the tree_key. A missing/unreadable repo row DEGRADES the
		// path lenses to no paths rather than failing the whole scan — a repo whose tree was
		// never fetched still runs every content-only Conflict lens that needs no tree.
		repo, repoErr := e.repositoryDAO.GetByID(sCtx, orgID, repoID)
		if repoErr != nil {
			logger.WarnWithCtx(sCtx, "harness-improve: repo row unavailable, path lenses degrade to empty tree",
				zap.String(logger.ORG_ID, orgID), zap.String(logger.REPO_ID, repoID), zap.Error(repoErr))
		} else if repo.TreeContent != nil {
			treeContent = *repo.TreeContent
		}

		// The org's current live DETECTED partitions for this repo — the vanished-type auto-close input.
		// A live-detected partition whose type has no committed artifact this run auto-closes (see Run).
		liveDetected, liveErr := e.libraryDAO.ListLiveDetectedPartitions(sCtx, orgID,
			promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, repoID)
		if liveErr != nil {
			return fmt.Errorf("reading live detected partitions for repo %s: %w", repoID, liveErr)
		}
		out.liveDetectedPartitions = fromDAOPartitions(liveDetected)

		// Personal lane: the engineers this repo run OWNS (their primary repo is this one), their
		// whole personal harness, and their in-window invocations. The artifact read is the one
		// per-person read (DESKTOP + person_id IN ?), the inverse of every VCS read above. It uses the
		// same usage window as the reads above, and the org filter is the DAO's own predicate.
		out.personal.windowEnd = to
		out.personal.windowDays = e.detectorCfg.windowDays
		out.personal.invocations = map[string]int64{}
		persons, personsErr := e.usageDAO.ListPrimaryRepoPersons(sCtx, orgID, repoID, from, to)
		if personsErr != nil {
			return fmt.Errorf("reading primary-repo persons for repo %s: %w", repoID, personsErr)
		}
		out.personal.persons = persons
		if len(persons) > 0 {
			personalArtifacts, personalErr := e.harnessArtifactDAO.ListPersonalByPersons(sCtx, orgID, persons)
			if personalErr != nil {
				return fmt.Errorf("reading personal artifacts for repo %s owners: %w", repoID, personalErr)
			}
			out.personal.artifacts = personalArtifacts
			invocations, usageErr := e.usageDAO.ListInvocationsForPersons(sCtx, orgID, persons, from, to)
			if usageErr != nil {
				return fmt.Errorf("reading personal invocations for repo %s owners: %w", repoID, usageErr)
			}
			out.personal.invocations = foldPersonalInvocations(personalArtifacts, invocations)
			// One org-wide PERSON-scope read (scopeValue "" = every person), narrowed to the owned
			// persons in Go — cheaper than one read per person, and the partition set is small.
			livePersonal, livePersonalErr := e.libraryDAO.ListLiveDetectedPartitions(sCtx, orgID,
				promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PERSON, "")
			if livePersonalErr != nil {
				return fmt.Errorf("reading live personal detected partitions for org %s: %w", orgID, livePersonalErr)
			}
			owned := make(map[string]struct{}, len(persons))
			for _, p := range persons {
				owned[p] = struct{}{}
			}
			for _, partition := range fromDAOPartitions(livePersonal) {
				if _, ok := owned[partition.ScopeValue]; ok {
					out.personal.liveDetectedPartitions = append(out.personal.liveDetectedPartitions, partition)
				}
			}
		}
		return nil
	}(ctx); err != nil {
		return harnessImproveAggregate{}, fmt.Errorf("harness-improve aggregate reads for org %s: %w", orgID, err)
	}

	// Fold the two behavioral-bridge reads into canonical form (pure Go):
	// pool behavior rows by canonical name, and demote every canonical name that maps to ≥2 distinct
	// committed contents (≥2 distinct ContentHash) so the lens never attributes org-wide behavior to a
	// colliding artifact. Order-independent: the demoted set is computed from the full content list.
	for rawName, behavior := range behavioralRaw {
		canonical := resolveCanonicalName(rawName, e.detectorCfg.aliases)
		if existing, ok := out.behavioralByName[canonical]; ok {
			out.behavioralByName[canonical] = mergeInvokedSkillBehavior(existing, behavior)
		} else {
			out.behavioralByName[canonical] = behavior
		}
	}
	out.nameCollisionDemoted = demotedCollisionNames(collisionContents, e.detectorCfg.aliases)
	for range out.nameCollisionDemoted {
		harnessBehavioralNameMatchTotal.WithLabelValues("collision").Inc()
	}

	// Parse the paths-only repo tree body read from the repo row (bodies-as-column; no object store).
	// Every degrade path leaves repoPaths empty — an absent or unparseable tree never fails the scan
	// (the path lenses simply see no committed paths and stay silent).
	if treeContent != "" {
		var parsed repoTreeBlob
		if unmarshalErr := json.Unmarshal([]byte(treeContent), &parsed); unmarshalErr != nil {
			logger.WarnWithCtx(ctx, "harness-improve: skipping unparseable repo tree content",
				zap.String(logger.ORG_ID, orgID), zap.String(logger.REPO_ID, repoID), zap.Error(unmarshalErr))
		} else {
			capped := false
			for _, entry := range parsed.Entries {
				// UNSTRIPPED: keep every committed blob path (no isStrippedPath filter). A dead glob or a
				// broken reference to a vendored/generated file is still a real defect.
				if entry.Type != blobEntryType {
					continue
				}
				if len(out.repoPaths) >= maxRepoPathsEntries {
					capped = true
					break
				}
				out.repoPaths[entry.Path] = struct{}{}
			}
			// A capped ingest is a partial listing, so mark it truncated exactly like a vendor-clipped
			// tree — the non-existence-asserting lenses must not run against it.
			out.treeTruncated = parsed.Truncated || capped
			if capped {
				logger.WarnWithCtx(ctx, "harness-improve: repo tree path set capped, path lenses degrade to truncated",
					zap.String(logger.ORG_ID, orgID), zap.String(logger.REPO_ID, repoID),
					zap.Int("cap", maxRepoPathsEntries))
			}
		}
	}

	// Read each detectable artifact's body from its harness_files.content (bodies-as-column; no object
	// store). A file with no content is skipped (no body → no finding), degrading the run to fewer
	// findings, never failing it.
	for _, artifact := range detectable {
		harnessFile, ok := filesByID[artifact.CurrentFileID]
		if !ok || len(harnessFile.Content) == 0 {
			continue
		}
		out.bodies[artifact.ID] = string(harnessFile.Content)
	}
	return out, nil
}

// bodyRequiredForDetection reports whether an artifact's body must be fetched for ANY wired detector.
// It is the UNION of every detector's own body predicate — Instruction-Intent's instruction set, the
// Conflict lens's wider set (instructions plus SKILL/COMMAND), and Security's PERMISSIONS carrier —
// so a body is fetched once and fed to whichever lenses claim it. A type no wired detector reads
// costs no the content store round-trip.
func bodyRequiredForDetection(a *model.HarnessArtifact) bool {
	return bodyRequiredForInstruction(a) || bodyRequiredForConflict(a) || bodyRequiredForSecurity(a) ||
		bodyRequiredForBehavioral(a)
}

// isPrimaryTruncated reports whether the artifact's primary committed file was clipped at the fetch
// cap — its stored bytes are incomplete, so content metrics computed over them would score partial
// content. Such an artifact is left unevaluated rather than mis-scored.
func isPrimaryTruncated(a *model.HarnessArtifact) bool {
	for _, ref := range a.FileRefs() {
		if ref.GetIsPrimary() {
			return ref.GetTruncated()
		}
	}
	return false
}

// detectAndGrade is the detector-first middle: deterministic detectors over committed artifact
// bodies → findings. It runs OUTSIDE any open tenant transaction, reading the already-assembled
// aggregate (bodies + repo paths + behavioral bridge) and the enrich phase's intent map. It is PURE
// — every input is already fetched, so it issues no I/O of its own (the behavioral lens's LLM leg
// ran in enrichIntents, the deterministic lenses read only aggregate fields). The Instruction-Intent
// (placement + behavioral-deviation), Conflict, and per-repo Security permission-inventory lenses
// are wired here. The ORG-scoped Security detectors (MCP sprawl / coverage-gap) are deliberately NOT
// wired here — they run once per org in the org-relational scan slice (see security.go), so wiring
// them per-repo would duplicate a single org finding across every repo scan.
func (e *HarnessImproveEngine) detectAndGrade(ctx context.Context, orgID, repoID, taskExecutionID string, aggregate harnessImproveAggregate, intentByArtifactID map[string]classifiedIntent) []*model.PromptLibraryItem {
	findings := append(instructionIntentFindings(orgID, repoID, taskExecutionID, aggregate),
		conflictFindings(orgID, repoID, taskExecutionID, aggregate, e.detectorCfg.vendorMarkers, e.detectorCfg.hardMarkers, e.detectorCfg.ruleLedgerBasename)...)
	findings = append(findings, securityPermissionFindings(ctx, orgID, repoID, taskExecutionID, aggregate)...)
	return append(findings, behavioralDeviationFindings(orgID, repoID, taskExecutionID, &aggregate, intentByArtifactID,
		e.detectorCfg.bands, e.detectorCfg.aliases)...)
}

// enrichIntents is the behavioral lens's LLM leg: for each DISTINCT body content among the committed
// skills/commands/subagents with a non-demoted canonical-name behavioral match and a fetched body, it
// classifies the intent once via tetriz-py and applies the result to every candidate sharing that
// content (two artifacts with an identical body get the same classification). It runs OUTSIDE any
// tenant transaction (a tenant tx must not span the LLM calls) over a capped errgroup mirroring the
// body-fetch fan-out. A classification below the confidence floor, an ambiguous class, or a classify
// error DROPS that content (metering the degrade reason) and never fails the run — one bad body
// yields fewer findings, never a wedged scan. Returns the surviving intents keyed by artifact id and
// a degraded flag: true when any candidate's classification degraded (LLM error, ambiguous class,
// below-floor confidence, or a classify-cap truncation), which the findings gate reads as
// behavioralComplete=false so the behavioral partition is treated as only partially evaluated.
func (e *HarnessImproveEngine) enrichIntents(ctx context.Context, orgID string, aggregate *harnessImproveAggregate) (map[string]classifiedIntent, bool) {
	// Collect the scorable artifacts (metering matched/unmatched per artifact), grouped by DISTINCT
	// body content. A generated/duplicated repo can commit many artifacts sharing one body, and the
	// intent is a function of the content, so one classify per distinct content covers every artifact
	// that shares it — no lost coverage, and the duplicate blowup collapses to the distinct-content
	// count (maxDetectorBodiesPerRun caps distinct the content store reads, NOT candidate count, so without this a
	// same-body repo enqueues one LLM call per artifact).
	artifactsByContent := map[string][]*model.HarnessArtifact{}
	repByContent := map[string]*model.HarnessArtifact{}
	var contentKeys []string
	for _, artifact := range aggregate.committed {
		if !isBehavioralCandidate(artifact) {
			continue
		}
		canonical := resolveCanonicalName(artifact.Name, e.detectorCfg.aliases)
		if _, demoted := aggregate.nameCollisionDemoted[canonical]; demoted {
			continue
		}
		if _, matched := aggregate.behavioralByName[canonical]; !matched {
			harnessBehavioralNameMatchTotal.WithLabelValues("unmatched").Inc()
			continue
		}
		harnessBehavioralNameMatchTotal.WithLabelValues("matched").Inc()
		body, ok := aggregate.bodies[artifact.ID]
		if !ok {
			// No body fetched (unresolvable/missing/read error) → nothing to classify.
			continue
		}
		key := behavioralContentKey(body)
		if _, seen := artifactsByContent[key]; !seen {
			repByContent[key] = artifact // first artifact of this content drives the one classify call
			contentKeys = append(contentKeys, key)
		}
		artifactsByContent[key] = append(artifactsByContent[key], artifact)
	}
	intents := map[string]classifiedIntent{}
	if len(contentKeys) == 0 {
		return intents, false
	}
	// Deterministic cap: sort the distinct-content keys so a truncation drops the same tail across
	// runs, then bound the classify calls (mirrors the body-fetch cap logging).
	sort.Strings(contentKeys)
	// A truncation leaves the dropped candidates unclassified, so the behavioral half did not fully
	// run — the same partial-evaluation the gate must not treat as resolved.
	degraded := false
	if len(contentKeys) > e.detectorCfg.maxBehavioralClassify {
		logger.WarnWithCtx(ctx, "harness-improve: capping behavioral intent classifications for this run",
			zap.String(logger.ORG_ID, orgID), zap.Int("cap", e.detectorCfg.maxBehavioralClassify), zap.Int("total", len(contentKeys)))
		contentKeys = contentKeys[:e.detectorCfg.maxBehavioralClassify]
		degraded = true
	}

	var mu sync.Mutex
	grp := errgroup.New()
	grp.SetLimit(detectorFanoutLimit)
	for _, key := range contentKeys {
		key := key
		rep := repByContent[key]
		body := aggregate.bodies[rep.ID]
		grp.Go(func() error {
			result, classifyErr := e.classify.ClassifyHarnessIntent(ctx, orgID, rep.ID, HarnessIntentInput{
				EntityType:    behavioralEntityType(rep.Type),
				StatedPurpose: "", // no stated-purpose field on a committed VCS artifact today
				Content:       body,
			})
			if classifyErr != nil {
				harnessBehavioralDegradeTotal.WithLabelValues("llm_error").Inc()
				logger.WarnWithCtx(ctx, "harness-improve: skipping artifact intent classify failure",
					zap.String(logger.ORG_ID, orgID), zap.String("artifact_id", rep.ID), zap.Error(classifyErr))
				mu.Lock()
				degraded = true
				mu.Unlock()
				return nil
			}
			if result.IntentClass == "ambiguous" {
				harnessBehavioralDegradeTotal.WithLabelValues("ambiguous").Inc()
				mu.Lock()
				degraded = true
				mu.Unlock()
				return nil
			}
			if result.Confidence < e.detectorCfg.floor {
				harnessBehavioralDegradeTotal.WithLabelValues("below_floor").Inc()
				mu.Lock()
				degraded = true
				mu.Unlock()
				return nil
			}
			// Apply the one classification to every candidate sharing this content.
			mu.Lock()
			for _, artifact := range artifactsByContent[key] {
				intents[artifact.ID] = classifiedIntent{class: result.IntentClass, confidence: result.Confidence}
			}
			mu.Unlock()
			return nil
		})
	}
	// Every per-content error is swallowed-with-log inside the func (returns nil), so Wait returns
	// nil; still check + log so a future non-nil (e.g. a recovered panic) is visible, not silent.
	if waitErr := grp.Wait(); waitErr != nil {
		logger.WarnWithCtx(ctx, "harness-improve: intent-classify fan-out returned an error",
			zap.String(logger.ORG_ID, orgID), zap.Error(waitErr))
	}
	return intents, degraded
}

// produceFixes routes each detector finding to its fix lane (harnessFixLane) and returns the produced
// suggested bodies keyed by finding slug — the map gateFindings grounds and attachGroundedFixes
// persists. It runs OUTSIDE any tenant transaction (same position as enrichIntents), reading only the
// already-assembled aggregate. Two lanes produce a body:
//
//   - the DETERMINISTIC lane (a broken-path Conflict finding): PatchBrokenPath resolves the reference
//     in Go against the committed repo path set. It is FREE and uncapped; a finding whose rename is
//     ambiguous (not autoFixable) simply produces no fix and stays advisory.
//   - the LLM lane (the content-fix families — the Instruction-Intent rewrites token-floor /
//     tok-per-line / directive-share, behavioral-deviation, and the Conflict remove-line families
//     dead-glob / dup-id / vendor-surface / root-restatement): e.fix.GenerateHarnessFix drafts a
//     corrected body via tetriz-py. It is CAPPED (detectorCfg.maxFixGen) over a slug-sorted order so
//     a truncation drops the same tail across runs, and fanned out over a limited errgroup mirroring
//     enrichIntents. A generate error is logged-and-dropped (the finding stays advisory), never a run
//     failure — one bad draft yields fewer fixes, never a wedged scan.
//
// Every SECURITY family is advisory here and produces no fix: a permission finding's anchor is the whole
// settings.json body (secret-density), deliberately not egressed to the LLM (see harnessFixLane), and the
// ORG coverage-gap/mcp fixes are generated from scratch in produceOrgFixes, not this per-repo lane.
func (e *HarnessImproveEngine) produceFixes(ctx context.Context, orgID string, aggregate harnessImproveAggregate, findings []*model.PromptLibraryItem) map[string]HarnessFixResult {
	fixes := map[string]HarnessFixResult{}
	artifactByID := make(map[string]*model.HarnessArtifact, len(aggregate.committed))
	for _, artifact := range aggregate.committed {
		artifactByID[artifact.ID] = artifact
	}
	repoPathsSlice := sortedPathKeys(aggregate.repoPaths)

	// Deterministic pass (FREE, uncapped): resolve every broken-path rename in Go, and collect the LLM
	// lane's findings for the capped pass below.
	var llmFindings []*model.PromptLibraryItem
	for _, finding := range findings {
		switch harnessFixLane(finding) {
		case laneDeterministic:
			body, hasBody := fixBody(finding, aggregate)
			if !hasBody {
				harnessFixProduceTotal.WithLabelValues(fixOutcomeAdvisory).Inc()
				continue
			}
			patched, resolvedPath, autoFixable := PatchBrokenPath(finding.Evidence.GetReferencePoint(), body, aggregate.repoPaths)
			if !autoFixable {
				// An ambiguous rename (zero or two-plus candidates) is left advisory, never guessed.
				harnessFixProduceTotal.WithLabelValues(fixOutcomeAdvisory).Inc()
				continue
			}
			fixes[finding.Slug] = HarnessFixResult{
				SuggestedContent: patched,
				ChangeSummary:    fmt.Sprintf("Rewrite broken path reference %q to %q", finding.Evidence.GetReferencePoint(), resolvedPath),
				CitedPaths:       []string{resolvedPath},
				BaselineContent:  body,
			}
			harnessFixProduceTotal.WithLabelValues(fixOutcomeDeterministic).Inc()
		case laneLLM:
			if _, hasBody := fixBody(finding, aggregate); !hasBody {
				harnessFixProduceTotal.WithLabelValues(fixOutcomeAdvisory).Inc()
				continue
			}
			llmFindings = append(llmFindings, finding)
		default:
			harnessFixProduceTotal.WithLabelValues(fixOutcomeAdvisory).Inc()
		}
	}
	// LLM pass: draft each remove-line / restructure body via tetriz-py, capped + fanned by the shared
	// runCappedFixGeneration skeleton (same cap/fan-out/degrade discipline as the org path). The per-finding
	// closure runs the generate, then validates the BODY's path refs — not only its self-reported
	// cited_paths (the schema permits an empty cited list): an LLM- or prompt-injection-induced body can
	// carry a NEW or RETAINED path that grounds nowhere, so resolve every path-like ref in SuggestedContent
	// against the artifact's own directory (the same broken-path resolver the detector uses) and drop an
	// ungrounded fix to advisory rather than write a bogus path to the content store with AutoFixable=true. The gate's
	// cited-path grounding stays the second, independent check.
	//
	// This body check inherits brokenPathRefs' precision-over-recall bias BY DESIGN, so it is a
	// high-signal — not exhaustive — grounding gate: a fix body citing a bare name, a template
	// placeholder, or a partial path that segment-aligns to a real file is treated as grounded (a fix
	// body is judged the same way a committed artifact is), and only a full or `./`/`../` path that
	// resolves NOWHERE is caught. Non-existence is unprovable on a clipped/empty tree, so the check is
	// skipped there (fix treated as grounded), mirroring the detector's own truncated-tree guard.
	fixTree := newCommittedTree(aggregate.repoPaths) // built once; reused by every finding's body check
	fixTreeProvable := !aggregate.treeTruncated && len(aggregate.repoPaths) > 0
	llmFixes := e.runCappedFixGeneration(ctx, orgID, "harness-improve: capping LLM fix generations for this run",
		llmFindings, func(finding *model.PromptLibraryItem) (HarnessFixResult, bool) {
			artifact := artifactByID[*finding.HarnessArtifactID]
			body := aggregate.bodies[*finding.HarnessArtifactID]
			result, fixErr := e.fix.GenerateHarnessFix(ctx, orgID, *finding.HarnessArtifactID, HarnessFixInput{
				Dimension:      harnessFixDimensionLabel(finding),
				Title:          finding.Title,
				ReferencePoint: finding.Evidence.GetReferencePoint(),
				CurrentContent: body,
				RepoPaths:      repoPathsSlice,
				EntityType:     artifactTypeLabel(finding.ArtifactType),
				ArtifactName:   artifactName(artifact),
			})
			if fixErr != nil {
				// A degraded generate leaves the finding advisory — log, meter, drop, never fail the run.
				harnessFixProduceTotal.WithLabelValues(fixOutcomeLLMError).Inc()
				logger.WarnWithCtx(ctx, "harness-improve: skipping fix generation failure",
					zap.String(logger.ORG_ID, orgID), zap.String("artifact_id", *finding.HarnessArtifactID),
					zap.String("slug", finding.Slug), zap.Error(fixErr))
				return HarnessFixResult{}, false
			}
			sourcePath := ""
			if artifact != nil {
				sourcePath = artifact.SourcePath
			}
			// Skip the body scan entirely on a clipped/empty tree (non-existence unprovable): the fix is
			// treated as grounded, mirroring the detector's own truncated-tree guard.
			if fixTreeProvable {
				if bogus := brokenPathRefs(result.SuggestedContent, fixTree, sourcePath); len(bogus) > 0 {
					harnessFixProduceTotal.WithLabelValues(fixOutcomeAdvisory).Inc()
					logger.WarnWithCtx(ctx, "harness-improve: dropping LLM fix with ungrounded path(s) in its body, finding stays advisory",
						zap.String(logger.ORG_ID, orgID), zap.String("artifact_id", *finding.HarnessArtifactID),
						zap.String("slug", finding.Slug), zap.Strings("ungrounded_paths", bogus))
					return HarnessFixResult{}, false
				}
			}
			// A no-op fix (the model returned the committed body unchanged) is not applicable: enforcing it
			// is an empty diff that would mark the still-unresolved finding FIXED, and next month's scan
			// re-detects the same problem. Drop it to advisory. Compared trimmed so trailing-whitespace
			// noise never reads as a real change.
			if strings.TrimSpace(result.SuggestedContent) == strings.TrimSpace(body) {
				harnessFixProduceTotal.WithLabelValues(fixOutcomeAdvisory).Inc()
				logger.WarnWithCtx(ctx, "harness-improve: dropping no-op LLM fix (unchanged body), finding stays advisory",
					zap.String(logger.ORG_ID, orgID), zap.String("artifact_id", *finding.HarnessArtifactID),
					zap.String("slug", finding.Slug))
				return HarnessFixResult{}, false
			}
			// Record the committed body the fix was computed from as the diff's "before".
			result.BaselineContent = body
			return result, true
		})
	// Merge the LLM fixes over the deterministic ones already in fixes (disjoint slugs — a finding routes
	// to exactly one lane).
	for slug, llmFix := range llmFixes {
		fixes[slug] = llmFix
	}
	return fixes
}

// runCappedFixGeneration is the shared cap + fan-out + store skeleton for both fix producers — the
// per-repo LLM lane (produceFixes) and the org generate lane (produceOrgFixes) — so their cap
// semantics, fan-out width, and success metering can never drift. It sorts findings by slug (a cap
// truncation then drops the same tail across runs), caps at detectorCfg.maxFixGen (metering each
// dropped finding fixOutcomeCapped and warning once with capLogMsg), and fans `produce` out over a
// detectorFanoutLimit-wide errgroup. `produce` runs the slow per-finding generate and returns the fix
// plus whether to keep it — it owns its OWN degrade metering (an LLM error, an ungrounded body → return
// ok=false, finding stays advisory); a kept fix is stored by slug and metered fixOutcomeLLM here. It
// never fails the run: a per-finding error is the producer's to swallow-and-log, so Wait is only
// checked to surface a future non-nil (e.g. a recovered panic).
func (e *HarnessImproveEngine) runCappedFixGeneration(ctx context.Context, orgID, capLogMsg string,
	findings []*model.PromptLibraryItem, produce func(*model.PromptLibraryItem) (HarnessFixResult, bool),
) map[string]HarnessFixResult {
	fixes := map[string]HarnessFixResult{}
	if len(findings) == 0 {
		return fixes
	}
	ordered := append([]*model.PromptLibraryItem(nil), findings...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Slug < ordered[j].Slug })
	if len(ordered) > e.detectorCfg.maxFixGen {
		for range ordered[e.detectorCfg.maxFixGen:] {
			harnessFixProduceTotal.WithLabelValues(fixOutcomeCapped).Inc()
		}
		logger.WarnWithCtx(ctx, capLogMsg,
			zap.String(logger.ORG_ID, orgID), zap.Int("cap", e.detectorCfg.maxFixGen), zap.Int("total", len(ordered)))
		ordered = ordered[:e.detectorCfg.maxFixGen]
	}

	var mu sync.Mutex
	grp := errgroup.New()
	grp.SetLimit(detectorFanoutLimit)
	for _, finding := range ordered {
		finding := finding
		grp.Go(func() error {
			result, keep := produce(finding)
			if !keep {
				return nil
			}
			mu.Lock()
			fixes[finding.Slug] = result
			harnessFixProduceTotal.WithLabelValues(fixOutcomeLLM).Inc()
			mu.Unlock()
			return nil
		})
	}
	if waitErr := grp.Wait(); waitErr != nil {
		logger.WarnWithCtx(ctx, "harness-improve: fix-generate fan-out returned an error",
			zap.String(logger.ORG_ID, orgID), zap.Error(waitErr))
	}
	return fixes
}

// produceOrgFixes generates a suggested fix for each ORG-scoped finding (coverage-gap, mcp) — the
// families the per-repo produceFixes path never sees (they carry no HarnessArtifactID). Every org
// finding is a GENERATE-from-scratch case: the artifact does not exist yet (a missing UserPromptSubmit
// guard hook) or has no committed body the org aggregate carries (an mcp server config), so
// CurrentContent is EMPTY and the LLM synthesizes the recommended body. Each result is marked Generated
// so attachGroundedFixes keeps the diff's "before" empty (an all-added "new content" render). Like
// produceFixes it is CAPPED + fanned + per-item-degrading (an LLM error leaves the finding advisory) and
// runs OUTSIDE any tenant transaction. It deliberately does NOT run produceFixes' body-path validation:
// a generated artifact legitimately references paths that do not exist in the repo yet, and the org run
// has no repo tree to ground against — the gate's cited-path grounding (vacuous for the empty cited set a
// generate returns) is the sufficient check.
func (e *HarnessImproveEngine) produceOrgFixes(ctx context.Context, orgID string, findings []*model.PromptLibraryItem) map[string]HarnessFixResult {
	return e.runCappedFixGeneration(ctx, orgID, "harness-improve: capping org LLM fix generations for this run",
		findings, func(finding *model.PromptLibraryItem) (HarnessFixResult, bool) {
			// CurrentContent is EMPTY — the generate signal. sourceID is the finding slug (an org finding
			// carries no HarnessArtifactID). RepoPaths is nil: an org artifact grounds against no repo tree.
			result, fixErr := e.fix.GenerateHarnessFix(ctx, orgID, finding.Slug, HarnessFixInput{
				Dimension:      harnessFixDimensionLabel(finding),
				Title:          finding.Title,
				ReferencePoint: finding.Evidence.GetReferencePoint(),
				CurrentContent: "",
				RepoPaths:      nil,
				EntityType:     artifactTypeLabel(finding.ArtifactType),
				ArtifactName:   "",
			})
			if fixErr != nil {
				// A degraded generate leaves the finding advisory — log, meter, drop, never fail the run.
				harnessFixProduceTotal.WithLabelValues(fixOutcomeLLMError).Inc()
				logger.WarnWithCtx(ctx, "harness-improve: skipping org fix generation failure",
					zap.String(logger.ORG_ID, orgID), zap.String("slug", finding.Slug), zap.Error(fixErr))
				return HarnessFixResult{}, false
			}
			result.Generated = true
			result.BaselineContent = "" // no committed body — the diff is all-added new content
			return result, true
		})
}

// fixBody returns a finding's anchor artifact's fetched committed body: the input every fix lane
// rewrites. A finding with no HarnessArtifactID (an ORG-relational finding), whose body was not
// fetched this run, or whose committed body is empty has nothing to rewrite, so no fix is produced.
// The empty-body case is load-bearing for the LLM lane: an empty CurrentContent is tetriz-py's
// GENERATE-from-scratch signal, so passing an empty per-repo body through would fabricate a new body
// instead of rewriting — treat it as no-body and leave the finding advisory (generate-from-scratch is
// the ORG path's job, via produceOrgFixes, never the per-repo rewrite path).
func fixBody(finding *model.PromptLibraryItem, aggregate harnessImproveAggregate) (string, bool) {
	if finding.HarnessArtifactID == nil {
		return "", false
	}
	body, ok := aggregate.bodies[*finding.HarnessArtifactID]
	if !ok || body == "" {
		return "", false
	}
	return body, true
}

// artifactName returns a nil-safe artifact name for the fix trace context (empty when the anchor
// artifact is somehow absent from the committed set).
func artifactName(artifact *model.HarnessArtifact) string {
	if artifact == nil {
		return ""
	}
	return artifact.Name
}

// attachGroundedFixes attaches an the content store content/evidence body for each finding whose slug is in
// groundedFixes; a finding absent from the map stays advisory (nil ArtifactContent). It writes the
// fix's suggested body to the content store as the finding's ArtifactContent (and the fix's one-line change summary
// to the finding's evidence blob). It runs OUTSIDE any tenant transaction — the content store write must not span
// the write tx — in the middle between the gate and the persist tail. The DB id is minted here FIRST
// (not left for the insert to generate): PutArtifactContent derives its object key from the id, so an
// empty id would collide every fix on one key — pre-minting gives each body a distinct key and the row
// inserts with that id. A fix whose body cannot be stored DEGRADES to advisory (metered
// fix_produce_total{attach_error}, logged) and the run continues — one un-storable body must never
// drop the whole scan's findings, mirroring the per-item degrade of the body-read and LLM legs.
func (e *HarnessImproveEngine) attachGroundedFixes(ctx context.Context, orgID string, findings []*model.PromptLibraryItem, groundedFixes map[string]HarnessFixResult) error {
	if len(groundedFixes) == 0 {
		return nil
	}
	for _, finding := range findings {
		fix, hasFix := groundedFixes[finding.Slug]
		if !hasFix {
			continue
		}
		if finding.ID == "" {
			finding.ID = uuid.NewString()
		}
		// Original = the committed body the fix was computed FROM (the diff's "before"); Current = the
		// suggested replacement (the "after", and the body Apply-fix enforces). Storing the scan-time
		// baseline here — rather than fetching it live at serve — keeps the pair coherent: the diff shows
		// the suggestion against exactly the body it replaces, not a since-drifted live baseline. A fix
		// with no fetched baseline falls back to Original = suggestion (an empty diff, never a nil body).
		// CitedPaths are the grounded repo paths. EXCEPTION: a Generated fix (a created-from-scratch org
		// artifact — the coverage-gap hook, an mcp config suggestion) has a legitimately-empty baseline, so
		// it keeps Original empty to render all-added "new content", NOT the fetch-failed empty-diff fallback.
		baseline := fix.BaselineContent
		if baseline == "" && !fix.Generated {
			baseline = fix.SuggestedContent
		}
		content := &promptlibraryv1.ArtifactContent{
			Original:   baseline,
			Current:    fix.SuggestedContent,
			CitedPaths: fix.CitedPaths,
		}
		contentKey, err := e.store.PutArtifactContent(ctx, orgID, finding.ID, content)
		if err != nil {
			// Degrade this ONE fix to advisory and continue — an un-storable suggested body must not
			// fail the whole scan and drop every other finding (including the advisory ones that need
			// no fix), mirroring the per-item degrade of the body-read and LLM legs. The finding stays
			// VISIBLE with no ArtifactContent; the transient the content store blip is retried by next month's scan.
			harnessFixProduceTotal.WithLabelValues(fixOutcomeAttachError).Inc()
			logger.WarnWithCtx(ctx, "harness-improve: dropping grounded fix, could not store content",
				zap.String(logger.ORG_ID, orgID), zap.String("slug", finding.Slug), zap.Error(err))
			continue
		}
		finding.ArtifactContent = &promptlibraryv1.ContentRef{Key: contentKey}
		// A grounded suggested body just landed, so the finding is auto-fixable — the serving surface's
		// one-click-apply affordance keys on this projection flag. Guarded like the evidence projection
		// below (a detector may leave Evidence nil until a fix attaches).
		if finding.Evidence == nil {
			finding.Evidence = &promptlibraryv1.EvidenceProjection{}
		}
		finding.Evidence.AutoFixable = true
		// A grounded fix landed, so this finding is no longer advisory — the serving surface offers
		// Apply-fix (a diff + enforce) instead of Dismiss-only. The builders default advisory=true.
		finding.Evidence.Advisory = false
		// ArtifactContent has no change_summary field, so route the fix's one-line summary to the full
		// ArtifactEvidence blob's Summary and point the finding's evidence projection at it. Preserves
		// any reference_point a detector already stamped on the projection. An evidence-store failure
		// keeps the (already-stored) fix body and only drops the summary — the body is the load-bearing
		// part — so it too degrades rather than failing the run.
		if fix.ChangeSummary != "" {
			evidence := &promptlibraryv1.ArtifactEvidence{Summary: fix.ChangeSummary}
			evidenceKey, evErr := e.store.PutArtifactEvidence(ctx, orgID, finding.ID, evidence)
			if evErr != nil {
				harnessFixProduceTotal.WithLabelValues(fixOutcomeAttachError).Inc()
				logger.WarnWithCtx(ctx, "harness-improve: kept grounded fix body but could not store its change summary",
					zap.String(logger.ORG_ID, orgID), zap.String("slug", finding.Slug), zap.Error(evErr))
				continue
			}
			finding.Evidence.Ref = &promptlibraryv1.ContentRef{Key: evidenceKey}
		}
	}
	return nil
}

// toDAOPartitions maps the gate's internal evaluated-partition set to the dao's EvaluatedPartition
// input for PersistFindings' auto-close reconcile — the dao package cannot import this internal type.
func toDAOPartitions(evaluated []evaluatedPartition) []dao.EvaluatedPartition {
	partitions := make([]dao.EvaluatedPartition, 0, len(evaluated))
	for _, partition := range evaluated {
		partitions = append(partitions, dao.EvaluatedPartition{
			Scope:        partition.Scope,
			ScopeValue:   partition.ScopeValue,
			ArtifactType: partition.ArtifactType,
		})
	}
	return partitions
}

// fromDAOPartitions maps the dao's live-detected partitions to the gate's internal evaluatedPartition
// type so they compose with the run's own evaluated set (the reverse of toDAOPartitions).
func fromDAOPartitions(partitions []dao.EvaluatedPartition) []evaluatedPartition {
	converted := make([]evaluatedPartition, 0, len(partitions))
	for _, partition := range partitions {
		converted = append(converted, evaluatedPartition{
			Scope:        partition.Scope,
			ScopeValue:   partition.ScopeValue,
			ArtifactType: partition.ArtifactType,
		})
	}
	return converted
}

// vanishedDetectedPartitions returns the subset of liveDetected partitions whose ArtifactType has NO
// committed artifact this run — a type whose artifact was deleted, so its stale DETECTED finding must
// auto-close. A live-detected partition whose type still has a committed artifact is left to the run's
// normal scanned-partition logic (partial/full), never force-added here — so a partition whose artifact
// merely had a body-fetch failure this run is NOT treated as vanished.
func vanishedDetectedPartitions(committed []*model.HarnessArtifact, liveDetected []evaluatedPartition) []evaluatedPartition {
	// The set of prompt_library artifact types the repo's committed harness maps to this run — the
	// authoritative "does this type still exist" the vanished check keys on. It spans ALL committed
	// artifacts (not only body-fetched ones), so a type whose artifact exists but whose body was
	// unavailable this run still counts as present and is never mistaken for vanished.
	present := make(map[promptlibraryv1.ArtifactType]struct{}, len(committed))
	for _, artifact := range committed {
		artifactType := instructionArtifactType(artifact.Type)
		if artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_UNSPECIFIED {
			continue
		}
		present[artifactType] = struct{}{}
	}
	vanished := make([]evaluatedPartition, 0, len(liveDetected))
	for _, partition := range liveDetected {
		if _, exists := present[partition.ArtifactType]; exists {
			continue
		}
		vanished = append(vanished, partition)
	}
	return vanished
}

// failRun marks the run FAILED with the cause and meters it. A thin wrapper keyed to the
// harness-improve counter (not Engine.fail, whose metrics are the derive lane's), so the two engines
// stay isolated. Mirrors Engine.fail's contract: on a canceled/deadline-exceeded ctx it does NOT
// write — the runner redelivers this task against the still-RUNNING row (a shutdown re-queue), and a
// FAILED write would race that redelivery and could win the compare-and-set and supersede it, so it
// leaves the row RUNNING for the redelivery to own; a genuinely abandoned row is backstopped by the
// reaper's StaleAfter sweep. The counter is incremented only for a FAILED transition THIS run
// committed (marked); a reaped row is the reaper's outcome.
func (e *HarnessImproveEngine) failRun(ctx context.Context, orgID, execID string, cause error) {
	logger.Error(ctx, "harness-improve: scan run failed",
		zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", execID), zap.Error(cause))
	if ctxErr := ctx.Err(); ctxErr != nil {
		logger.Info(ctx, "harness-improve: skipping FAILED write, handler ctx canceled (redelivery re-owns the still-RUNNING run; reaper backstops)",
			zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", execID), zap.Error(ctxErr))
		return
	}
	var marked bool
	if err := e.tx.InTx(ctx, func(sCtx context.Context) error {
		var markErr error
		marked, markErr = e.executionDAO.MarkTerminalIfRunning(sCtx, orgID, execID,
			taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_FAILED,
			taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_UNSPECIFIED, time.Now().UTC(), cause.Error())
		if markErr != nil {
			return fmt.Errorf("marking run failed: %w", markErr)
		}
		if !marked {
			// The reaper already failed this row mid-flight — don't clobber its outcome.
			logger.Info(sCtx, "harness-improve: FAILED write skipped, run no longer RUNNING (reaped)",
				zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", execID))
		}
		return nil
	}); err != nil {
		logger.Error(ctx, "harness-improve: marking run FAILED also failed",
			zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", execID), zap.Error(err))
		return
	}
	if marked {
		harnessImproveScanRunsTotal.WithLabelValues("failed").Inc()
	}
}

// skipRun marks the run SKIPPED with the reason and meters it. Same ctx-cancellation contract as
// failRun: on a canceled ctx it does NOT write (a SKIPPED write would race the redelivery,
// which re-gates and skips again idempotently) — it leaves the row RUNNING for the redelivery to
// own; the reaper's StaleAfter sweep backstops a genuinely abandoned row. The gate checks run before
// any long work, so a canceled ctx here is a pod-shutdown edge, not the scan timeout. The counter is
// incremented only for a SKIPPED transition THIS run committed (marked).
func (e *HarnessImproveEngine) skipRun(ctx context.Context, orgID, execID, reason string) {
	logger.Info(ctx, "harness-improve: scan run skipped",
		zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", execID), zap.String("reason", reason))
	if ctxErr := ctx.Err(); ctxErr != nil {
		logger.Info(ctx, "harness-improve: skipping SKIPPED write, handler ctx canceled (redelivery re-gates the still-RUNNING run; reaper backstops)",
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
		logger.Error(ctx, "harness-improve: marking run SKIPPED also failed",
			zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", execID), zap.Error(err))
		return
	}
	if marked {
		harnessImproveScanRunsTotal.WithLabelValues("skipped").Inc()
	}
}
