// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	daomocks "github.com/tetriz-ai/tetriz-harness/recommendations/dao/mocks"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

const (
	gateOrgID  = "11111111-1111-1111-1111-111111111111"
	gateExecID = "22222222-2222-2222-2222-222222222222"
	gateRepoID = "33333333-3333-3333-3333-333333333333"
)

// newGateEngine builds an engine carrying only the fields the findings gate reads (the library DAO
// for suppression + the resolved detector settings), so gateFindings can be exercised in isolation.
func newGateEngine(t *testing.T) (*HarnessImproveEngine, *daomocks.MockIPromptLibraryDAO) {
	t.Helper()
	ctrl := gomock.NewController(t)
	lib := daomocks.NewMockIPromptLibraryDAO(ctrl)
	return &HarnessImproveEngine{libraryDAO: lib, detectorCfg: defaultDetectorConfig()}, lib
}

// expectSuppression programs the two suppression reads gateFindings always issues for a PROJECT run.
func expectSuppression(lib *daomocks.MockIPromptLibraryDAO, dismissed []string, hidden []model.ArtifactSlugRef) {
	lib.EXPECT().ListDismissedSlugs(gomock.Any(), gateOrgID,
		promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateRepoID).Return(dismissed, nil).Times(1)
	lib.EXPECT().ListRecentHiddenFailedSlugs(gomock.Any(), gateOrgID,
		promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateRepoID, gomock.Any(), defaultRecentHiddenWindowRuns).
		Return(hidden, nil).Times(1)
}

func artifactOfType(id string, artifactType commonv1.HarnessArtifactType) *model.HarnessArtifact {
	return &model.HarnessArtifact{ID: id, Type: artifactType, Name: id, SourcePath: id + ".md"}
}

// deadGlobFinding is a path-asserting Conflict finding (degrades on an incomplete tree).
func deadGlobFinding(artifactID string) *model.PromptLibraryItem {
	return newConflictFinding(gateOrgID, gateRepoID, gateExecID, artifactOfType(artifactID, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE),
		conflictFamilyDeadGlob, "Rule pattern matches no files in the repo", "src/**/*.ts", priorityHigh)
}

// dupIDFinding is a non-path Conflict finding (always fully evaluated).
func dupIDFinding(artifactID string) *model.PromptLibraryItem {
	return newConflictFinding(gateOrgID, gateRepoID, gateExecID, artifactOfType(artifactID, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE),
		conflictFamilyDupID, "Rule ID defined in more than one file", "GO-001 also defined in b.md", priorityMedium)
}

// behavioralFinding is the Instruction-Intent behavioral-deviation finding (degrades on a bad classify leg).
func behavioralFinding(artifactID string) *model.PromptLibraryItem {
	return newBehavioralFinding(gateOrgID, gateExecID, gateRepoID, artifactOfType(artifactID, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL),
		"mechanical", 0.9, "completion 0.10 outside [0.50,1.00]")
}

// securityFinding is a per-repo Security permission finding (always fully evaluated).
func securityFinding(artifactID string) *model.PromptLibraryItem {
	return newProjectFinding(gateOrgID, gateRepoID, gateExecID, artifactOfType(artifactID, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_PERMISSIONS),
		promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_SECURITY, priorityHigh,
		securitySlug(gateRepoID, securityFamilyPermissionGrant, artifactID, "Bash(*)"), "Over-broad permission grant", "allow Bash(*)")
}

// completeStatus is the all-lenses-ran evaluation status: no transient drop fires.
func completeStatus() evaluationStatus {
	return evaluationStatus{pathLensComplete: true, behavioralComplete: true}
}

// TestGateFindings_Suppression asserts the VISIBLE→HIDDEN_FAILED_EVAL flip: a VISIBLE finding stays
// VISIBLE + in gated; a dismissed slug or a recently-hidden (type, slug) flips to HIDDEN_FAILED_EVAL
// but stays in gated (a PERSISTED hide the recent-hidden dedup and auto-close must still see).
func TestGateFindings_Suppression(t *testing.T) {
	finding := deadGlobFinding("art-1")
	tests := []struct {
		name      string
		dismissed []string
		hidden    []model.ArtifactSlugRef
		wantState promptlibraryv1.ArtifactSurfaceState
	}{
		{
			name:      "unsuppressed stays visible",
			wantState: promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE,
		},
		{
			name:      "dismissed slug hides",
			dismissed: []string{finding.Slug},
			wantState: promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL,
		},
		{
			name:      "recent-hidden typed slug hides",
			hidden:    []model.ArtifactSlugRef{{ArtifactType: finding.ArtifactType, Slug: finding.Slug}},
			wantState: promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL,
		},
		{
			// A hidden slug of a DIFFERENT type must not suppress this RULE finding (typed dedup).
			name:      "recent-hidden other type does not hide",
			hidden:    []model.ArtifactSlugRef{{ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL, Slug: finding.Slug}},
			wantState: promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine, lib := newGateEngine(t)
			expectSuppression(lib, tt.dismissed, tt.hidden)
			item := deadGlobFinding("art-1")
			gated, _, _, _, err := engine.gateFindings(context.Background(), gateOrgID, gateRepoID,
				promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, []*model.PromptLibraryItem{item},
				nil, map[string]struct{}{}, completeStatus())
			require.NoError(t, err)
			require.Len(t, gated, 1, "a suppressed finding is a persisted hide, still in gated")
			require.Equal(t, tt.wantState, gated[0].SurfaceState)
		})
	}
}

// TestGateFindings_PartialPartitionKeepsFinding locks the spec-correct partial-partition contract:
// every produced finding is KEPT in gated (it upserts) — a valid behavioral finding from a
// successfully-classified artifact is not dropped just because ANOTHER candidate degraded the classify
// leg — while the degraded partition is withheld from evaluated (so its prior is_latest survives and
// auto-close cannot retract a standing finding). No finding is ever transient-dropped.
func TestGateFindings_PartialPartitionKeepsFinding(t *testing.T) {
	behavioralStatus := evaluationStatus{
		pathLensComplete:   true,
		behavioralComplete: false, // one candidate degraded the classify leg
		scannedPartitions: []scannedPartition{
			{partition: partitionOf(promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL), behavioralFed: true, bodyComplete: true},
		},
	}
	pathStatus := evaluationStatus{
		pathLensComplete:   false, // truncated tree
		behavioralComplete: true,
		scannedPartitions: []scannedPartition{
			{partition: partitionOf(promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE), pathFed: true, bodyComplete: true},
		},
	}
	tests := []struct {
		name    string
		finding *model.PromptLibraryItem
		status  evaluationStatus
	}{
		{
			// The behavioral finding is valid data (a successfully-classified artifact), so it is KEPT even
			// though the classify leg degraded; only its SKILL partition is withheld from evaluated.
			name:    "behavioral finding kept while its degraded partition is excluded",
			finding: behavioralFinding("art-1"),
			status:  behavioralStatus,
		},
		{
			// A path-asserting dead-glob finding is likewise KEPT under a truncated tree; its RULE partition
			// is withheld from evaluated.
			name:    "dead-glob finding kept while its truncated partition is excluded",
			finding: deadGlobFinding("art-2"),
			status:  pathStatus,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine, lib := newGateEngine(t)
			expectSuppression(lib, nil, nil)
			gated, evaluated, _, _, err := engine.gateFindings(context.Background(), gateOrgID, gateRepoID,
				promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, []*model.PromptLibraryItem{tt.finding},
				nil, map[string]struct{}{}, tt.status)
			require.NoError(t, err)
			require.Len(t, gated, 1, "a produced finding is always kept — partial partitions are handled by the evaluated-set exclusion, not by dropping findings")
			require.Equal(t, promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE, gated[0].SurfaceState)
			require.Empty(t, typesOf(evaluated),
				"the degraded partition is withheld from evaluated so its prior is_latest is not retracted")
		})
	}
}

// TestGateFindings_FixGrounding asserts an ungrounded fix never hides the finding AND is withheld from
// the returned grounded-fix set: with a grounded fix the finding stays VISIBLE and the fix appears in
// groundedFixes; with an ungrounded fix the finding still stays VISIBLE but the fix is ABSENT from
// groundedFixes, so its hallucinated body can never be persisted.
func TestGateFindings_FixGrounding(t *testing.T) {
	repoPaths := map[string]struct{}{"src/app.ts": {}, "docs/readme.md": {}}
	tests := []struct {
		name         string
		cited        []string
		wantGrounded bool
	}{
		{name: "all cited paths ground", cited: []string{"src/app.ts", "./docs/readme.md"}, wantGrounded: true},
		{name: "a cited path does not ground", cited: []string{"src/app.ts", "src/ghost.ts"}, wantGrounded: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine, lib := newGateEngine(t)
			expectSuppression(lib, nil, nil)
			finding := deadGlobFinding("art-1")
			fixes := map[string]HarnessFixResult{finding.Slug: {SuggestedContent: "fixed", CitedPaths: tt.cited}}
			gated, _, groundedFixes, _, err := engine.gateFindings(context.Background(), gateOrgID, gateRepoID,
				promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, []*model.PromptLibraryItem{finding},
				fixes, repoPaths, completeStatus())
			require.NoError(t, err)
			require.Len(t, gated, 1, "an ungrounded fix demotes to advisory, it never hides the finding")
			require.Equal(t, promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE, gated[0].SurfaceState)
			if tt.wantGrounded {
				require.Contains(t, groundedFixes, finding.Slug, "a grounded fix is returned so the caller may persist it")
				require.Equal(t, "fixed", groundedFixes[finding.Slug].SuggestedContent)
			} else {
				require.NotContains(t, groundedFixes, finding.Slug, "an ungrounded fix is withheld so its hallucinated body can never persist")
			}
		})
	}
}

// TestGroundedFix is the fix-body keep/drop decision: every non-blank cited path must resolve (after
// normalization) to a committed file; a blank entry is ignored and an empty set grounds vacuously.
func TestGroundedFix(t *testing.T) {
	repoPaths := map[string]struct{}{"src/app.ts": {}, "docs/readme.md": {}}
	tests := []struct {
		name  string
		cited []string
		want  bool
	}{
		{name: "all ground", cited: []string{"src/app.ts", "docs/readme.md"}, want: true},
		{name: "normalized forms ground", cited: []string{"./src/app.ts", "/docs/readme.md"}, want: true},
		{name: "one hallucinated fails", cited: []string{"src/app.ts", "src/ghost.ts"}, want: false},
		{name: "blank entries ignored", cited: []string{"", "  "}, want: true},
		{name: "empty grounds vacuously", cited: nil, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, groundedFix(HarnessFixResult{CitedPaths: tt.cited}, repoPaths))
		})
	}
}

// TestGateFindings_ZeroPriorityErrors asserts the gate flags a surfaced finding with zero priority
// (a detector wiring bug) rather than persisting it or silently re-mapping the priority.
func TestGateFindings_ZeroPriorityErrors(t *testing.T) {
	engine, lib := newGateEngine(t)
	expectSuppression(lib, nil, nil)
	finding := dupIDFinding("art-1")
	finding.Priority = 0
	gated, evaluated, groundedFixes, _, err := engine.gateFindings(context.Background(), gateOrgID, gateRepoID,
		promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, []*model.PromptLibraryItem{finding},
		nil, map[string]struct{}{}, completeStatus())
	require.Error(t, err)
	require.Nil(t, gated)
	require.Nil(t, evaluated)
	require.Nil(t, groundedFixes)
}

// TestGateFindings_AllSuppressedPersistsButNoVisible locks the two-decision split Run keys on: an
// all-suppressed run keeps its findings in gated (len>0, so they ARE persisted — the HIDDEN rows carry
// the dismissal state and let the auto-close reconcile re-promote the prior visible run) yet reports
// ZERO visible findings, so Run stamps COMPLETED_NO_VISIBLE rather than COMPLETED. Mirrors the derive
// engine's NoneVisible()/len(items) split.
func TestGateFindings_AllSuppressedPersistsButNoVisible(t *testing.T) {
	engine, lib := newGateEngine(t)
	finding := securityFinding("art-1")
	// Dismiss the only finding's slug, so the gate flips it VISIBLE→HIDDEN_FAILED_EVAL but keeps it.
	expectSuppression(lib, []string{finding.Slug}, nil)
	gated, _, _, _, err := engine.gateFindings(context.Background(), gateOrgID, gateRepoID,
		promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, []*model.PromptLibraryItem{finding},
		nil, map[string]struct{}{}, completeStatus())
	require.NoError(t, err)
	require.Len(t, gated, 1, "a suppressed finding is a persisted hide, still in gated (persist keys on len(gated))")
	require.Equal(t, promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL, gated[0].SurfaceState)
	require.Zero(t, visibleFindingCount(gated),
		"an all-suppressed run has zero VISIBLE findings, so Run stamps COMPLETED_NO_VISIBLE, not COMPLETED")
}

// TestGateFindings_NonPathFindingKeptWhilePartitionExcluded locks the subtlest interaction of the two
// resolutions: partition exclusion is at (scope, artifactType) granularity while the transient-drop is
// per finding-family. A non-path dup-id RULE finding does not depend on the tree, so it SURVIVES a
// truncated run (kept in gated), yet its RULE partition is path-fed, so the truncated tree EXCLUDES
// that partition from evaluated — the safe/conservative direction (auto-close withheld until a clean
// run reports the partition).
func TestGateFindings_NonPathFindingKeptWhilePartitionExcluded(t *testing.T) {
	engine, lib := newGateEngine(t)
	expectSuppression(lib, nil, nil)
	status := evaluationStatus{
		pathLensComplete:   false, // truncated tree
		behavioralComplete: true,
		scannedPartitions: []scannedPartition{
			{partition: partitionOf(promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE), pathFed: true, bodyComplete: true},
		},
	}
	finding := dupIDFinding("art-1") // a non-path Conflict finding on the RULE type
	gated, evaluated, _, _, err := engine.gateFindings(context.Background(), gateOrgID, gateRepoID,
		promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, []*model.PromptLibraryItem{finding},
		nil, map[string]struct{}{}, status)
	require.NoError(t, err)
	require.Len(t, gated, 1, "a non-path dup-id finding does not depend on the tree, so it survives a truncated run")
	require.Empty(t, typesOf(evaluated),
		"the RULE partition is path-fed, so a truncated tree excludes it from evaluated even though its non-path finding was kept")
}

// TestEvaluatedPartitions asserts the evaluated-partition filter: a fully-evaluated partition (even
// one that produced zero findings) is emitted, a path-fed partition is excluded under an incomplete
// tree, and a behavioral-fed partition is excluded under a degraded classify leg.
func TestEvaluatedPartitions(t *testing.T) {
	rule := scannedPartition{partition: partitionOf(promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE), pathFed: true, bodyComplete: true}
	skill := scannedPartition{partition: partitionOf(promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL), pathFed: true, behavioralFed: true, bodyComplete: true}
	agent := scannedPartition{partition: partitionOf(promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT), behavioralFed: true, bodyComplete: true}
	permissions := scannedPartition{partition: partitionOf(promptlibraryv1.ArtifactType_ARTIFACT_TYPE_PERMISSIONS), bodyComplete: true}
	all := []scannedPartition{rule, skill, agent, permissions}

	tests := []struct {
		name   string
		status evaluationStatus
		want   []promptlibraryv1.ArtifactType
	}{
		{
			name:   "all lenses complete emits every scanned partition (incl N to 0)",
			status: evaluationStatus{pathLensComplete: true, behavioralComplete: true, scannedPartitions: all},
			want: []promptlibraryv1.ArtifactType{
				promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE,
				promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL,
				promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT,
				promptlibraryv1.ArtifactType_ARTIFACT_TYPE_PERMISSIONS,
			},
		},
		{
			name:   "truncated tree excludes path-fed partitions",
			status: evaluationStatus{pathLensComplete: false, behavioralComplete: true, scannedPartitions: all},
			want: []promptlibraryv1.ArtifactType{
				promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT,
				promptlibraryv1.ArtifactType_ARTIFACT_TYPE_PERMISSIONS,
			},
		},
		{
			name:   "behavioral degrade excludes behavioral-fed partitions",
			status: evaluationStatus{pathLensComplete: true, behavioralComplete: false, scannedPartitions: all},
			want: []promptlibraryv1.ArtifactType{
				promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE,
				promptlibraryv1.ArtifactType_ARTIFACT_TYPE_PERMISSIONS,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, typesOf(tt.status.evaluatedPartitions()))
		})
	}
}

// TestAggregateEvaluationStatus asserts the per-repo candidate-partition derivation from committed
// artifacts: each detectable type becomes one partition tagged with the degradable lenses that feed
// it (a Conflict-bodied type is path-fed, a behavioral candidate is behavioral-fed, PERMISSIONS is
// neither), and the completeness bools mirror the aggregate's tree/behavioral state.
func TestAggregateEvaluationStatus(t *testing.T) {
	agg := harnessImproveAggregate{
		committed: []*model.HarnessArtifact{
			artifactOfType("r", commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE),
			artifactOfType("s", commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL),
			artifactOfType("a", commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT),
			artifactOfType("p", commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_PERMISSIONS),
		},
		// Every committed body-requiring artifact has a fetched body, so all partitions are body-complete.
		bodies:             map[string]string{"r": "r-body", "s": "s-body", "a": "a-body", "p": "p-body"},
		repoPaths:          map[string]struct{}{"src/app.ts": {}},
		treeTruncated:      false,
		behavioralDegraded: false,
	}
	status := agg.deriveEvaluationStatus(promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateRepoID)
	require.True(t, status.pathLensComplete)
	require.True(t, status.behavioralComplete)

	byType := map[promptlibraryv1.ArtifactType]scannedPartition{}
	for _, sp := range status.scannedPartitions {
		require.Equal(t, gateRepoID, sp.partition.ScopeValue)
		require.Equal(t, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, sp.partition.Scope)
		byType[sp.partition.ArtifactType] = sp
	}
	require.Len(t, byType, 4)
	require.True(t, byType[promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE].pathFed)
	require.False(t, byType[promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE].behavioralFed)
	require.True(t, byType[promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL].pathFed)
	require.True(t, byType[promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL].behavioralFed)
	require.False(t, byType[promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT].pathFed)
	require.True(t, byType[promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT].behavioralFed)
	require.False(t, byType[promptlibraryv1.ArtifactType_ARTIFACT_TYPE_PERMISSIONS].pathFed)
	require.False(t, byType[promptlibraryv1.ArtifactType_ARTIFACT_TYPE_PERMISSIONS].behavioralFed)
	// Every committed body-requiring artifact had a fetched body, so every partition is body-complete.
	for artifactType, partition := range byType {
		require.True(t, partition.bodyComplete, "partition %s has all bodies fetched", artifactType)
	}

	// A truncated tree flips pathLensComplete; a behavioral degrade flips behavioralComplete.
	agg.treeTruncated = true
	agg.behavioralDegraded = true
	degraded := agg.deriveEvaluationStatus(promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateRepoID)
	require.False(t, degraded.pathLensComplete)
	require.False(t, degraded.behavioralComplete)
	// Only the PERMISSIONS partition (neither path- nor behavioral-fed) survives both degrades.
	require.Equal(t, []promptlibraryv1.ArtifactType{promptlibraryv1.ArtifactType_ARTIFACT_TYPE_PERMISSIONS},
		typesOf(degraded.evaluatedPartitions()))
}

// TestDeriveEvaluationStatus_BodyIncompletePartitionExcluded locks the body-fetch completeness gate: a
// type is fully evaluated only when EVERY committed body-requiring artifact of that type had its body
// fetched. A RULE type with one body absent from aggregate.bodies (an unresolvable file, an the content store
// miss/read error, a truncated primary, or the per-run body-fetch cap) is EXCLUDED from evaluated,
// since its body-requiring detectors skipped the body-less artifact; a repo whose bodies all fetched
// is included.
func TestDeriveEvaluationStatus_BodyIncompletePartitionExcluded(t *testing.T) {
	// Two RULE artifacts share the RULE partition; PERMISSIONS is a single independent partition.
	committed := []*model.HarnessArtifact{
		artifactOfType("rule-a", commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE),
		artifactOfType("rule-b", commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE),
		artifactOfType("perm", commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_PERMISSIONS),
	}
	tests := []struct {
		name   string
		bodies map[string]string
		want   []promptlibraryv1.ArtifactType
	}{
		{
			name:   "all bodies fetched includes every partition",
			bodies: map[string]string{"rule-a": "a", "rule-b": "b", "perm": "p"},
			want: []promptlibraryv1.ArtifactType{
				promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE,
				promptlibraryv1.ArtifactType_ARTIFACT_TYPE_PERMISSIONS,
			},
		},
		{
			// rule-b's body is absent, so the RULE partition is not fully evaluated and is excluded, even
			// though a RULE artifact (rule-a) was committed and scanned.
			name:   "one RULE body absent excludes the RULE partition",
			bodies: map[string]string{"rule-a": "a", "perm": "p"},
			want:   []promptlibraryv1.ArtifactType{promptlibraryv1.ArtifactType_ARTIFACT_TYPE_PERMISSIONS},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agg := harnessImproveAggregate{
				committed:     committed,
				bodies:        tt.bodies,
				repoPaths:     map[string]struct{}{"src/app.ts": {}},
				treeTruncated: false,
			}
			status := agg.deriveEvaluationStatus(promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, gateRepoID)
			require.ElementsMatch(t, tt.want, typesOf(status.evaluatedPartitions()))
		})
	}
}

func partitionOf(artifactType promptlibraryv1.ArtifactType) evaluatedPartition {
	return evaluatedPartition{Scope: promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, ScopeValue: gateRepoID, ArtifactType: artifactType}
}

func typesOf(partitions []evaluatedPartition) []promptlibraryv1.ArtifactType {
	types := make([]promptlibraryv1.ArtifactType, len(partitions))
	for i, partition := range partitions {
		types[i] = partition.ArtifactType
	}
	return types
}
