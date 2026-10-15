// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

const (
	personalOrgID  = "11111111-1111-1111-1111-111111111111"
	personalRepoID = "33333333-3333-3333-3333-333333333333"
	personalExecID = "22222222-2222-2222-2222-222222222222"
	personA        = "aaaaaaaa-0000-0000-0000-00000000000a"
	personB        = "bbbbbbbb-0000-0000-0000-00000000000b"
)

var personalWindowEnd = time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)

// personalArtifact builds one live DESKTOP personal artifact. repoID nil → USER_GLOBAL; set → REPO_LOCAL.
// createdAt defaults to well past the never-used min-age unless overridden by the caller.
func personalArtifact(id, personID string, repoID *string, artifactType commonv1.HarnessArtifactType, name, sourcePath, fileID string) *model.HarnessArtifact {
	scope := commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_USER_GLOBAL
	if repoID != nil {
		scope = commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_REPO_LOCAL
	}
	pid := personID
	return &model.HarnessArtifact{
		ID:               id,
		OrganizationID:   personalOrgID,
		PersonID:         &pid,
		IdeType:          commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		Scope:            scope,
		Type:             artifactType,
		Name:             name,
		RepositoryID:     repoID,
		SourcePath:       sourcePath,
		CurrentFileID:    fileID,
		OperationalState: commonv1.HarnessOperationalState_HARNESS_OPERATIONAL_STATE_ENABLED,
		ArtifactSource:   commonv1.HarnessArtifactSource_HARNESS_ARTIFACT_SOURCE_DESKTOP,
		CreatedAt:        personalWindowEnd.AddDate(0, 0, -(personalNeverUsedMinAgeDays + 10)),
	}
}

func committedArtifact(id string, artifactType commonv1.HarnessArtifactType, name, sourcePath, fileID string) *model.HarnessArtifact {
	repoID := personalRepoID
	return &model.HarnessArtifact{
		ID:             id,
		OrganizationID: personalOrgID,
		IdeType:        commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		Scope:          commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED,
		Type:           artifactType,
		Name:           name,
		RepositoryID:   &repoID,
		SourcePath:     sourcePath,
		CurrentFileID:  fileID,
		ArtifactSource: commonv1.HarnessArtifactSource_HARNESS_ARTIFACT_SOURCE_VCS,
	}
}

func familiesOf(findings []*model.PromptLibraryItem) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.ScopeValue+"|"+slugFamily(f.Slug)+"|"+f.ArtifactType.String())
	}
	sort.Strings(out)
	return out
}

func TestPersonalFindings(t *testing.T) {
	skill := commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL
	hook := commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_HOOK
	claudeMD := commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD
	otherRepo := "44444444-4444-4444-4444-444444444444"
	repoID := personalRepoID

	tests := []struct {
		name        string
		committed   []*model.HarnessArtifact
		artifacts   []*model.HarnessArtifact
		invocations map[string]int64
		mutate      func([]*model.HarnessArtifact)
		want        []string // "<person>|<family>|<type>", sorted
	}{
		{
			name: "global skill with zero invocations is never-used; a global CLAUDE.md is not a never-used subject",
			artifacts: []*model.HarnessArtifact{
				personalArtifact("s1", personA, nil, skill, "/deploy", ".claude/skills/deploy/SKILL.md", "f1"),
				personalArtifact("c1", personA, nil, claudeMD, "CLAUDE.md", ".claude/CLAUDE.md", "f2"),
			},
			invocations: map[string]int64{"s1": 0, "c1": 0},
			want:        []string{personA + "|" + personalFamilyNeverUsed + "|ARTIFACT_TYPE_SKILL"},
		},
		{
			name: "an invoked skill, a disabled hook, and a too-young skill are not never-used",
			artifacts: []*model.HarnessArtifact{
				personalArtifact("used", personA, nil, skill, "/pr", ".claude/skills/pr/SKILL.md", "f1"),
				personalArtifact("off", personA, nil, hook, "lint", ".claude/hooks/lint.sh", "f2"),
				personalArtifact("young", personA, nil, skill, "/new", ".claude/skills/new/SKILL.md", "f3"),
			},
			invocations: map[string]int64{"used": 3, "off": 0, "young": 0},
			mutate: func(a []*model.HarnessArtifact) {
				a[1].OperationalState = commonv1.HarnessOperationalState_HARNESS_OPERATIONAL_STATE_DISABLED
				a[2].CreatedAt = personalWindowEnd.AddDate(0, 0, -(personalNeverUsedMinAgeDays - 1))
			},
			want: []string{},
		},
		{
			name: "global skill sharing a committed skill's name with a different body shadows it; identical body does not",
			committed: []*model.HarnessArtifact{
				committedArtifact("c-pr", skill, "/pr", ".claude/skills/pr/SKILL.md", "team-pr"),
				committedArtifact("c-deploy", skill, "/deploy", ".claude/skills/deploy/SKILL.md", "team-deploy"),
			},
			artifacts: []*model.HarnessArtifact{
				personalArtifact("p-pr", personA, nil, skill, "PR", "~/.claude/skills/pr/SKILL.md", "mine-pr"),                  // canonicalizes to "pr", differs
				personalArtifact("p-deploy", personA, nil, skill, "/deploy", "~/.claude/skills/deploy/SKILL.md", "team-deploy"), // same body
			},
			invocations: map[string]int64{"p-pr": 5, "p-deploy": 5},
			want:        []string{personA + "|" + personalFamilyShadowsCommitted + "|ARTIFACT_TYPE_SKILL"},
		},
		{
			name: "repo-local skill is conflict-checked only in its own repo",
			committed: []*model.HarnessArtifact{
				committedArtifact("c-pr", skill, "/pr", ".claude/skills/pr/SKILL.md", "team-pr"),
			},
			artifacts: []*model.HarnessArtifact{
				personalArtifact("here", personA, &repoID, skill, "/pr", ".claude/skills/pr/SKILL.md", "mine-1"),
				personalArtifact("there", personA, &otherRepo, skill, "/pr", ".claude/skills/pr/SKILL.md", "mine-2"),
			},
			invocations: map[string]int64{"here": 1, "there": 1},
			want:        []string{personA + "|" + personalFamilyShadowsCommitted + "|ARTIFACT_TYPE_SKILL"},
		},
		{
			name: "two engineers' findings carry their own person as scope value",
			artifacts: []*model.HarnessArtifact{
				personalArtifact("a1", personA, nil, skill, "/a", ".claude/skills/a/SKILL.md", "fa"),
				personalArtifact("b1", personB, nil, hook, "b", ".claude/hooks/b.sh", "fb"),
			},
			invocations: map[string]int64{"a1": 0, "b1": 0},
			want: []string{
				personA + "|" + personalFamilyNeverUsed + "|ARTIFACT_TYPE_SKILL",
				personB + "|" + personalFamilyNeverUsed + "|ARTIFACT_TYPE_HOOK",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.mutate != nil {
				tt.mutate(tt.artifacts)
			}
			aggregate := harnessImproveAggregate{
				committed: tt.committed,
				personal: personalAggregate{
					persons:     []string{personA, personB},
					artifacts:   tt.artifacts,
					invocations: tt.invocations,
					windowEnd:   personalWindowEnd,
					windowDays:  90,
				},
			}
			got := personalFindings(personalOrgID, personalRepoID, personalExecID, aggregate, nil)
			require.Equal(t, tt.want, familiesOf(got))
			for _, f := range got {
				require.Equal(t, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PERSON, f.Scope)
				require.NotNil(t, f.HarnessArtifactID, "attributed to the desktop row")
				require.NotNil(t, f.RecommendationSource)
				require.Equal(t, promptlibraryv1.ArtifactRecommendationSource_ARTIFACT_RECOMMENDATION_SOURCE_DETECTED, *f.RecommendationSource)
				require.NotZero(t, f.Priority)
				require.NotEmpty(t, f.Evidence.GetSummary(), "every personal family has user-facing copy")
			}
		})
	}
}

func TestFoldPersonalInvocations(t *testing.T) {
	repoID := personalRepoID
	global := personalArtifact("g", personA, nil, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL, "/g", ".claude/skills/g/SKILL.md", "f")
	local := personalArtifact("l", personA, &repoID, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL, "/l", ".claude/skills/l/SKILL.md", "f")
	tests := []struct {
		name      string
		summaries []dao.PersonalInvocationSummary
		want      map[string]int64
	}{
		{
			name: "resolved rows credit by artifact id",
			summaries: []dao.PersonalInvocationSummary{
				{PersonID: personA, ArtifactID: "g", Invocations: 2},
				{PersonID: personA, ArtifactID: "g", Invocations: 3},
			},
			want: map[string]int64{"g": 5, "l": 0},
		},
		{
			name: "unresolved rows credit by inline identity, scope-aware",
			summaries: []dao.PersonalInvocationSummary{
				{PersonID: personA, IdeType: commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, SourcePath: ".claude/skills/g/SKILL.md", UserGlobal: true, Invocations: 4},
				// Same path but repo-scoped: matches nothing (g is global, l has another path).
				{PersonID: personA, IdeType: commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, SourcePath: ".claude/skills/g/SKILL.md", UserGlobal: false, Invocations: 9},
				// Another person's identical identity never credits this person's artifact.
				{PersonID: personB, IdeType: commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, SourcePath: ".claude/skills/l/SKILL.md", UserGlobal: false, Invocations: 7},
			},
			want: map[string]int64{"g": 4, "l": 0},
		},
		{
			name:      "every artifact has a key even with no usage at all",
			summaries: nil,
			want:      map[string]int64{"g": 0, "l": 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, foldPersonalInvocations([]*model.HarnessArtifact{global, local}, tt.summaries))
		})
	}
}

func TestPersonalEvaluationStatus(t *testing.T) {
	skill := commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL
	person := func(id string) evaluatedPartition {
		return evaluatedPartition{Scope: promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PERSON, ScopeValue: id}
	}
	withType := func(p evaluatedPartition, t promptlibraryv1.ArtifactType) evaluatedPartition {
		p.ArtifactType = t
		return p
	}
	tests := []struct {
		name      string
		personal  personalAggregate
		wantByPer map[string][]evaluatedPartition
	}{
		{
			name: "one partition per (person, lens-subject type); an instruction file contributes none",
			personal: personalAggregate{
				persons: []string{personA, personB},
				artifacts: []*model.HarnessArtifact{
					personalArtifact("a1", personA, nil, skill, "/a", "p", "f"),
					personalArtifact("a2", personA, nil, skill, "/b", "q", "f"),
					personalArtifact("a3", personA, nil, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD, "CLAUDE.md", "CLAUDE.md", "f"),
				},
			},
			wantByPer: map[string][]evaluatedPartition{
				personA: {withType(person(personA), promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL)},
				personB: {},
			},
		},
		{
			name: "a live PERSON partition of an owned person joins the evaluated set (vanished auto-close); another person's does not",
			personal: personalAggregate{
				persons: []string{personA},
				liveDetectedPartitions: []evaluatedPartition{
					withType(person(personA), promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK),
					withType(person(personB), promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK),
				},
			},
			wantByPer: map[string][]evaluatedPartition{
				personA: {withType(person(personA), promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK)},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := personalEvaluationStatus(tt.personal)
			require.Len(t, got, len(tt.wantByPer))
			for personID, want := range tt.wantByPer {
				status, ok := got[personID]
				require.True(t, ok, "person %s has a status", personID)
				require.True(t, status.pathLensComplete && status.behavioralComplete, "no degradable lens feeds the personal lane")
				require.Equal(t, want, status.evaluatedPartitions())
			}
		})
	}
}
