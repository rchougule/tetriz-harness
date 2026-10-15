// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// fakeArtifactStore is an in-memory artifactStore that captures the content/evidence blobs
// attachGroundedFixes writes (keyed by artifact id), so a test can assert the fix body round-trips and
// the returned key is stamped on the row. Only the two Put methods attachGroundedFixes uses are
// meaningful; the run-blob methods satisfy the interface and are unused.
type fakeArtifactStore struct {
	contents map[string]*promptlibraryv1.ArtifactContent
	evidence map[string]*promptlibraryv1.ArtifactEvidence
}

func newFakeArtifactStore() *fakeArtifactStore {
	return &fakeArtifactStore{
		contents: map[string]*promptlibraryv1.ArtifactContent{},
		evidence: map[string]*promptlibraryv1.ArtifactEvidence{},
	}
}

// fakeContentKey / fakeEvidenceKey are deterministic opaque content refs the fake store returns, so
// the test can assert the ref from PutArtifact{Content,Evidence} is threaded onto the model. The real
// content store returns the artifact id (the content store key builders were dropped with s3store.go); the exact
// ref scheme is immaterial here — only that whatever the store returns round-trips onto the model.
func fakeContentKey(orgID, artifactID string) string  { return "content/" + orgID + "/" + artifactID }
func fakeEvidenceKey(orgID, artifactID string) string { return "evidence/" + orgID + "/" + artifactID }

func (f *fakeArtifactStore) PutRunInput(context.Context, string, string, *promptlibraryv1.AggregatedContext) (string, error) {
	return "", nil
}

func (f *fakeArtifactStore) PutRunOutput(context.Context, string, string, []byte) (string, error) {
	return "", nil
}

func (f *fakeArtifactStore) PutArtifactContent(_ context.Context, orgID, artifactID string, content *promptlibraryv1.ArtifactContent) (string, error) {
	f.contents[artifactID] = content
	return fakeContentKey(orgID, artifactID), nil
}

func (f *fakeArtifactStore) PutArtifactEvidence(_ context.Context, orgID, artifactID string, evidence *promptlibraryv1.ArtifactEvidence) (string, error) {
	f.evidence[artifactID] = evidence
	return fakeEvidenceKey(orgID, artifactID), nil
}

// TestAttachGroundedFixes proves the grounded-fix the content store writer: a finding whose slug carries a grounded
// fix gets its id MINTED (the id is DB-generated, empty here, and the content store key derives from it), its
// suggested body written to the content store as ArtifactContent (round-tripped through the fake store), and the
// fix's change summary routed to the evidence blob while preserving the detector's reference_point. A
// finding with no fix stays advisory — untouched, no minted id, no content.
func TestAttachGroundedFixes(t *testing.T) {
	const orgID = "11111111-1111-1111-1111-111111111111"
	store := newFakeArtifactStore()
	engine := &HarnessImproveEngine{store: store}

	finding := &model.PromptLibraryItem{
		OrganizationID: orgID,
		Slug:           "repo:conflict:dead-glob:abc123",
		SurfaceState:   promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE,
		Evidence:       &promptlibraryv1.EvidenceProjection{ReferencePoint: "glob matches nothing"},
	}
	advisory := &model.PromptLibraryItem{
		OrganizationID: orgID,
		Slug:           "repo:conflict:other:def456",
	}
	fix := HarnessFixResult{
		SuggestedContent: "paths:\n  - \"src/**/*.go\"\n",
		ChangeSummary:    "narrow the dead glob to the committed Go tree",
		CitedPaths:       []string{"src/main.go"},
	}

	require.NoError(t, engine.attachGroundedFixes(context.Background(), orgID,
		[]*model.PromptLibraryItem{finding, advisory},
		map[string]HarnessFixResult{finding.Slug: fix}))

	// The finding got a minted id and an the content store-backed ArtifactContent at the id-derived key.
	require.NotEmpty(t, finding.ID, "an empty id must be minted before the content store key is derived")
	require.NotNil(t, finding.ArtifactContent)
	assert.Equal(t, fakeContentKey(orgID, finding.ID), finding.ArtifactContent.GetKey())

	// The fix body round-trips through the fake store (Original == Current == suggested; cited paths carried).
	stored := store.contents[finding.ID]
	require.NotNil(t, stored, "the content blob must be written under the minted id")
	assert.Equal(t, fix.SuggestedContent, stored.GetOriginal())
	assert.Equal(t, fix.SuggestedContent, stored.GetCurrent())
	assert.Equal(t, fix.CitedPaths, stored.GetCitedPaths())

	// The change summary routes to the evidence blob, referenced from the projection, and the detector's
	// reference_point is preserved (attach only adds the content store ref, it does not overwrite the projection).
	require.NotNil(t, finding.Evidence.GetRef())
	assert.Equal(t, fakeEvidenceKey(orgID, finding.ID), finding.Evidence.GetRef().GetKey())
	require.NotNil(t, store.evidence[finding.ID])
	assert.Equal(t, fix.ChangeSummary, store.evidence[finding.ID].GetSummary())
	assert.Equal(t, "glob matches nothing", finding.Evidence.GetReferencePoint())

	// The advisory finding (no fix) is untouched — no minted id, no content.
	assert.Empty(t, advisory.ID)
	assert.Nil(t, advisory.ArtifactContent)
}

// TestAttachGroundedFixesEmptyIsNoOp: the wired flow always passes an empty grounded-fix map (no fix
// producer feeds Run yet), so attach must be a no-op — no id minted, no the content store write.
func TestAttachGroundedFixesEmptyIsNoOp(t *testing.T) {
	const orgID = "11111111-1111-1111-1111-111111111111"
	store := newFakeArtifactStore()
	engine := &HarnessImproveEngine{store: store}
	finding := &model.PromptLibraryItem{OrganizationID: orgID, Slug: "repo:conflict:dead-glob:abc123"}

	require.NoError(t, engine.attachGroundedFixes(context.Background(), orgID,
		[]*model.PromptLibraryItem{finding}, map[string]HarnessFixResult{}))

	assert.Empty(t, finding.ID)
	assert.Nil(t, finding.ArtifactContent)
	assert.Empty(t, store.contents)
}

// fakeHarnessFixGen is a hand-rolled harnessFixGenerator returning a fixed result — enough to drive
// produceFixes' LLM lane without the tetriz-py client.
type fakeHarnessFixGen struct{ result HarnessFixResult }

func (f fakeHarnessFixGen) GenerateHarnessFix(context.Context, string, string, HarnessFixInput) (HarnessFixResult, error) {
	return f.result, nil
}

// TestProduceFixes_DropsLLMFixWithUngroundedBodyPath proves the LLM fix producer grounds the generated
// BODY, not only its self-reported cited_paths: a suggested body carrying a path that grounds nowhere
// (an LLM/prompt-injection hallucination) is dropped to advisory even when cited_paths is empty, while
// a body whose paths all ground is accepted.
func TestProduceFixes_DropsLLMFixWithUngroundedBodyPath(t *testing.T) {
	const (
		artifactID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		repoID     = "33333333-3333-3333-3333-333333333333"
	)
	aid := artifactID
	artifact := &model.HarnessArtifact{ID: artifactID, SourcePath: "backend/app/CLAUDE.md"}
	agg := harnessImproveAggregate{
		committed: []*model.HarnessArtifact{artifact},
		bodies:    map[string]string{artifactID: "current body"},
		repoPaths: map[string]struct{}{"backend/app/util.go": {}},
	}
	dim := promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_INSTRUCTION_INTENT
	finding := &model.PromptLibraryItem{
		Dimension:         &dim,
		Slug:              instructionIntentSlug(repoID, instructionIntentFamilyTokenFloor, artifactID),
		HarnessArtifactID: &aid,
	}

	// A body citing a path that grounds NOWHERE, with an EMPTY cited list (the schema permits it).
	bogus := &HarnessImproveEngine{
		fix:         fakeHarnessFixGen{result: HarnessFixResult{SuggestedContent: "Follow the guide in docs/hallucinated.md now.", CitedPaths: nil}},
		detectorCfg: detectorSettingsFromConfig(nil),
	}
	fixes := bogus.produceFixes(context.Background(), "org", agg, []*model.PromptLibraryItem{finding})
	_, kept := fixes[finding.Slug]
	require.False(t, kept, "an LLM fix whose BODY carries an ungrounded path is dropped to advisory even with an empty cited list")

	// A body whose only path ref grounds (a committed sibling) is accepted.
	clean := &HarnessImproveEngine{
		fix:         fakeHarnessFixGen{result: HarnessFixResult{SuggestedContent: "Reuse the helper in [util](util.go).", CitedPaths: []string{"backend/app/util.go"}}},
		detectorCfg: detectorSettingsFromConfig(nil),
	}
	fixes = clean.produceFixes(context.Background(), "org", agg, []*model.PromptLibraryItem{finding})
	got, kept := fixes[finding.Slug]
	require.True(t, kept, "an LLM fix whose body paths all ground is accepted")
	require.Contains(t, got.SuggestedContent, "util.go")
}

// TestToDAOPartitions maps the gate's internal evaluated set to the dao input, field for field, and
// returns an empty (non-nil-safe) slice for no partitions.
func TestToDAOPartitions(t *testing.T) {
	evaluated := []evaluatedPartition{
		{Scope: promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, ScopeValue: "repo-1", ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE},
		{Scope: promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_ORG, ScopeValue: "", ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL},
	}
	got := toDAOPartitions(evaluated)
	require.Len(t, got, 2)
	assert.Equal(t, dao.EvaluatedPartition{Scope: promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, ScopeValue: "repo-1", ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE}, got[0])
	assert.Equal(t, dao.EvaluatedPartition{Scope: promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_ORG, ScopeValue: "", ArtifactType: promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL}, got[1])
	assert.Empty(t, toDAOPartitions(nil))
}
