// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
)

// allFindingFamilies is every family a builder can emit — the completeness contract for the copy map.
// Keep in lockstep with the family consts in conflict.go / security.go / instruction_intent.go and the
// behavioral literal. A family added to a detector without a copy entry surfaces a bare finding, which
// TestFindingCopy_EveryFamilyHasCopy fails on.
var allFindingFamilies = []string{
	conflictFamilyDeadGlob,
	conflictFamilyBrokenPath,
	conflictFamilyDupID,
	conflictFamilyRootRestatement,
	conflictFamilyVendorSurface,
	securityFamilyPermissionDefaultMode,
	securityFamilyPermissionGrant,
	securityFamilyMcpProdPointed,
	securityFamilyMcpSprawl,
	securityFamilyCoverageGap,
	instructionIntentFamilyTokenFloor,
	instructionIntentFamilyTokPerLine,
	instructionIntentFamilyDirectiveShare,
	behavioralFamilyDeviation,
	personalFamilyNeverUsed,
	personalFamilyShadowsCommitted,
}

func TestFindingCopy_EveryFamilyHasCopy(t *testing.T) {
	for _, family := range allFindingFamilies {
		t.Run(family, func(t *testing.T) {
			fc := findingCopyFor(family, "the-ref", "review")
			assert.NotEmpty(t, fc.Summary, "family %q must have a non-empty summary", family)
			assert.NotEmpty(t, fc.RecommendedAction, "family %q must have a non-empty recommended action", family)
		})
	}
	// The map must not carry copy for a family no builder emits (a typo'd key that would never surface).
	for family := range findingCopyByFamily {
		assert.Contains(t, allFindingFamilies, family, "map has copy for unknown family %q", family)
	}
	assert.Len(t, findingCopyByFamily, len(allFindingFamilies), "map size must equal the family count")
}

func TestFindingCopy_RefInterpolated(t *testing.T) {
	// Non-behavioral families with a {ref} in their template must interpolate the reference_point.
	tests := []struct {
		name   string
		family string
		ref    string
	}{
		{"dead-glob", conflictFamilyDeadGlob, "nonexistent/**/*.rs"},
		{"permission-grant", securityFamilyPermissionGrant, "Bash(*)"},
		{"token-floor", instructionIntentFamilyTokenFloor, "42000 tokens"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := findingCopyFor(tt.family, tt.ref, "")
			assert.Contains(t, fc.Summary, tt.ref, "summary must contain the reference_point")
			assert.NotContains(t, fc.Summary, "{ref}", "placeholder must be substituted")
		})
	}
}

func TestFindingCopy_UnknownFamilyIsEmpty(t *testing.T) {
	fc := findingCopyFor("no-such-family", "x", "")
	assert.Empty(t, fc.Summary)
	assert.Empty(t, fc.RecommendedAction)
}

// TestHumanizeBehavioralBand_MatchesEvaluateBandsOutput is the coupling guard between the two files:
// humanizeBehavioralBand (finding_copy.go) parses the machine band string evaluateBands (behavioral.go)
// produces. TestHumanizeBehavioralBand above feeds hand-written literals; this feeds evaluateBands' REAL
// output, so a drift in evaluateBands' Sprintf format or metric labels — which would silently degrade
// every behavioral finding's copy to the generic no-clause fallback — fails HERE, not in production.
func TestHumanizeBehavioralBand_MatchesEvaluateBandsOutput(t *testing.T) {
	// Every metric out of band, so evaluateBands emits all three clauses.
	behavior := dao.InvokedSkillBehavior{
		Invocations:            10,
		CompletionRate:         0.0,
		AvgEffectiveContextUse: 0.75,
		ContextCoverageN:       5,
		AvgToolErrors:          99.0,
		ToolErrorCoverageN:     5,
	}
	band := behavioralBand{
		completion: &behavioralMetricRange{min: 0.40, max: 1.00},
		contextUse: &behavioralMetricRange{min: 0.10, max: 0.50},
		toolError:  &behavioralMetricRange{min: 0.00, max: 3.00},
	}
	deviations := evaluateBands(behavior, band)
	require.Len(t, deviations, 3, "all three metrics should be out of band")

	// The exact join behavioralDeviationFindings does before it becomes the finding's reference_point.
	got := humanizeBehavioralBand(strings.Join(deviations, "; "), "review")
	// Real metric clauses, not the no-clause generic fallback — proves the humanizer parsed
	// evaluateBands' actual format (regex + metric labels + units), which is the whole point of the guard.
	assert.Contains(t, got, "task-completion rate is 0%")
	assert.Contains(t, got, "context usage is 75%")
	assert.Contains(t, got, "tool-error count is 99")
	assert.NotContains(t, got, "based on how it's actually used", "must not degrade to the generic fallback")
	assert.NotContains(t, got, "outside [", "the machine band shape must be humanized away")
}

func TestHumanizeBehavioralBand(t *testing.T) {
	tests := []struct {
		name        string
		band        string
		intentClass string
		wantSubstr  []string
		wantAbsent  []string
	}{
		{
			// The exact 3-metric shape evaluateBands (behavioral.go) emits. Completion is a 0..1 rate
			// rendered as a percent (×100), tool-errors a raw count, context-use already a percent; all
			// with units and no ".00" padding. Keep in lockstep with evaluateBands' format strings.
			name:        "all three metrics: units added, decimals trimmed",
			band:        "completion 0.00 outside [0.40,1.00]; effective-context-use 75% outside [10%,50%]; tool-errors 99.00 outside [0.00,3.00]",
			intentClass: "review",
			wantSubstr: []string{
				"review skill",
				"task-completion rate is 0%", "40%–100%", // rate → percent, ×100
				"context usage is 75%", "10%–50%", // already percent
				"tool-error count is 99", "0–3", // count, no unit
			},
			wantAbsent: []string{
				"outside [", "{ref}", "effective-context-use",
				"0.00", "99.00", "1.00", "0.40", // no decimal padding anywhere
			},
		},
		{
			name:        "unknown metric falls back to its raw name, still trimmed",
			band:        "some-metric 5.00 outside [0.00,2.00]",
			intentClass: "",
			wantSubstr:  []string{"some-metric", "is 5,", "0–2"},
			wantAbsent:  []string{"5.00", "0.00", "2.00"},
		},
		{
			name:        "unparseable band degrades to a generic sentence",
			band:        "completely unparseable",
			intentClass: "review",
			wantSubstr:  []string{"review skill", "how it's actually used"},
			wantAbsent:  []string{"unparseable"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := humanizeBehavioralBand(tt.band, tt.intentClass)
			require.NotEmpty(t, got)
			for _, want := range tt.wantSubstr {
				assert.Containsf(t, got, want, "want %q in %q", want, got)
			}
			for _, absent := range tt.wantAbsent {
				assert.NotContainsf(t, got, absent, "did not want %q in %q", absent, got)
			}
		})
	}
}
