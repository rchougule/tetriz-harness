// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"

	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/logger"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// The findings gate is the harness-improve counterpart of the derive QualityGate (see the
// detector spec). It is a SEPARATE grading function, not a branch of
// QualityGate.Grade (which is derive-shaped): it reuses the suppression reads and pathsGrounded idea
// but not the additive-only checks. It runs in the detect/LLM middle — OUTSIDE the run's write scope
// — and decides, per finding: suppress (VISIBLE→HIDDEN_FAILED_EVAL) and fix-grounding (an ungrounded
// fix is withheld from the grounded-fix set so its hallucinated body can never persist, while the
// finding stays VISIBLE as advisory — a fix never hides a finding). Every produced finding is KEPT
// in the persist set: a partially-evaluated partition is handled SOLELY by excluding it from the
// fully-evaluated partition set the auto-close reconcile consumes (its finding rows still upsert and
// its prior is_latest is left untouched), never by dropping a valid finding.

// evaluatedPartition identifies one (scope, scope_value, artifact_type) partition this run fully
// evaluated — the unit the auto-close reconcile reconciles over. A partition present here (even with
// zero surviving findings) is a partition whose stale DETECTED rows the auto-close reconcile may
// clear; a partition ABSENT here (partially evaluated) must keep its prior is_latest untouched, so a
// lens that did not run cannot retract a standing finding.
type evaluatedPartition struct {
	Scope        promptlibraryv1.ArtifactScope
	ScopeValue   string
	ArtifactType promptlibraryv1.ArtifactType
}

// scannedPartition is one candidate partition the run's detectors covered for this repo, tagged with
// the degradable lenses that feed it: pathFed when a path-asserting Conflict lens (dead-glob /
// broken-path) reads its artifact type, behavioralFed when the behavioral-deviation lens does. A
// partition fed by neither (a per-repo Security PERMISSIONS partition, an instruction partition with
// no path/behavioral dependency) is always fully evaluated. The tags let the gate exclude only the
// partitions whose feeding lens degraded this run, while still representing an N→0 fully-evaluated
// partition that emitted no finding.
type scannedPartition struct {
	partition     evaluatedPartition
	pathFed       bool
	behavioralFed bool
	// bodyComplete is false when any committed body-requiring artifact of this partition's type had no
	// fetched body this run (an unresolvable file, a missing/unreadable the content store object, a truncated primary,
	// or the per-run body-fetch cap). The body-requiring detectors SKIP a body-less artifact, so the
	// type was not fully evaluated — its partition is then excluded from the evaluated set, the same
	// conservative treatment a degraded path/behavioral lens gives the partitions it feeds.
	bodyComplete bool
}

// evaluationStatus carries the run's per-repo evaluation completeness the gate needs. pathLensComplete
// is false when the path-asserting Conflict lenses could not run against a full tree (truncated or
// empty), behavioralComplete is false when any behavioral candidate degraded in the intent-classify
// leg; both drive the evaluated-partition exclusion (per-partition body completeness is the third).
// scannedPartitions is the per-repo candidate set (the partitions the detectors covered), so a
// fully-evaluated partition that produced zero findings still reaches the auto-close reconcile.
type evaluationStatus struct {
	pathLensComplete   bool
	behavioralComplete bool
	scannedPartitions  []scannedPartition
}

// deriveEvaluationStatus derives the run's completeness from the aggregate (path-lens inputs + the
// fetched bodies) plus the behavioral-degrade signal enrichIntents fed back onto it. scannedPartitions
// is one entry per DISTINCT prompt_library artifact type the repo's committed body-requiring artifacts
// map to, each tagged with whether a path-asserting Conflict lens and/or the behavioral lens feeds it
// and whether EVERY body-requiring artifact of that type had its body fetched — so the partition is
// represented even when it produced no finding (the N→0 auto-close case), and excluded when a lens it
// depends on degraded or a body its detectors need was never fetched.
//
// Exclusion resolves at (scope, artifactType) granularity and never drops a finding: a tree-independent
// sub-lens that shares a type with a degraded one (e.g. dup-id on the RULE type) has its finding KEPT
// yet its whole RULE partition EXCLUDED from the evaluated set under a truncated tree. That is the safe
// direction: auto-close is withheld until a clean run reports the partition, and a standing is_latest
// finding is never wrongly retracted.
func (a harnessImproveAggregate) deriveEvaluationStatus(scope promptlibraryv1.ArtifactScope, scopeValue string) evaluationStatus {
	status := evaluationStatus{
		// A truncated or empty tree means the path/glob lenses degraded (they never assert
		// non-existence against a partial listing), so a partition they feed is not fully evaluated.
		pathLensComplete:   !a.treeTruncated && len(a.repoPaths) > 0,
		behavioralComplete: !a.behavioralDegraded,
	}
	indexByType := map[promptlibraryv1.ArtifactType]int{}
	for _, artifact := range a.committed {
		// Only a type a wired detector reads is a candidate partition; a non-detectable type is
		// never scanned, so it must not report itself fully evaluated to the auto-close reconcile.
		if !bodyRequiredForDetection(artifact) {
			continue
		}
		artifactType := instructionArtifactType(artifact.Type)
		if artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_UNSPECIFIED {
			continue
		}
		index, seen := indexByType[artifactType]
		if !seen {
			status.scannedPartitions = append(status.scannedPartitions, scannedPartition{
				partition:    evaluatedPartition{Scope: scope, ScopeValue: scopeValue, ArtifactType: artifactType},
				bodyComplete: true,
			})
			index = len(status.scannedPartitions) - 1
			indexByType[artifactType] = index
		}
		// The path-asserting Conflict lenses read exactly the Conflict-bodied types (broken-path
		// covers all of them, dead-glob a subset), so a Conflict-bodied artifact marks its partition
		// path-fed; a behavioral candidate marks it behavioral-fed.
		if bodyRequiredForConflict(artifact) {
			status.scannedPartitions[index].pathFed = true
		}
		if isBehavioralCandidate(artifact) {
			status.scannedPartitions[index].behavioralFed = true
		}
		// A body-requiring artifact absent from the fetched bodies (unresolvable file, the content store miss/read
		// error, truncated primary, or the per-run body-fetch cap) was SKIPPED by its detectors, so its
		// type was not fully evaluated — mark the partition body-incomplete so it is excluded from the
		// evaluated set exactly as a degraded lens excludes the partitions it feeds.
		if _, hasBody := a.bodies[artifact.ID]; !hasBody {
			status.scannedPartitions[index].bodyComplete = false
		}
	}
	return status
}

// evaluatedPartitions returns the fully-evaluated subset of the run's scanned partitions: a partition
// is excluded when a lens that feeds it degraded this run (path-asserting when !pathLensComplete,
// behavioral when !behavioralComplete) or a body its detectors need was never fetched (!bodyComplete).
// A partition fed by neither degradable lens whose bodies all fetched is always emitted.
func (s evaluationStatus) evaluatedPartitions() []evaluatedPartition {
	evaluated := make([]evaluatedPartition, 0, len(s.scannedPartitions))
	for _, candidate := range s.scannedPartitions {
		if candidate.pathFed && !s.pathLensComplete {
			continue
		}
		if candidate.behavioralFed && !s.behavioralComplete {
			continue
		}
		if !candidate.bodyComplete {
			continue
		}
		evaluated = append(evaluated, candidate.partition)
	}
	return evaluated
}

// gateFindings grades one run's detector findings and reports the partitions it fully evaluated. It
// suppresses a finding whose (type, slug) the org dismissed or recently hid (a PERSISTED hide —
// VISIBLE→HIDDEN_FAILED_EVAL, kept in gated) and grounds any fix's cited paths (a fix whose paths all
// resolve is returned in groundedFixes; an ungrounded fix is ABSENT from groundedFixes so its
// hallucinated body can never persist, while the finding itself stays VISIBLE as advisory — a fix
// never hides a real finding). Every produced finding is KEPT in gated; a partially-evaluated
// partition is withheld from evaluated (never dropped as a finding), so its rows upsert and its prior
// is_latest is left untouched. It asserts every surfaced finding carries a non-zero priority (the
// detectors stamp it; a zero is a detector bug). fixes and groundedFixes are both keyed by finding
// slug. gated is the persist set; evaluated is the fully-evaluated partition set the auto-close
// reconcile reads over; groundedFixes is the subset of fixes safe to persist; hiddenReasons carries one
// gate_hidden_total reason label per suppressed finding, for the caller to meter only after the run
// commits (so gate_hidden is attributed to the same committed runs as findings_emitted, never to a
// superseded or failed attempt).
func (e *HarnessImproveEngine) gateFindings(
	ctx context.Context,
	orgID, scopeValue string,
	scope promptlibraryv1.ArtifactScope,
	findings []*model.PromptLibraryItem,
	fixes map[string]HarnessFixResult,
	repoPaths map[string]struct{},
	evalStatus evaluationStatus,
) (gated []*model.PromptLibraryItem, evaluated []evaluatedPartition, groundedFixes map[string]HarnessFixResult, hiddenReasons []string, err error) {
	// The suppression reads are the only DB work here; run them in their own short tenant scope (the
	// split-scope discipline: the detect/LLM middle holds no open tenant transaction).
	var dismissed map[string]struct{}
	var hidden map[typedSlugKey]struct{}
	if scopeErr := func(sCtx context.Context) error {
		var readErr error
		dismissed, hidden, readErr = e.harnessSuppressionSlugs(sCtx, orgID, scope, scopeValue)
		if readErr != nil {
			return fmt.Errorf("reading suppression slugs: %w", readErr)
		}
		return nil
	}(ctx); scopeErr != nil {
		return nil, nil, nil, nil, fmt.Errorf("harness-improve gate suppression scope for org %s scope-value %s: %w", orgID, scopeValue, scopeErr)
	}

	gated = make([]*model.PromptLibraryItem, 0, len(findings))
	groundedFixes = make(map[string]HarnessFixResult, len(fixes))
	for _, finding := range findings {
		// The detectors assign a fixed per-family priority; a zero is a detector wiring bug, not a value
		// the gate re-maps. Asserted so a zero-priority finding fails loud rather than persisting a
		// mis-stamped row, masking the detector bug on exactly the runs that most need it caught.
		if finding.Priority == 0 {
			return nil, nil, nil, nil, fmt.Errorf("harness-improve gate: finding %q (type %s) has zero priority — a detector must stamp one", finding.Slug, finding.ArtifactType)
		}
		// Suppression is a PERSISTED hide (the user dismissed the name / it was recently hidden): flip
		// VISIBLE→HIDDEN_FAILED_EVAL but keep it in gated so the recent-hidden dedup and auto-close see it.
		// The reason (dismissed vs recently_hidden) is only in scope here, so collect it for the caller to
		// meter after the run commits — metering in-place would count a run that later supersedes or fails.
		suppressionSlug := strings.ToLower(strings.TrimSpace(finding.Slug))
		if reason := harnessGateHiddenReason(suppressionSlug, finding.ArtifactType, dismissed, hidden); reason != "" {
			finding.SurfaceState = promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL
			hiddenReasons = append(hiddenReasons, reason)
		}
		if fix, hasFix := fixes[finding.Slug]; hasFix {
			if groundedFix(fix, repoPaths) {
				// Every cited path resolves: the fix body is safe to persist, so surface it in the
				// grounded-fix set the caller may write.
				groundedFixes[finding.Slug] = fix
			} else {
				// An ungrounded fix cited a path the tree does not contain: withhold the fix body from
				// groundedFixes (so a hallucinated SuggestedContent can never be written) and leave the
				// finding VISIBLE as advisory — the problem is real even when the generated fix hallucinated.
				logger.Info(ctx, "harness-improve gate: dropping ungrounded fix body, finding stays advisory",
					zap.String(logger.ORG_ID, orgID), zap.String("slug", finding.Slug))
			}
		}
		gated = append(gated, finding)
	}

	return gated, evalStatus.evaluatedPartitions(), groundedFixes, hiddenReasons, nil
}

// visibleFindingCount reports how many gated findings remain VISIBLE. A suppressed finding is flipped
// to HIDDEN_FAILED_EVAL yet KEPT in gated (for dedup/audit), so len(gated) overcounts what actually
// surfaces; the run's terminal status must key on this count, not on len(gated), so an all-suppressed
// run stamps COMPLETED_NO_VISIBLE. The persist decision stays on len(gated) — the HIDDEN rows are
// still written. This is the harness-improve counterpart of the derive engine's graded.NoneVisible().
func visibleFindingCount(gated []*model.PromptLibraryItem) int {
	visible := 0
	for _, finding := range gated {
		if finding.SurfaceState == promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE {
			visible++
		}
	}
	return visible
}

// evaluatedPartitionTypeNames returns the sorted, de-duplicated ArtifactType names present in the
// evaluated partition set — a bounded, small set logged beside the count so the observe-dark
// telemetry can tell a correct partition set from a wrong one of equal cardinality.
func evaluatedPartitionTypeNames(evaluated []evaluatedPartition) []string {
	seen := map[string]struct{}{}
	for _, partition := range evaluated {
		seen[partition.ArtifactType.String()] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// groundedFix reports whether every non-blank cited path of a fix resolves to a committed file. The
// cited paths are normalized to the tree's clean repo-relative form (reusing normalizeRepoPath) before
// membership so "./x", "/x", "x/", or a backslash separator still ground. An empty cited-path set
// grounds vacuously.
func groundedFix(fix HarnessFixResult, repoPaths map[string]struct{}) bool {
	for _, citedPath := range fix.CitedPaths {
		if strings.TrimSpace(citedPath) == "" {
			continue
		}
		if _, ok := repoPaths[normalizeRepoPath(citedPath)]; !ok {
			return false
		}
	}
	return true
}

// harnessSuppressionSlugs returns the gate's two exact-match suppression sets for one (scope,
// scopeValue): the org's all-history admin-DISMISSED slugs (keyed by slug) and its recently-HIDDEN
// slugs (windowed to the last RecentHiddenWindowRuns distinct runs, keyed by (artifact_type, slug) so
// a hidden rule does not suppress a same-slug hook). It is the scope-parameterized harness counterpart
// of the derive gate's suppressionSlugs — it takes scope as an argument (so an ORG-scoped finding
// suppresses correctly) and deliberately reads ONLY the two suppression sets, never the derive gate's
// pinned set. Stored slugs are already canonical, so this only lowercases + trims as a defensive guard
// against case/whitespace drift.
func (e *HarnessImproveEngine) harnessSuppressionSlugs(ctx context.Context, orgID string, scope promptlibraryv1.ArtifactScope, scopeValue string) (map[string]struct{}, map[typedSlugKey]struct{}, error) {
	dismissed, err := e.libraryDAO.ListDismissedSlugs(ctx, orgID, scope, scopeValue)
	if err != nil {
		return nil, nil, fmt.Errorf("listing dismissed slugs for org %s scope %s value %s: %w", orgID, scope, scopeValue, err)
	}
	detectedSource := promptlibraryv1.ArtifactRecommendationSource_ARTIFACT_RECOMMENDATION_SOURCE_DETECTED
	hidden, err := e.libraryDAO.ListRecentHiddenFailedSlugs(ctx, orgID, scope, scopeValue, &detectedSource, e.detectorCfg.recentHiddenWindowRuns)
	if err != nil {
		return nil, nil, fmt.Errorf("listing recent hidden slugs for org %s scope %s value %s: %w", orgID, scope, scopeValue, err)
	}
	dismissedSet := make(map[string]struct{}, len(dismissed))
	for _, slug := range dismissed {
		if normalized := strings.ToLower(strings.TrimSpace(slug)); normalized != "" {
			dismissedSet[normalized] = struct{}{}
		}
	}
	hiddenSet := make(map[typedSlugKey]struct{}, len(hidden))
	for _, row := range hidden {
		if normalized := strings.ToLower(strings.TrimSpace(row.Slug)); normalized != "" {
			hiddenSet[typedSlugKey{artifactType: row.ArtifactType, slug: normalized}] = struct{}{}
		}
	}
	return dismissedSet, hiddenSet, nil
}
