// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// Local ids for the internal-package detector tests (the external-package harness constants live in
// the promptlibrary_test package and are not reachable from here).
const (
	iiOrgID  = "11111111-1111-1111-1111-111111111111"
	iiExecID = "22222222-2222-2222-2222-222222222222"
	iiRepoID = "33333333-3333-3333-3333-333333333333"
)

func TestEstimateTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    int
	}{
		{name: "empty", content: "", want: 0},
		{name: "under four chars", content: "abc", want: 0},
		{name: "exactly four chars", content: "abcd", want: 1},
		{name: "chars-over-four", content: strings.Repeat("x", 16712), want: 4178},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, estimateTokens(tt.content))
		})
	}
}

func TestCountContentLines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    int
	}{
		{name: "empty floors at one", content: "", want: 1},
		{name: "all blank floors at one", content: "\n  \n\t\n", want: 1},
		{name: "counts only non-blank", content: "a\n\nb\n   \nc", want: 3},
		{name: "single line", content: "only", want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, countContentLines(tt.content))
		})
	}
}

func TestDirectiveShare(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		content      string
		wantShare    float64
		wantReliable bool
	}{
		{name: "no content lines", content: "\n\n", wantShare: 0, wantReliable: true},
		{
			name:         "all directive bullets",
			content:      "- one\n- two\n* three\n+ four",
			wantShare:    1.0,
			wantReliable: true,
		},
		{
			name:         "headings are content but not directives",
			content:      "# Heading\n- bullet",
			wantShare:    0.5,
			wantReliable: true,
		},
		{
			name:         "ordered list markers count",
			content:      "1. first\n2) second\nprose line",
			wantShare:    2.0 / 3.0,
			wantReliable: true,
		},
		{
			name: "fenced code is not content",
			// Two content lines (the two bullets); the fenced block and its delimiters are excluded.
			content:      "- a\n```\n- not counted\ncode line\n```\n- b",
			wantShare:    1.0,
			wantReliable: true,
		},
		{
			name:         "prose only is zero",
			content:      "just some prose\nmore prose",
			wantShare:    0,
			wantReliable: true,
		},
		{
			name:         "digit without delimiter is not a marker",
			content:      "3 apples\n- real bullet",
			wantShare:    0.5,
			wantReliable: true,
		},
		{
			name: "unbalanced fence is unreliable",
			// The opening ``` is never closed, so the remainder is swallowed and the share cannot be
			// trusted: reliable is false.
			content:      "- a\n```\ncode line",
			wantShare:    1.0,
			wantReliable: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			share, reliable := directiveShare(tt.content)
			require.InDelta(t, tt.wantShare, share, 1e-9)
			require.Equal(t, tt.wantReliable, reliable)
		})
	}
}

func TestPercentile75NearestRank(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		values []float64
		want   float64
	}{
		// n=4 → rank = ceil(3.0) = 3 → values[2] after sort.
		{name: "four values", values: []float64{10, 20, 30, 40}, want: 30},
		// n=5 → rank = ceil(3.75) = 4 → values[3] after sort.
		{name: "five values unsorted", values: []float64{5, 1, 4, 2, 3}, want: 4},
		{name: "single value", values: []float64{7}, want: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.InDelta(t, tt.want, percentile75NearestRank(tt.values), 1e-9)
		})
	}
}

func TestPercentile75NearestRank_DoesNotMutateInput(t *testing.T) {
	t.Parallel()
	values := []float64{3, 1, 2}
	_ = percentile75NearestRank(values)
	require.Equal(t, []float64{3, 1, 2}, values, "input order must be preserved")
}

func TestIsRootAlwaysLoadedInstruction(t *testing.T) {
	t.Parallel()
	artifact := func(sourcePath string, artifactType commonv1.HarnessArtifactType) *model.HarnessArtifact {
		return &model.HarnessArtifact{Type: artifactType, SourcePath: sourcePath}
	}
	tests := []struct {
		name     string
		artifact *model.HarnessArtifact
		want     bool
	}{
		{name: "root CLAUDE.md", artifact: artifact("CLAUDE.md", commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD), want: true},
		{name: "root AGENTS.md (Codex convention)", artifact: artifact("AGENTS.md", commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENTS_MD), want: true},
		{name: "nested CLAUDE.md is not root", artifact: artifact("backend/CLAUDE.md", commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD), want: false},
		{name: "nested AGENTS.md is not root", artifact: artifact("backend/AGENTS.md", commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENTS_MD), want: false},
		{name: "root path but rule type", artifact: artifact("CLAUDE.md", commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, isRootAlwaysLoadedInstruction(tt.artifact))
		})
	}
}

// bigBody builds a body with exactly `lines` non-blank lines whose total byte length is
// `targetLen`, so estimateTokens = targetLen/4 and countContentLines = lines. Padding lands on the
// last line so the line count stays exact.
func bigBody(t *testing.T, lines, targetLen int) string {
	t.Helper()
	require.Positive(t, lines)
	// Each of the first (lines-1) lines is "x\n" (2 bytes). The last line carries the rest with no
	// trailing newline, padded to hit targetLen exactly.
	base := 2 * (lines - 1)
	require.Greater(t, targetLen, base, "targetLen too small for the requested line count")
	lastLineLen := targetLen - base
	require.Positive(t, lastLineLen)
	var builder strings.Builder
	for i := 0; i < lines-1; i++ {
		builder.WriteString("x\n")
	}
	builder.WriteString(strings.Repeat("y", lastLineLen))
	out := builder.String()
	require.Len(t, out, targetLen)
	require.Equal(t, lines, countContentLines(out))
	return out
}

// directiveHeavyBody builds a body of `lines` non-blank BULLET lines totaling `targetLen` bytes, so
// every line is a directive (directive share == 1.0) while the byte/line ratio stays high — it
// isolates the tok-per-line lens from the directive-share lens (a dense body that is nonetheless
// well-structured instructions, not prose). Padding lands on the last bullet so the line count is
// exact.
func directiveHeavyBody(t *testing.T, lines, targetLen int) string {
	t.Helper()
	require.Positive(t, lines)
	// Each of the first (lines-1) lines is "- x\n" (4 bytes). The last line is a bullet ("- " + fill)
	// carrying the remaining bytes with no trailing newline.
	base := 4 * (lines - 1)
	require.Greater(t, targetLen, base+2, "targetLen too small for the requested line count")
	lastLineLen := targetLen - base
	var builder strings.Builder
	for i := 0; i < lines-1; i++ {
		builder.WriteString("- x\n")
	}
	builder.WriteString("- ")
	builder.WriteString(strings.Repeat("y", lastLineLen-2))
	out := builder.String()
	require.Len(t, out, targetLen)
	require.Equal(t, lines, countContentLines(out))
	return out
}

// TestInstructionIntentFindings_TokPerLineDoD is the spec Definition-of-Done case: a 176-line /
// 4178-token instruction file (tok/line ≈ 23.7) lands strictly above a corpus 75th-pctl, so the
// tok-per-line lens fires for it; a dense-but-within-quartile sibling does not.
func TestInstructionIntentFindings_TokPerLineDoD(t *testing.T) {
	t.Parallel()
	// The over-dense file: 176 non-blank lines, 16712 bytes → 4178 tokens → tok/line ≈ 23.74. Built
	// as all-bullet lines so its directive share is 1.0 — it is dense yet well-structured, so only
	// the tok-per-line lens fires, never directive-share (which would otherwise trip on share 0).
	overDense := directiveHeavyBody(t, 176, 16712)
	require.Equal(t, 4178, estimateTokens(overDense))
	require.Equal(t, 176, countContentLines(overDense))

	// Three low-density siblings so the corpus is 4 (the quartile floor) and the 75th-pctl sits
	// well below 23.74. Each is many short lines: low tok/line.
	lowDensity := func(lines int) string { return bigBody(t, lines, lines*8) } // ~2 tok/line

	artifacts := []*model.HarnessArtifact{
		{ID: "a-over", Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE, Name: "over", SourcePath: ".claude/rules/over.md"},
		{ID: "a-low1", Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE, Name: "low1", SourcePath: ".claude/rules/low1.md"},
		{ID: "a-low2", Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE, Name: "low2", SourcePath: ".claude/rules/low2.md"},
		{ID: "a-low3", Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE, Name: "low3", SourcePath: ".claude/rules/low3.md"},
	}
	bodies := map[string]string{
		"a-over": overDense,
		"a-low1": lowDensity(40),
		"a-low2": lowDensity(50),
		"a-low3": lowDensity(60),
	}
	aggregate := harnessImproveAggregate{committed: artifacts, bodies: bodies}

	findings := instructionIntentFindings(iiOrgID, iiRepoID, iiExecID, aggregate)

	// Exactly the over-dense artifact fires the tok-per-line lens. No directive-share finding fires:
	// the over-dense file is all bullet lines (directive share 1.0), and the low-density siblings are
	// under the min-tokens-for-share floor. None is a root CLAUDE.md, so the token-floor lens is silent.
	var tokPerLineFor []string
	for _, finding := range findings {
		require.NotNil(t, finding.HarnessArtifactID)
		require.NotContains(t, finding.Slug, ":"+instructionIntentFamilyDirectiveShare+":",
			"directive-heavy over-dense file and sub-floor siblings must not fire directive-share")
		if strings.Contains(finding.Slug, ":"+instructionIntentFamilyTokPerLine+":") {
			tokPerLineFor = append(tokPerLineFor, *finding.HarnessArtifactID)
		}
	}
	require.Equal(t, []string{"a-over"}, tokPerLineFor, "only the over-dense file lands above the 75th-pctl")
}

// TestInstructionIntentFindings_UnbalancedFenceNoDirectiveShare: a non-trivial mostly-prose file
// whose body ends with an unmatched ``` is measured as directive share 0, which alone would fire the
// directive-share lens — but the open fence makes the share unreliable, so no finding is emitted.
func TestInstructionIntentFindings_UnbalancedFenceNoDirectiveShare(t *testing.T) {
	t.Parallel()
	var builder strings.Builder
	// 200 prose lines (26 bytes each) → 5200 bytes → 1300 tokens, over the min-tokens-for-share
	// floor. No directive lines, so the raw directive share is 0.
	for i := 0; i < 200; i++ {
		builder.WriteString("this is a prose line here\n")
	}
	builder.WriteString("```") // opened, never closed — the fence is left unbalanced.
	body := builder.String()
	require.Greater(t, estimateTokens(body), instructionIntentMinTokensForShare)
	share, reliable := directiveShare(body)
	require.InDelta(t, 0.0, share, 1e-9)
	require.False(t, reliable)

	artifact := &model.HarnessArtifact{ID: "a-fence", Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE, SourcePath: ".claude/rules/fence.md"}
	aggregate := harnessImproveAggregate{
		committed: []*model.HarnessArtifact{artifact},
		bodies:    map[string]string{"a-fence": body},
	}
	findings := instructionIntentFindings(iiOrgID, iiRepoID, iiExecID, aggregate)
	for _, finding := range findings {
		require.NotContains(t, finding.Slug, ":"+instructionIntentFamilyDirectiveShare+":",
			"an unreliable (unbalanced-fence) share must not drive a directive-share finding")
	}
}

// TestInstructionIntentFindings_BelowCorpusFloorNoTokPerLine: under the corpus floor, the tok/line
// lens is skipped entirely (insufficient corpus) even for a very dense file.
func TestInstructionIntentFindings_BelowCorpusFloorNoTokPerLine(t *testing.T) {
	t.Parallel()
	dense := bigBody(t, 176, 16712)
	artifacts := []*model.HarnessArtifact{
		{ID: "a-1", Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE, Name: "one", SourcePath: ".claude/rules/one.md"},
		{ID: "a-2", Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE, Name: "two", SourcePath: ".claude/rules/two.md"},
	}
	aggregate := harnessImproveAggregate{
		committed: artifacts,
		bodies:    map[string]string{"a-1": dense, "a-2": dense},
	}
	findings := instructionIntentFindings(iiOrgID, iiRepoID, iiExecID, aggregate)
	for _, finding := range findings {
		require.NotContains(t, finding.Slug, ":"+instructionIntentFamilyTokPerLine+":",
			"tok/line lens must not fire below the corpus floor")
	}
}

// TestInstructionIntentFindings_TokenFloor: a root CLAUDE.md over the token floor fires the
// token-floor lens; a body is required (an artifact with no fetched body produces nothing).
func TestInstructionIntentFindings_TokenFloor(t *testing.T) {
	t.Parallel()
	overFloor := strings.Repeat("x", (instructionIntentTokenFloor+500)*4) // estimateTokens well over the floor
	tests := []struct {
		name       string
		artifact   *model.HarnessArtifact
		hasBody    bool
		wantFiring bool
	}{
		{
			name:       "root claude over floor fires",
			artifact:   &model.HarnessArtifact{ID: "root", Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD, SourcePath: "CLAUDE.md"},
			hasBody:    true,
			wantFiring: true,
		},
		{
			name:       "nested claude over floor does not fire token-floor",
			artifact:   &model.HarnessArtifact{ID: "nested", Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD, SourcePath: "backend/CLAUDE.md"},
			hasBody:    true,
			wantFiring: false,
		},
		{
			name:       "root claude without body produces no finding",
			artifact:   &model.HarnessArtifact{ID: "nobody", Type: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD, SourcePath: "CLAUDE.md"},
			hasBody:    false,
			wantFiring: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bodies := map[string]string{}
			if tt.hasBody {
				bodies[tt.artifact.ID] = overFloor
			}
			aggregate := harnessImproveAggregate{committed: []*model.HarnessArtifact{tt.artifact}, bodies: bodies}
			findings := instructionIntentFindings(iiOrgID, iiRepoID, iiExecID, aggregate)
			fired := false
			for _, finding := range findings {
				if strings.Contains(finding.Slug, ":"+instructionIntentFamilyTokenFloor+":") {
					fired = true
					require.Equal(t, iiRepoID, finding.ScopeValue)
					require.Equal(t, priorityLow, finding.Priority)
				}
			}
			require.Equal(t, tt.wantFiring, fired)
		})
	}
}
