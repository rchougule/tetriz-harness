// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/logger"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// userPromptSubmitEvent is the hook event that constitutes a repo's prompt-time guard: a repo is
// coverage-gap-covered iff it commits a HOOK whose event segment is exactly this. The desktop/VCS
// ingest encodes a hook's Name as "<event>" or "<event>:<matcher>" (see harness.ParseSettingsArtifacts),
// so the event is the Name up to the first ':'.
const userPromptSubmitEvent = "UserPromptSubmit"

// orgDetectors pairs each ORG-scoped detector with the artifact_type partition its findings occupy,
// so the emitted findings and the evaluated-partition set (which auto-closes a resolved partition) can
// never drift as detectors are added: adding a detector here adds both its findings AND its partition.
//
// fullyEvaluated decides whether the detector's partition is auto-close-eligible this run — i.e.
// whether the run examined that partition's WHOLE surface, so PersistFindings may reconcile a prior
// standing finding to closed. A partition that is NOT fully evaluated is left OUT of the eval set, so
// a prior finding on it survives untouched. Deriving this per detector (not "always") is what keeps a
// dormant coverage-gap from silently auto-closing (see the coverage-gap predicate below).
var orgDetectors = []struct {
	run            func(OrgSecurityAggregate, string) []*model.PromptLibraryItem
	artifactType   promptlibraryv1.ArtifactType
	fullyEvaluated func(agg OrgSecurityAggregate, emitted []*model.PromptLibraryItem) bool
}{
	{
		run:          OrgMcpFindings,
		artifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_MCP_SERVER,
		// MCP is pure telemetry: the distinct server-name set read each run IS the whole surface, so the
		// partition always concludes and a resolved prior sprawl always auto-closes.
		fullyEvaluated: func(_ OrgSecurityAggregate, _ []*model.PromptLibraryItem) bool { return true },
	},
	{
		run:          OrgCoverageGapFindings,
		artifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK,
		// The coverage-gap partition is auto-close-eligible ONLY when the gap is RESOLVED (every eligible
		// repo covered, or none eligible) or a coverage-gap finding was emitted this run (to promote it).
		// It must be EXCLUDED in the DORMANT case — uncovered repos still exist but no security flag
		// surfaced this run, so the lens stays silent (no threat to surface). Closing the partition then
		// would auto-close the standing coverage-gap and HIDE repos that still lack the UserPromptSubmit
		// hook. The gap's lifecycle is tied to HOOK coverage, not to flag presence: it closes only when
		// every eligible repo has the hook.
		fullyEvaluated: func(agg OrgSecurityAggregate, emitted []*model.PromptLibraryItem) bool {
			return len(emitted) > 0 || allReposCovered(agg.HookCoverageByRepo)
		},
	},
}

// allReposCovered reports whether every eligible repo in the coverage map has its UserPromptSubmit
// hook. An EMPTY map is vacuously true — no eligible repos means no gap to keep open, so a stale
// coverage-gap correctly auto-closes.
func allReposCovered(coverage map[string]bool) bool {
	for _, covered := range coverage {
		if !covered {
			return false
		}
	}
	return true
}

// RunOrgScan runs the ONCE-per-org relational harness scan for a task_executions run and persists its
// ORG-scoped findings dark. It is the sibling of Run for the ORG grain: it reuses the same run
// lifecycle (RUNNING-guard, fail-closed enrich, split read/write scopes, MarkTerminalIfRunning +
// PersistFindings) but has no per-repo tree/body/LLM middle — it folds a handful of org-wide reads
// into the OrgSecurityAggregate and runs the two PURE ORG detectors (OrgMcpFindings,
// OrgCoverageGapFindings) that the per-repo detect/grade path deliberately leaves UNWIRED (wiring
// them per-repo would duplicate one org finding across every repo scan — see security.go / detectAndGrade).
//
// The run always evaluates {ORG, "", MCP_SERVER} (MCP telemetry is a complete read, so a resolved
// prior sprawl auto-closes). The {ORG, "", HOOK} coverage-gap partition is auto-close-eligible only
// when the gap is RESOLVED (every eligible repo covered) or a coverage-gap finding was emitted; a
// DORMANT gap (uncovered repos but no security flag this run) leaves the HOOK partition OUT so the
// standing gap survives — see the orgDetectors fullyEvaluated predicates.
func (e *HarnessImproveEngine) RunOrgScan(ctx context.Context, orgID, trigger, taskExecutionID string) error {
	// trigger carries the raw trigger-type string a scheduler handler's payload delivers, so the handler
	// can thread it through unchanged; the ORG scan itself branches on no trigger, so it is unused here.
	_ = trigger

	// Wall-clock start for scan_duration_seconds, observed only on the success terminal below.
	start := time.Now()
	exec, err := e.executionDAO.GetByID(ctx, orgID, taskExecutionID)
	if err != nil {
		// A load failure returns the error so the caller retries; no row to mark FAILED yet.
		return fmt.Errorf("loading harness-improve org run %s for org %s: %w", taskExecutionID, orgID, err)
	}
	if exec.Status != taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_RUNNING {
		// A redelivery of an already-terminal row: ack as a no-op (the reaper or a prior run owns it).
		logger.Info(ctx, "harness-improve: org run not RUNNING, skipping redelivery",
			zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", taskExecutionID),
			zap.String("status", exec.Status.String()))
		return nil
	}

	// Re-check the rollout flag at dequeue (mirrors Run): a queued org-scan task must not repopulate
	// DETECTED rows after the detectors feature flag was flipped off (a rollback + scrub). Mark
	// SKIPPED, before any reads, and ack.
	if e.flagEnabled != nil && !e.flagEnabled(orgID) {
		e.skipRun(ctx, orgID, exec.ID, "the detectors feature flag disabled at scan time")
		return nil
	}

	// Single-tenant: the derivation window comes from config, and every record in it is read.

	// The org-wide relational reads (MCP telemetry, open security flags, per-repo hook coverage) folded
	// into the ORG detector contract.
	aggregate, err := e.orgAggregate(ctx, orgID)
	if err != nil {
		e.failRun(ctx, orgID, exec.ID, err)
		return nil
	}

	// PURE ORG detectors (OUTSIDE any tenant transaction — no I/O of their own). These fold over the
	// aggregate once per org; the per-repo detect/grade path excludes them on purpose. Deriving BOTH the
	// findings and the scanned-partition set from the single orgDetectors registry keeps them in lockstep
	// — a detector's findings and the partition that auto-closes them can never drift apart.
	var findings []*model.PromptLibraryItem
	scannedPartitions := make([]scannedPartition, 0, len(orgDetectors))
	for _, detector := range orgDetectors {
		emitted := detector.run(aggregate, exec.ID)
		findings = append(findings, emitted...)
		// A partition joins the auto-close eval set only when its detector CONCLUDED its whole surface
		// (fullyEvaluated). A dormant coverage-gap run leaves the HOOK partition OUT, so PersistFindings
		// does not reconcile it and the prior standing gap's is_latest survives. An org read has no
		// tree/body/behavioral degradation, so an evaluated partition is always body-complete.
		if detector.fullyEvaluated(aggregate, emitted) {
			scannedPartitions = append(scannedPartitions, scannedPartition{
				partition:    orgEvaluatedPartition(detector.artifactType),
				bodyComplete: true,
			})
		}
	}

	// Build the eval-status so gateFindings.evaluatedPartitions() yields exactly the registry's
	// partitions — while the gate's suppression + zero-priority assertion still run over the findings.
	orgEvalStatus := evaluationStatus{
		pathLensComplete:   true,
		behavioralComplete: true,
		scannedPartitions:  scannedPartitions,
	}
	// ORG fix generation (OUTSIDE any tenant transaction — LLM + the content store never span the write tx): synthesize a
	// suggested body for each org finding — a coverage-gap guard hook, an mcp config suggestion — all
	// generate-from-scratch (empty CurrentContent). A per-finding LLM failure leaves that finding advisory.
	orgFixes := e.produceOrgFixes(ctx, orgID, findings)

	// scopeValue is EMPTY for an ORG finding (the model's org-scope convention), NOT exec.DedupKey —
	// an ORG run's DedupKey holds the orgID and exists only for single-in-flight dedup, never as a
	// scope value. repoPaths is empty: an org artifact grounds against no repo tree, so the gate's
	// cited-path grounding is vacuous for the empty cited set a generate returns (a fix whose model
	// nonetheless cited a path is correctly withheld, the finding staying advisory).
	gated, evaluated, groundedFixes, hiddenReasons, err := e.gateFindings(ctx, orgID, "", /*scopeValue*/
		promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_ORG, findings,
		orgFixes, map[string]struct{}{}, orgEvalStatus)
	if err != nil {
		e.failRun(ctx, orgID, exec.ID, err)
		return nil
	}
	logger.Info(ctx, "harness-improve: org findings gated",
		zap.String(logger.ORG_ID, orgID), zap.String("task_execution_id", exec.ID),
		zap.Int("gated", len(gated)), zap.Int("evaluated_partitions", len(evaluated)),
		zap.Int("grounded_fixes", len(groundedFixes)),
		zap.Strings("evaluated_partition_types", evaluatedPartitionTypeNames(evaluated)))

	// Attach each grounded fix's suggested body to its finding (the content store write, OUTSIDE the write tx below). A
	// finding with no grounded fix stays advisory; a no-op for an empty map (a run that produced no fixes).
	if err := e.attachGroundedFixes(ctx, orgID, gated, groundedFixes); err != nil {
		e.failRun(ctx, orgID, exec.ID, err)
		return nil
	}

	// SHORT WRITE SCOPE: claim the terminal transition FIRST with the atomic compare-and-set (a reaped
	// row must not be written over), then persist. The terminal status keys on whether any finding
	// SURFACES (visibleFindingCount), while the persist keys on len(gated) — an all-suppressed run
	// persists its HIDDEN rows yet stamps COMPLETED_NO_VISIBLE.
	status := taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE
	if visibleFindingCount(gated) > 0 {
		status = taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED
	}
	now := time.Now().UTC()
	// ONE transaction around the compare-and-set + persist, so a persist failure rolls back the
	// terminal stamp and no half-written is_latest partition can commit (the single-tenant replacement
	// for the tenant-scoped transaction; the atomicity + errRunSuperseded guarantee is preserved).
	if err := e.tx.InTx(ctx, func(sCtx context.Context) error {
		marked, markErr := e.executionDAO.MarkTerminalIfRunning(sCtx, orgID, exec.ID, status,
			taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_UNSPECIFIED, now, "")
		if markErr != nil {
			return fmt.Errorf("stamping terminal status %s: %w", status, markErr)
		}
		if !marked {
			return errRunSuperseded
		}
		// The MCP partition is ALWAYS evaluated (its telemetry read always concludes), so evaluated is
		// never empty and this persist is always reached: the MCP partition promotes its this-run rows
		// or auto-closes if it produced none. The HOOK partition joins only when the coverage-gap is
		// resolved or a finding was emitted — a dormant gap leaves it out so the standing gap survives.
		if len(gated) > 0 || len(evaluated) > 0 {
			if persistErr := e.libraryDAO.PersistFindings(sCtx, orgID, exec.ID, gated, toDAOPartitions(evaluated)); persistErr != nil {
				return fmt.Errorf("persisting org findings: %w", persistErr)
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

// orgEvaluatedPartition builds one fully-evaluated ORG partition for the given artifact type — Scope
// ORG, ScopeValue empty (the model's org-scope convention).
func orgEvaluatedPartition(artifactType promptlibraryv1.ArtifactType) evaluatedPartition {
	return evaluatedPartition{
		Scope:        promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_ORG,
		ScopeValue:   "",
		ArtifactType: artifactType,
	}
}

// orgAggregate does the org-wide relational reads and folds them into the OrgSecurityAggregate the
// two ORG detectors consume. The two harness_artifacts reads are VCS-org-canonical (person-less).
func (e *HarnessImproveEngine) orgAggregate(ctx context.Context, orgID string) (OrgSecurityAggregate, error) {
	aggregate := OrgSecurityAggregate{
		OrgID:              orgID,
		SecurityFlagCounts: map[string]int{},
		HookCoverageByRepo: map[string]bool{},
		RepoNamesByID:      map[string]string{},
	}
	// Class-A calendar window (same shape as Run.aggregate): conversation_start_date / security
	// flag_date are UTC calendar DATE columns compared inclusively, so anchor both bounds on a UTC
	// calendar-date midnight.
	now := time.Now().UTC()
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	from := to.AddDate(0, 0, -e.detectorCfg.windowDays)

	if err := func(sCtx context.Context) error {
		flagCounts, secErr := e.securityDAO.CountByFlagType(sCtx, orgID, from, to)
		if secErr != nil {
			return fmt.Errorf("reading security flags for org %s: %w", orgID, secErr)
		}
		aggregate.SecurityFlagCounts = flagCounts

		serverNames, mcpErr := e.sessionDAO.DistinctMcpServerNames(sCtx, orgID, from, to)
		if mcpErr != nil {
			return fmt.Errorf("reading distinct MCP server names for org %s: %w", orgID, mcpErr)
		}
		aggregate.McpServerNames = serverNames

		eligibleRepos, eligErr := e.harnessArtifactDAO.ListRepoIDsWithLiveVCSArtifacts(sCtx, orgID)
		if eligErr != nil {
			return fmt.Errorf("reading repos with live VCS artifacts for org %s: %w", orgID, eligErr)
		}
		// Coverage keys on the HOOK's NAME (event segment), not its body — but the content-joined read
		// is safe here because a committed HOOK is always body-born: UpsertArtifactManifest refuses a
		// child with no primary file (ArtifactManifestNeedsPrimary → the record is marked Failed, never
		// persisted live), so every LIVE HOOK row has a current_file_id and the JOIN never drops one.
		hookArtifacts, hookErr := e.harnessArtifactDAO.ListVCSArtifactContentByType(sCtx, orgID,
			[]commonv1.HarnessArtifactType{commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_HOOK})
		if hookErr != nil {
			return fmt.Errorf("reading committed hooks for org %s: %w", orgID, hookErr)
		}
		// A repo is covered iff it commits a HOOK whose event segment (the Name before ':') is
		// UserPromptSubmit — the prompt-time guard the coverage-gap lens checks for.
		coveredRepos := map[string]struct{}{}
		for _, hook := range hookArtifacts {
			if hookEventSegment(hook.Name) == userPromptSubmitEvent {
				coveredRepos[hook.RepositoryID] = struct{}{}
			}
		}
		for _, repoID := range eligibleRepos {
			_, covered := coveredRepos[repoID]
			aggregate.HookCoverageByRepo[repoID] = covered
		}
		// Resolve the eligible repo ids to their human names so the coverage-gap finding names the
		// uncovered repos (owner/repo) instead of raw UUIDs. Same org-scoped batch read the derivation
		// list view uses; an unresolvable id is simply absent (the finding falls back to the id).
		repos, repoErr := e.repositoryDAO.ListByIDs(sCtx, orgID, eligibleRepos)
		if repoErr != nil {
			return fmt.Errorf("resolving repo names for org %s: %w", orgID, repoErr)
		}
		for _, repo := range repos {
			name := repo.FullName
			if name == "" {
				name = repo.Name
			}
			if name != "" {
				aggregate.RepoNamesByID[repo.ID] = name
			}
		}
		// No org-level committed-PERMISSIONS read: neither ORG detector (OrgMcpFindings /
		// OrgCoverageGapFindings) needs it — the per-repo permission-inventory lens is what surfaces
		// PERMISSIONS findings, from each repo's own artifact.
		return nil
	}(ctx); err != nil {
		return OrgSecurityAggregate{}, fmt.Errorf("harness-improve org aggregate scope for org %s: %w", orgID, err)
	}
	return aggregate, nil
}

// hookEventSegment returns a hook artifact's event — the Name up to the first ':'. The ingest encodes
// a hook Name as "<event>" or "<event>:<matcher>", so a matcher-qualified hook and its bare-event
// sibling share one event segment.
func hookEventSegment(name string) string {
	if idx := strings.Index(name, ":"); idx >= 0 {
		return name[:idx]
	}
	return name
}
