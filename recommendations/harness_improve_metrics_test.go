// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// dimPtr returns a pointer to a dimension enum for a finding fixture; a nil dimension is passed
// literally in the table.
func dimPtr(dimension promptlibraryv1.HarnessFindingDimension) *promptlibraryv1.HarnessFindingDimension {
	return &dimension
}

// TestDimensionLabel covers every enum value plus a nil pointer — the label must never panic and
// UNSPECIFIED/nil both fold to "unspecified".
func TestDimensionLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		dimension *promptlibraryv1.HarnessFindingDimension
		want      string
	}{
		{"conflict", dimPtr(promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_CONFLICT), dimensionConflict},
		{"security", dimPtr(promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_SECURITY), dimensionSecurity},
		{"instruction_intent", dimPtr(promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_INSTRUCTION_INTENT), dimensionInstructionIntent},
		{"unspecified enum", dimPtr(promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_UNSPECIFIED), dimensionUnspecified},
		{"nil pointer", nil, dimensionUnspecified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, dimensionLabel(tt.dimension))
		})
	}
}

// TestPriorityToSeverityLabel covers every stamped rank plus the zero/unknown default — unlike
// priorityToInt (which defaults unknown to medium), an unrecognized rank folds to "unspecified".
func TestPriorityToSeverityLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		priority int32
		want     string
	}{
		{"critical", priorityCritical, severityCritical},
		{"high", priorityHigh, severityHigh},
		{"medium", priorityMedium, severityMedium},
		{"low", priorityLow, severityLow},
		{"zero unstamped", 0, severityUnspecified},
		{"unknown rank", 7, severityUnspecified},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, priorityToSeverityLabel(tt.priority))
		})
	}
}

// visibleFinding builds a VISIBLE finding fixture carrying a dimension + priority.
func visibleFinding(dimension *promptlibraryv1.HarnessFindingDimension, priority int32) *model.PromptLibraryItem {
	return &model.PromptLibraryItem{
		Dimension:    dimension,
		Priority:     priority,
		SurfaceState: promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE,
	}
}

// hiddenFinding builds a HIDDEN_FAILED_EVAL finding fixture — meterFindingsEmitted must skip it.
func hiddenFinding(dimension *promptlibraryv1.HarnessFindingDimension, priority int32) *model.PromptLibraryItem {
	return &model.PromptLibraryItem{
		Dimension:    dimension,
		Priority:     priority,
		SurfaceState: promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL,
	}
}

// TestMeterFindingsEmitted asserts the right findings_emitted_total{dimension,severity} cell moves for
// each VISIBLE finding and that a HIDDEN finding never increments (it is metered by gate_hidden_total).
// Deltas are captured before/after because the counter vec is process-global (seeded + shared).
func TestMeterFindingsEmitted(t *testing.T) {
	tests := []struct {
		name          string
		findings      []*model.PromptLibraryItem
		wantDimension string
		wantSeverity  string
		wantDelta     float64
	}{
		{
			name:          "visible conflict high increments its cell",
			findings:      []*model.PromptLibraryItem{visibleFinding(dimPtr(promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_CONFLICT), priorityHigh)},
			wantDimension: dimensionConflict,
			wantSeverity:  severityHigh,
			wantDelta:     1,
		},
		{
			name:          "visible security critical increments its cell",
			findings:      []*model.PromptLibraryItem{visibleFinding(dimPtr(promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_SECURITY), priorityCritical)},
			wantDimension: dimensionSecurity,
			wantSeverity:  severityCritical,
			wantDelta:     1,
		},
		{
			name:          "nil dimension + zero priority folds to unspecified/unspecified",
			findings:      []*model.PromptLibraryItem{visibleFinding(nil, 0)},
			wantDimension: dimensionUnspecified,
			wantSeverity:  severityUnspecified,
			wantDelta:     1,
		},
		{
			name: "hidden finding does not increment findings_emitted",
			findings: []*model.PromptLibraryItem{
				hiddenFinding(dimPtr(promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_CONFLICT), priorityLow),
			},
			wantDimension: dimensionConflict,
			wantSeverity:  severityLow,
			wantDelta:     0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := testutil.ToFloat64(harnessImproveFindingsEmittedTotal.WithLabelValues(tt.wantDimension, tt.wantSeverity))
			meterFindingsEmitted(tt.findings)
			after := testutil.ToFloat64(harnessImproveFindingsEmittedTotal.WithLabelValues(tt.wantDimension, tt.wantSeverity))
			assert.Equal(t, tt.wantDelta, after-before, "the (dimension, severity) cell delta")
		})
	}
}

// TestGateHiddenReason_MetersReason drives gateFindings through the suppression paths and meters the
// returned reasons via meterGateHidden — the real post-commit increment site — asserting the correct
// gate_hidden_total{reason} cell moves: dismissed wins over recently_hidden (including when BOTH match),
// and an unsuppressed finding increments neither. The gate seam (mocked suppression reads) is reused
// from harness_gate_test.go's helpers.
func TestGateHiddenReason_MetersReason(t *testing.T) {
	tests := []struct {
		name       string
		dismissed  []string
		hidden     []model.ArtifactSlugRef
		wantReason string // "" means neither cell should move
	}{
		{
			name:       "unsuppressed increments neither reason",
			wantReason: "",
		},
		{
			name:       "dismissed slug meters dismissed",
			dismissed:  []string{deadGlobFinding("art-1").Slug},
			wantReason: gateHiddenReasonDismissed,
		},
		{
			name:       "recent-hidden typed slug meters recently_hidden",
			hidden:     []model.ArtifactSlugRef{{ArtifactType: deadGlobFinding("art-1").ArtifactType, Slug: deadGlobFinding("art-1").Slug}},
			wantReason: gateHiddenReasonRecentlyHidden,
		},
		{
			// Both a dismissal AND a recent-hidden match the same slug: the precedence must attribute it to
			// dismissed only, so a reordering of the two branches is caught rather than passing silently.
			name:       "dismissed wins when both dismissed and recently-hidden match",
			dismissed:  []string{deadGlobFinding("art-1").Slug},
			hidden:     []model.ArtifactSlugRef{{ArtifactType: deadGlobFinding("art-1").ArtifactType, Slug: deadGlobFinding("art-1").Slug}},
			wantReason: gateHiddenReasonDismissed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine, lib := newGateEngine(t)
			expectSuppression(lib, tt.dismissed, tt.hidden)

			beforeDismissed := testutil.ToFloat64(harnessImproveGateHiddenTotal.WithLabelValues(gateHiddenReasonDismissed))
			beforeHidden := testutil.ToFloat64(harnessImproveGateHiddenTotal.WithLabelValues(gateHiddenReasonRecentlyHidden))

			item := deadGlobFinding("art-1")
			_, _, _, hiddenReasons, err := engine.gateFindings(context.Background(), gateOrgID, gateRepoID,
				promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, []*model.PromptLibraryItem{item},
				nil, map[string]struct{}{}, completeStatus())
			require.NoError(t, err)
			meterGateHidden(hiddenReasons)

			afterDismissed := testutil.ToFloat64(harnessImproveGateHiddenTotal.WithLabelValues(gateHiddenReasonDismissed))
			afterHidden := testutil.ToFloat64(harnessImproveGateHiddenTotal.WithLabelValues(gateHiddenReasonRecentlyHidden))

			wantDismissedDelta := 0.0
			wantHiddenDelta := 0.0
			switch tt.wantReason {
			case gateHiddenReasonDismissed:
				wantDismissedDelta = 1
			case gateHiddenReasonRecentlyHidden:
				wantHiddenDelta = 1
			}
			assert.Equal(t, wantDismissedDelta, afterDismissed-beforeDismissed, "dismissed reason delta")
			assert.Equal(t, wantHiddenDelta, afterHidden-beforeHidden, "recently_hidden reason delta")
		})
	}
}
