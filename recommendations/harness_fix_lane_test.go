// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"testing"

	"github.com/stretchr/testify/require"

	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// TestHarnessFixLane_Routing guards the fix-producer routing against the slug shape it depends on:
// every detector's REAL slug (built via the actual slug builders) must map to its intended lane.
// CONFLICT broken-path is deterministic; the single-file remove-line CONFLICT families (dead-glob /
// root-restatement / vendor-surface) and the INSTRUCTION_INTENT rewrites (token-floor / tok-per-line /
// directive-share / behavioral-deviation) are LLM. dup-id is ADVISORY — its duplicate spans multiple
// files, so no per-repo body rewrite resolves it. SECURITY families are ADVISORY here: the per-repo
// permission families anchor on the whole settings.json body (secret-density), so they are never
// egressed to the LLM fix lane, and the ORG-scoped families (coverage-gap, mcp) have their fixes
// produced from scratch in RunOrgScan's org fix path, not this per-repo lane. A builder that changed
// the slug's family segment, or a new detector family added without a routing decision, trips this test.
func TestHarnessFixLane_Routing(t *testing.T) {
	const (
		repoID     = "33333333-3333-3333-3333-333333333333"
		artifactID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		orgID      = "11111111-1111-1111-1111-111111111111"
		personID   = "aaaaaaaa-0000-0000-0000-00000000000a"
	)
	conflict := promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_CONFLICT
	instruction := promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_INSTRUCTION_INTENT
	security := promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_SECURITY

	tests := []struct {
		name string
		dim  promptlibraryv1.HarnessFindingDimension
		slug string
		want fixLane
	}{
		{"conflict broken-path → deterministic", conflict, conflictSlug(repoID, artifactID, conflictFamilyBrokenPath, "docs/OLD.md"), laneDeterministic},
		{"conflict dead-glob → llm", conflict, conflictSlug(repoID, artifactID, conflictFamilyDeadGlob, "src/**/*.ts"), laneLLM},
		{"conflict dup-id → advisory (multi-file duplicate no per-repo fix resolves)", conflict, conflictSlug(repoID, artifactID, conflictFamilyDupID, "GO-018"), laneAdvisory},
		{"conflict root-restatement → llm", conflict, conflictSlug(repoID, artifactID, conflictFamilyRootRestatement, "GO-018"), laneLLM},
		{"conflict vendor-surface → llm", conflict, conflictSlug(repoID, artifactID, conflictFamilyVendorSurface, ".codex/"), laneLLM},
		{"instruction token-floor → llm", instruction, instructionIntentSlug(repoID, instructionIntentFamilyTokenFloor, artifactID), laneLLM},
		{"instruction tok-per-line → llm", instruction, instructionIntentSlug(repoID, instructionIntentFamilyTokPerLine, artifactID), laneLLM},
		{"instruction directive-share → llm", instruction, instructionIntentSlug(repoID, instructionIntentFamilyDirectiveShare, artifactID), laneLLM},
		{"instruction behavioral-deviation → llm", instruction, behavioralDeviationSlug(repoID, artifactID, "mechanical"), laneLLM},
		{"security permission-grant → advisory (settings.json body not egressed to LLM)", security, securitySlug(repoID, securityFamilyPermissionGrant, artifactID, "allow Bash"), laneAdvisory},
		{"security permission-default-mode → advisory (settings.json body not egressed to LLM)", security, securitySlug(repoID, securityFamilyPermissionDefaultMode, artifactID, "bypassPermissions"), laneAdvisory},
		{"org security coverage-gap → advisory (org path)", security, orgSecuritySlug(securityFamilyCoverageGap, orgID), laneAdvisory},
		{"org security mcp-sprawl → advisory (org path)", security, orgSecuritySlug(securityFamilyMcpSprawl, "server"), laneAdvisory},
		{"org security mcp-prod-pointed → advisory (org path)", security, orgSecuritySlug(securityFamilyMcpProdPointed, "server"), laneAdvisory},
		{"personal never-used → advisory (engineer's own artifact, nothing to rewrite)", instruction, personalSlug(personID, personalFamilyNeverUsed, artifactID), laneAdvisory},
		{"personal shadows-committed → advisory (personal body never egressed, no enforce path to a machine)", conflict, personalSlug(personID, personalFamilyShadowsCommitted, artifactID), laneAdvisory},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dim := tt.dim
			finding := &model.PromptLibraryItem{Dimension: &dim, Slug: tt.slug}
			require.Equal(t, tt.want, harnessFixLane(finding))
		})
	}

	// A nil dimension is never routed to a paid/deterministic fix.
	require.Equal(t, laneAdvisory, harnessFixLane(&model.PromptLibraryItem{
		Slug: conflictSlug(repoID, artifactID, conflictFamilyBrokenPath, "docs/OLD.md"),
	}), "a finding with no dimension routes to advisory")

	// Completeness: every finding family the copy registry knows about (allFindingFamilies, the same list
	// TestFindingCopy_EveryFamilyHasCopy guards) must have a routing case above — so a new detector family
	// can't be added to one list without a fix-lane decision here. This is the forcing function that
	// couples the two hand-maintained family lists; Go can't enumerate the family consts, so a family
	// present in NEITHER list is the only remaining unguarded gap.
	covered := map[string]bool{}
	for _, tt := range tests {
		covered[slugFamily(tt.slug)] = true
	}
	for _, family := range allFindingFamilies {
		require.Truef(t, covered[family], "family %q has no harnessFixLane routing case — add one deciding its lane", family)
	}
}
