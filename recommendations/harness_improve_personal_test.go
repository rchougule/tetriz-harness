// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

const (
	hiPersonA = "aaaaaaaa-0000-0000-0000-00000000000a"
	hiPersonB = "bbbbbbbb-0000-0000-0000-00000000000b"
)

// TestHarnessImproveEngine_Run_PersonalFindings: a per-repo run that OWNS two engineers (their primary
// repo is this one) reads their personal harness through the DESKTOP+person read (never the VCS read),
// emits PERSON-scoped findings attributed to person + desktop artifact, evaluates each owned person's
// PERSON partitions (including a stale live one, so it auto-closes), and persists them in the SAME
// PersistFindings call as the committed lane — with no fix leg for any personal finding.
func TestHarnessImproveEngine_Run_PersonalFindings(t *testing.T) {
	h := newHarnessImproveHarness(t)
	const (
		committedPrID = "cccccccc-0000-0000-0000-0000000000c1"
		globalPrID    = "aaaaaaaa-1111-1111-1111-111111111111"
		globalDepID   = "aaaaaaaa-2222-2222-2222-222222222222"
		globalMDID    = "aaaaaaaa-3333-3333-3333-333333333333"
		bHookID       = "bbbbbbbb-1111-1111-1111-111111111111"
	)
	personA, personB := hiPersonA, hiPersonB
	// The usage window is anchored on the run's StartedAt, so the never-used min-age is measured
	// from it — the fixture stamps it as CreateIfNoneRunning would.
	exec := runningExec()
	exec.StartedAt = time.Now().UTC()
	oldEnough := exec.StartedAt.AddDate(0, 0, -60)

	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(exec, nil)
	h.expectBehavioralReadsEmpty()
	h.expectGateSuppressionEmpty()
	// The committed set: one skill "pr" whose body is unavailable (no harness_files row) so the committed
	// lenses stay silent and only the personal lane emits. Its CurrentFileID is the shadow lens's twin.
	h.artifacts.EXPECT().ListVCSByRepo(gomock.Any(), hiOrgID, hiRepoID).Return([]*model.HarnessArtifact{
		{
			ID: committedPrID, Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL, Name: "/pr",
			SourcePath: ".claude/skills/pr/SKILL.md", CurrentFileID: "team-pr-file",
		},
	}, nil)
	h.files.EXPECT().ListByIDs(gomock.Any(), hiOrgID, []string{"team-pr-file"}).Return(nil, nil)
	h.repos.EXPECT().GetByID(gomock.Any(), hiOrgID, hiRepoID).Return(&model.Repository{ID: hiRepoID}, nil)
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return(map[string]int{}, nil)
	// No live PROJECT partition; one STALE live PERSON partition for person B's SKILL type (B now has no
	// skill) that the run must union into the evaluated set so it auto-closes.
	h.library.EXPECT().ListLiveDetectedPartitions(gomock.Any(), hiOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, hiRepoID).Return(nil, nil)
	h.library.EXPECT().ListLiveDetectedPartitions(gomock.Any(), hiOrgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PERSON, "").
		Return([]dao.EvaluatedPartition{
			{Scope: promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PERSON, ScopeValue: personB, ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL},
			// Another (unowned) person's live partition must NOT be touched by this run.
			{Scope: promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PERSON, ScopeValue: "cccccccc-0000-0000-0000-00000000000c", ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL},
		}, nil)

	// The personal lane: this repo owns A and B. Read through the DESKTOP+person read only.
	h.usage.EXPECT().ListPrimaryRepoPersons(gomock.Any(), hiOrgID, hiRepoID, gomock.Any(), gomock.Any()).
		Return([]string{personA, personB}, nil)
	h.artifacts.EXPECT().ListPersonalByPersons(gomock.Any(), hiOrgID, []string{personA, personB}).Return([]*model.HarnessArtifact{
		// A: a global skill "pr" that shadows the committed one (different body) — and IS used.
		desktopArtifact(globalPrID, personA, nil, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL, "/pr", ".claude/skills/pr/SKILL.md", "mine-pr-file", oldEnough),
		// A: a global skill "deploy" never invoked → never-used.
		desktopArtifact(globalDepID, personA, nil, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL, "/deploy", ".claude/skills/deploy/SKILL.md", "dep-file", oldEnough),
		// A: a global CLAUDE.md — loaded, never invoked; no personal lens is about it.
		desktopArtifact(globalMDID, personA, nil, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD, "CLAUDE.md", ".claude/CLAUDE.md", "md-file", oldEnough),
		// B: a repo-local hook that fires regularly → no finding, but its HOOK partition is evaluated.
		desktopArtifact(bHookID, personB, strPtr(hiRepoID), commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_HOOK, "lint", ".claude/hooks/lint.sh", "hook-file", oldEnough),
	}, nil)
	h.usage.EXPECT().ListInvocationsForPersons(gomock.Any(), hiOrgID, []string{personA, personB}, gomock.Any(), gomock.Any()).
		Return([]dao.PersonalInvocationSummary{
			// A's "pr" is credited through an UNRESOLVED (identity-only) usage row — still counts as used.
			{PersonID: personA, ArtifactID: "", IdeType: commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, SourcePath: ".claude/skills/pr/SKILL.md", UserGlobal: true, Invocations: 2},
			// B's hook is credited through its resolved artifact id.
			{PersonID: personB, ArtifactID: bHookID, Invocations: 5},
		}, nil)
	// No fix leg for any personal finding: the fix generator must never be called.
	h.fix.EXPECT().GenerateHarnessFix(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED, gomock.Any(), gomock.Any(), "").
		Return(true, nil)
	h.library.EXPECT().PersistFindings(gomock.Any(), hiOrgID, hiExecID, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, items []*model.PromptLibraryItem, evaluated []dao.EvaluatedPartition) error {
			require.Len(t, items, 2, "never-used(deploy) + shadows-committed(pr), nothing for the CLAUDE.md or B's used hook")
			byArtifact := map[string]*model.PromptLibraryItem{}
			for _, item := range items {
				require.Equal(t, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PERSON, item.Scope)
				require.Equal(t, personA, item.ScopeValue, "every finding is A's — B produced none")
				require.NotNil(t, item.RecommendationSource)
				require.Equal(t, promptlibraryv1.ArtifactRecommendationSource_ARTIFACT_RECOMMENDATION_SOURCE_DETECTED, *item.RecommendationSource)
				require.NotNil(t, item.HarnessArtifactID)
				require.Equal(t, promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE, item.SurfaceState)
				require.Nil(t, item.ArtifactContent, "a personal finding is advisory — no fix body attached")
				byArtifact[*item.HarnessArtifactID] = item
			}
			neverUsed := byArtifact[globalDepID]
			require.NotNil(t, neverUsed)
			require.Contains(t, neverUsed.Slug, personA+":personal:personal-never-used:")
			require.Equal(t, promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_INSTRUCTION_INTENT, *neverUsed.Dimension)
			shadow := byArtifact[globalPrID]
			require.NotNil(t, shadow)
			require.Contains(t, shadow.Slug, personA+":personal:personal-shadows-committed:")
			require.Equal(t, promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_CONFLICT, *shadow.Dimension)
			require.Equal(t, ".claude/skills/pr/SKILL.md", shadow.Evidence.GetReferencePoint(), "points at the committed twin")

			// Evaluated: A's SKILL partition (its artifacts), B's HOOK partition (its artifact, no finding →
			// auto-close capable) and B's STALE SKILL partition (live before, no artifact now → auto-closes).
			// The unowned person's live partition is absent, and no PROJECT partition is evaluated (the
			// committed skill's body never fetched, so its PROJECT partition is body-incomplete).
			require.ElementsMatch(t, []dao.EvaluatedPartition{
				{Scope: promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PERSON, ScopeValue: personA, ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL},
				{Scope: promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PERSON, ScopeValue: personB, ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK},
				{Scope: promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PERSON, ScopeValue: personB, ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL},
			}, evaluated)
			return nil
		})

	require.NoError(t, h.engine.Run(context.Background(), hiOrgID,
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, hiExecID))
}

// TestHarnessImproveEngine_Run_PersonalLaneSkipsUnownedRepo: a repo that is nobody's primary repo issues
// NO personal-artifact or invocation read at all — an engineer's personal harness is read only by the one
// run that owns them, never by every repo they touch.
func TestHarnessImproveEngine_Run_PersonalLaneSkipsUnownedRepo(t *testing.T) {
	h := newHarnessImproveHarness(t)
	h.execution.EXPECT().GetByID(gomock.Any(), hiOrgID, hiExecID).Return(runningExec(), nil)
	h.expectEnrichOK() // programs ListPrimaryRepoPersons → no owners
	h.artifacts.EXPECT().ListVCSByRepo(gomock.Any(), hiOrgID, hiRepoID).Return(nil, nil)
	h.repos.EXPECT().GetByID(gomock.Any(), hiOrgID, hiRepoID).Return(&model.Repository{ID: hiRepoID}, nil)
	h.security.EXPECT().CountByFlagType(gomock.Any(), hiOrgID, gomock.Any(), gomock.Any()).Return(map[string]int{}, nil)
	// gomock fails the test if either personal read is issued.
	h.artifacts.EXPECT().ListPersonalByPersons(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	h.usage.EXPECT().ListInvocationsForPersons(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	h.execution.EXPECT().MarkTerminalIfRunning(gomock.Any(), hiOrgID, hiExecID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE, gomock.Any(), gomock.Any(), "").
		Return(true, nil)

	require.NoError(t, h.engine.Run(context.Background(), hiOrgID,
		taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_SCHEDULED, hiExecID))
}

func strPtr(s string) *string { return &s }

// desktopArtifact builds one live DESKTOP personal artifact for the gomock fixtures. repoID nil →
// USER_GLOBAL; set → REPO_LOCAL.
func desktopArtifact(id, personID string, repoID *string, artifactType commonv1.HarnessArtifactType, name, sourcePath, fileID string, createdAt time.Time) *model.HarnessArtifact {
	scope := commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_USER_GLOBAL
	if repoID != nil {
		scope = commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_REPO_LOCAL
	}
	return &model.HarnessArtifact{
		ID:               id,
		OrganizationID:   hiOrgID,
		PersonID:         strPtr(personID),
		IdeType:          commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		Scope:            scope,
		Type:             artifactType,
		Name:             name,
		RepositoryID:     repoID,
		SourcePath:       sourcePath,
		CurrentFileID:    fileID,
		OperationalState: commonv1.HarnessOperationalState_HARNESS_OPERATIONAL_STATE_ENABLED,
		ArtifactSource:   commonv1.HarnessArtifactSource_HARNESS_ARTIFACT_SOURCE_DESKTOP,
		CreatedAt:        createdAt,
	}
}
