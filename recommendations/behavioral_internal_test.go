// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"testing"

	"github.com/stretchr/testify/require"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/config"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

func TestCanonicalArtifactName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "pr", "pr"},
		{"leading slash command", "/deploy", "deploy"},
		{"leading dollar skill", "$review", "review"},
		// A plugin namespace is NOT stripped: an external plugin skill is a distinct artifact
		// from a local committed one of the same short name, so it must not fold into it.
		{"plugin namespace preserved", "plugin:acme:deploy", "plugin:acme:deploy"},
		{"plugin single colon preserved", "plugin:deploy", "plugin:deploy"},
		{"plugin lowercased only", "Plugin:Acme:Deploy", "plugin:acme:deploy"},
		{"trim and lowercase", "  PR-Review  ", "pr-review"},
		// The sigil is still stripped ahead of the (preserved) plugin prefix.
		{"slash then plugin preserved", "/plugin:acme:foo", "plugin:acme:foo"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, canonicalArtifactName(tt.in))
		})
	}
}

func TestResolveCanonicalName_Alias(t *testing.T) {
	aliases := map[string]string{
		"oldname": "newname",
		// A non-canonical alias TARGET (sigil + mixed case) must still resolve to the key the committed
		// side canonicalizes to, or the alias would never match anything.
		"oldname2": "/NewName",
	}
	require.Equal(t, "newname", resolveCanonicalName("OldName", aliases), "alias applies after canonicalization")
	require.Equal(t, "newname", resolveCanonicalName("OldName2", aliases), "alias target is itself canonicalized")
	require.Equal(t, "pr", resolveCanonicalName("/pr", aliases), "no alias → canonical name")
}

func TestDemotedCollisionNames(t *testing.T) {
	tests := []struct {
		name     string
		contents []dao.VCSArtifactContent
		want     []string
	}{
		{
			name: "same name two repos two hashes is demoted",
			contents: []dao.VCSArtifactContent{
				{Name: "pr", RepositoryID: "r1", ContentHash: "h1"},
				{Name: "pr", RepositoryID: "r2", ContentHash: "h2"},
			},
			want: []string{"pr"},
		},
		{
			name: "same name same content across many repos is not demoted",
			contents: []dao.VCSArtifactContent{
				{Name: "pr", RepositoryID: "r1", ContentHash: "h1"},
				{Name: "pr", RepositoryID: "r2", ContentHash: "h1"},
				{Name: "pr", RepositoryID: "r3", ContentHash: "h1"},
			},
			want: nil,
		},
		{
			name: "two distinct contents in ONE repo is demoted",
			contents: []dao.VCSArtifactContent{
				{Name: "pr", RepositoryID: "r1", ContentHash: "h1"},
				{Name: "pr", RepositoryID: "r1", ContentHash: "h2"},
			},
			want: []string{"pr"},
		},
		{
			name: "distinct names never collide",
			contents: []dao.VCSArtifactContent{
				{Name: "pr", RepositoryID: "r1", ContentHash: "h1"},
				{Name: "deploy", RepositoryID: "r2", ContentHash: "h2"},
			},
			want: nil,
		},
		{
			name: "collision detected across canonicalized names",
			contents: []dao.VCSArtifactContent{
				{Name: "/pr", RepositoryID: "r1", ContentHash: "h1"},
				{Name: "pr", RepositoryID: "r2", ContentHash: "h2"},
			},
			want: []string{"pr"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			demoted := demotedCollisionNames(tt.contents, nil)
			require.Len(t, demoted, len(tt.want))
			for _, name := range tt.want {
				_, ok := demoted[name]
				require.Truef(t, ok, "expected %q demoted", name)
			}
		})
	}
}

// behaviorFinding builds a one-artifact aggregate + intent and returns the findings, the shared
// fixture for the band-comparison cases.
func behaviorFinding(t *testing.T, behavior dao.InvokedSkillBehavior, class string, confidence float64, demoted, matched bool) []*model.PromptLibraryItem {
	t.Helper()
	artifact := &model.HarnessArtifact{ID: "a1", Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL, Name: "pr"}
	agg := &harnessImproveAggregate{
		committed:            []*model.HarnessArtifact{artifact},
		behavioralByName:     map[string]dao.InvokedSkillBehavior{},
		nameCollisionDemoted: map[string]struct{}{},
	}
	if matched {
		agg.behavioralByName["pr"] = behavior
	}
	if demoted {
		agg.nameCollisionDemoted["pr"] = struct{}{}
	}
	intent := map[string]classifiedIntent{"a1": {class: class, confidence: confidence}}
	// Drive the pure lens on the Go-seed defaults (the bands + aliases the engine would resolve
	// from a nil config).
	return behavioralDeviationFindings("org", "repo", "exec", agg, intent, behavioralBandsByClass, behavioralNameMatchAliases)
}

func TestBehavioralDeviationFindings_BandComparison(t *testing.T) {
	// mechanical band: completion [0.5,1.0], contextUse [0.0,0.80] (0-1 fraction), toolError [0,3].
	tests := []struct {
		name        string
		behavior    dao.InvokedSkillBehavior
		wantFinding bool
		wantRefHint string
	}{
		{
			name:        "completion below band fires",
			behavior:    dao.InvokedSkillBehavior{Invocations: 10, CompletionRate: 0.30},
			wantFinding: true,
			wantRefHint: "completion",
		},
		{
			name:        "all within band is silent",
			behavior:    dao.InvokedSkillBehavior{Invocations: 10, CompletionRate: 0.90},
			wantFinding: false,
		},
		{
			// 0.95 (a 0-1 fraction) is above the mechanical contextUse max of 0.80 — proving the lens
			// fires on the 0.0..1.0 scale, the effectiveContextUsagePercent proto contract.
			name:        "context use above band fires",
			behavior:    dao.InvokedSkillBehavior{Invocations: 10, CompletionRate: 0.90, AvgEffectiveContextUse: 0.95, ContextCoverageN: 4},
			wantFinding: true,
			wantRefHint: "effective-context-use",
		},
		{
			name:        "tool errors above band fires",
			behavior:    dao.InvokedSkillBehavior{Invocations: 10, CompletionRate: 0.90, AvgToolErrors: 9, ToolErrorCoverageN: 4},
			wantFinding: true,
			wantRefHint: "tool-errors",
		},
		{
			name:        "out-of-band context with zero coverage is not scored",
			behavior:    dao.InvokedSkillBehavior{Invocations: 10, CompletionRate: 0.90, AvgEffectiveContextUse: 0.99, ContextCoverageN: 0},
			wantFinding: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := behaviorFinding(t, tt.behavior, "mechanical", 0.9, false, true)
			if !tt.wantFinding {
				require.Empty(t, findings)
				return
			}
			require.Len(t, findings, 1)
			require.Contains(t, findings[0].Evidence.GetReferencePoint(), tt.wantRefHint)
		})
	}
}

func TestBehavioralDeviationFindings_InsufficientData(t *testing.T) {
	// A demoted name yields no finding even when out of band.
	require.Empty(t, behaviorFinding(t, dao.InvokedSkillBehavior{Invocations: 10, CompletionRate: 0.1}, "mechanical", 0.9, true, true))
	// No behavioral row (unmatched) yields no finding.
	require.Empty(t, behaviorFinding(t, dao.InvokedSkillBehavior{}, "mechanical", 0.9, false, false))
	// An unknown intent class has no band → no finding.
	require.Empty(t, behaviorFinding(t, dao.InvokedSkillBehavior{Invocations: 10, CompletionRate: 0.1}, "made-up", 0.9, false, true))
}

func TestBehavioralDeviationFindings_Fields(t *testing.T) {
	findings := behaviorFinding(t, dao.InvokedSkillBehavior{Invocations: 10, CompletionRate: 0.30}, "mechanical", 0.83, false, true)
	require.Len(t, findings, 1)
	finding := findings[0]

	require.NotNil(t, finding.Dimension)
	require.Equal(t, promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_INSTRUCTION_INTENT, *finding.Dimension)
	require.Equal(t, priorityMedium, finding.Priority)
	require.Equal(t, float32(0.83), finding.ConfidenceScore, "carries the classifier confidence, not 1.0")
	require.Equal(t, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, finding.Scope)
	require.Equal(t, "repo", finding.ScopeValue)
	require.Equal(t, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL, finding.ArtifactType)
	require.NotNil(t, finding.HarnessArtifactID)
	require.Equal(t, "a1", *finding.HarnessArtifactID)
	require.Equal(t, promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE, finding.SurfaceState)
	require.NotNil(t, finding.RecommendationSource)
	require.Equal(t, promptlibraryv1.ArtifactRecommendationSource_ARTIFACT_RECOMMENDATION_SOURCE_DETECTED, *finding.RecommendationSource)
	require.Equal(t, "mechanical", finding.Evidence.GetIntentClass())
	require.Contains(t, finding.Slug, "repo:instruction-intent:behavioral-deviation:")

	// Slug converges per (artifact, class): a second build yields the identical slug.
	again := behaviorFinding(t, dao.InvokedSkillBehavior{Invocations: 10, CompletionRate: 0.30}, "mechanical", 0.5, false, true)
	require.Equal(t, finding.Slug, again[0].Slug)
	// A different class yields a different slug.
	other := behaviorFinding(t, dao.InvokedSkillBehavior{Invocations: 10, CompletionRate: 0.1, AvgToolErrors: 9, ToolErrorCoverageN: 4}, "gate", 0.9, false, true)
	require.NotEqual(t, finding.Slug, other[0].Slug)
}

// TestDetectorSettingsFromConfig_BandsDriveFindings proves the detector scores against the CONFIG
// bands, not the package-const seeds: the same behavior is silent under the Go-seed mechanical band
// [0.5,1.0] but fires under a config band that tightens it to [0.7,1.0]. This is the wiring guarantee
// FIX 2 adds — a calibrated YAML value actually reaches the lens.
func TestDetectorSettingsFromConfig_BandsDriveFindings(t *testing.T) {
	artifact := &model.HarnessArtifact{ID: "a1", Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL, Name: "pr"}
	agg := &harnessImproveAggregate{
		committed:            []*model.HarnessArtifact{artifact},
		behavioralByName:     map[string]dao.InvokedSkillBehavior{"pr": {Invocations: 10, CompletionRate: 0.6}},
		nameCollisionDemoted: map[string]struct{}{},
	}
	intent := map[string]classifiedIntent{"a1": {class: "mechanical", confidence: 0.9}}

	// 0.6 sits INSIDE the Go-seed mechanical completion band [0.5,1.0] → silent.
	def := defaultDetectorConfig()
	require.Empty(t, behavioralDeviationFindings("org", "repo", "exec", agg, intent, def.bands, def.aliases),
		"0.6 is within the default mechanical completion band")

	// A config tightening the mechanical band to [0.7,1.0] must now FIRE on the same behavior.
	tightened := detectorSettingsFromConfig(&config.DetectorConfig{
		ClassifyConfidenceFloor: 0.5,
		BehavioralWindowDays:    90,
		BehavioralBands: map[string]*config.BehavioralBand{
			"mechanical": {Completion: &config.MetricRange{Min: 0.7, Max: 1.0}},
		},
	})
	findings := behavioralDeviationFindings("org", "repo", "exec", agg, intent, tightened.bands, tightened.aliases)
	require.Len(t, findings, 1, "0.6 is below the config-tightened mechanical band [0.7,1.0]")
	require.Contains(t, findings[0].Evidence.GetReferencePoint(), "completion")
}

// TestDetectorSettingsFromConfig_NilFallsBackToDefaults: a nil/empty config yields the Go-seed
// defaults, so a caller that does not tune calibration still runs the built-in bands/floor/window.
func TestDetectorSettingsFromConfig_NilFallsBackToDefaults(t *testing.T) {
	require.Equal(t, defaultDetectorConfig(), detectorSettingsFromConfig(nil))
	require.Equal(t, defaultDetectorConfig(), detectorSettingsFromConfig(&config.DetectorConfig{}),
		"an empty config (no bands) also falls back to defaults")
}

// TestDetectorSettingsFromConfig_PartialConfigKeepsUnsetSeeds: a config that sets only some fields
// (and no bands) applies exactly those fields and keeps every other seed. A caller tuning one marker or
// cap must not have it dropped because it left the behavioral bands unset.
func TestDetectorSettingsFromConfig_PartialConfigKeepsUnsetSeeds(t *testing.T) {
	cfg := DefaultDetectorConfig()
	cfg.RuleLedgerBasename = "rules.md"
	cfg.MaxFixGenPerRun = 3
	cfg.VendorSurfaceMarkers = []string{".windsurfrules"}

	want := defaultDetectorConfig()
	want.ruleLedgerBasename = "rules.md"
	want.maxFixGen = 3
	want.vendorMarkers = []string{".windsurfrules"}
	require.Equal(t, want, detectorSettingsFromConfig(&cfg))
}

// TestDetectorSettingsFromConfig_PublicDefaultsResolveToSeeds: the public DefaultDetectorConfig resolves
// to exactly the built-in settings, so passing it changes nothing against passing nil.
func TestDetectorSettingsFromConfig_PublicDefaultsResolveToSeeds(t *testing.T) {
	cfg := DefaultDetectorConfig()
	require.Equal(t, defaultDetectorConfig(), detectorSettingsFromConfig(&cfg))
}

func TestMergeInvokedSkillBehavior_WeightedMeans(t *testing.T) {
	merged := mergeInvokedSkillBehavior(
		dao.InvokedSkillBehavior{Invocations: 2, CompletionRate: 1.0, AvgEffectiveContextUse: 10, ContextCoverageN: 2, AvgToolErrors: 4, ToolErrorCoverageN: 1},
		dao.InvokedSkillBehavior{Invocations: 2, CompletionRate: 0.0, AvgEffectiveContextUse: 30, ContextCoverageN: 2, AvgToolErrors: 0, ToolErrorCoverageN: 0},
	)
	require.Equal(t, 4, merged.Invocations)
	require.InDelta(t, 0.5, merged.CompletionRate, 1e-9, "invocation-weighted mean of 1.0 and 0.0")
	require.Equal(t, 4, merged.ContextCoverageN)
	require.InDelta(t, 20.0, merged.AvgEffectiveContextUse, 1e-9, "coverage-weighted mean of 10 and 30")
	require.Equal(t, 1, merged.ToolErrorCoverageN)
	require.InDelta(t, 4.0, merged.AvgToolErrors, 1e-9, "only the covered row contributes")
}
