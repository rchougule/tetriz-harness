// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package config holds the plain-struct detector and derivation tunables the
// recommendations engine reads: ordinary Go structs a caller fills (or takes the neutral
// defaults). The recommendations package re-exports the detector types as
// recommendations.DetectorConfig, BehavioralBand and MetricRange, which is how a caller
// outside this module names them.
package config

// MetricRange is an inclusive [Min, Max] band for one behavioral metric.
type MetricRange struct {
	Min float64
	Max float64
}

// BehavioralBand is one intent-class's acceptable range per behavioral metric. Each range
// is a pointer so an unset metric (nil) means "not scored for this class" rather than a
// [0,0] band that would flag every value.
type BehavioralBand struct {
	// Completion is the acceptable invoked-artifact completion-rate range (0..1).
	Completion *MetricRange
	// ContextUse is the acceptable effective-context-use range (0.0..1.0 fraction).
	ContextUse *MetricRange
	// ToolError is the acceptable average-tool-errors-per-invocation range (>=0).
	ToolError *MetricRange
}

// DetectorConfig carries the DETECTED (harness-improve) detectors' tunables: the
// per-intent-class behavioral bands, the invoked-artifact name alias map, the classifier
// confidence floor, the trailing behavioral window, the per-run caps, the vendor-surface
// marker set, and the conflict detector's marker knobs.
type DetectorConfig struct {
	// BehavioralBands maps an intent_class to its acceptable [min,max] range per behavioral
	// metric; a metric absent from the band is not scored for that class. Bands are
	// deliberately wide (precision over recall) so the lens under-fires.
	BehavioralBands map[string]*BehavioralBand
	// NameMatchAliases maps a canonical invoked-artifact name to the canonical harness-artifact
	// name it is attributed to, for renames the canonicalizer alone cannot bridge.
	NameMatchAliases map[string]string
	// ClassifyConfidenceFloor drops an intent classification below this confidence (0..1).
	ClassifyConfidenceFloor float64
	// BehavioralWindowDays is the trailing calendar window the invoked-artifact behavior is read over.
	BehavioralWindowDays int
	// RecentHiddenWindowRuns is the number of distinct recent runs the findings gate deduplicates a
	// hidden finding over. A non-positive value falls back to the built-in default, so a partial
	// config never zeroes the window into "suppress nothing".
	RecentHiddenWindowRuns int
	// MaxDetectorBodiesPerRun caps how many distinct artifact bodies one scan fetches. Non-positive
	// falls back to the built-in default.
	MaxDetectorBodiesPerRun int
	// MaxBehavioralClassifyPerRun caps how many distinct-content intent classifications one scan
	// issues to the LLM service. Non-positive falls back to the built-in default.
	MaxBehavioralClassifyPerRun int
	// MaxFixGenPerRun caps how many LLM fix-generate calls one scan issues. The deterministic
	// broken-path lane is free and uncapped. Non-positive falls back to the built-in default.
	MaxFixGenPerRun int
	// VendorSurfaceMarkers are the path-like config markers of another AI tool's setup that, when
	// surfaced as authority in a primary-tool file, raise a vendor-surface conflict finding. Empty
	// falls back to DefaultVendorSurfaceMarkers.
	VendorSurfaceMarkers []string
	// HardMarkers are the whole-word tokens that mark a rule-ledger row as hard-gated (so a
	// root-level restatement of it is a finding). Empty falls back to DefaultHardMarkers.
	HardMarkers []string
	// RuleLedgerBasename is the filename the root-restatement detector treats as the rule ledger.
	// Empty falls back to DefaultRuleLedgerBasename.
	RuleLedgerBasename string
}

// DeriveConfig carries the DERIVED flow's tunables: the session-quality bucket boundaries,
// the repo-tree and review-body caps, the review-classification window/corpus knobs, and the
// per-run artifact caps. A caller reads these per run so a change takes effect without a restart.
type DeriveConfig struct {
	// LowScoreMax is the overall-score at or below which a session counts as "low".
	LowScoreMax int
	// HighScoreMin is the overall-score at or above which a session counts as "high".
	HighScoreMin int
	// RepoTreeMaxTokens caps the per-call RepoTree encoding (0 = uncapped).
	RepoTreeMaxTokens int
	// MaxReviewBodies is the per-project review-body safety valve (0 = uncapped).
	MaxReviewBodies int
	// ReviewClassifyLookbackDays bounds the review-comment classify window to the trailing N days
	// (clamped to the derivation-window floor); <=0 means no extra clamp.
	ReviewClassifyLookbackDays int
	// ReviewClassifyMaxComments caps the review corpus fed to classification, most-recent-first;
	// <=0 falls back to MaxReviewBodies.
	ReviewClassifyMaxComments int
	// MinReviewCommentsPerRepo is the comment count at/above which a repo classifies 1:1. Below it
	// (and above the admission floor) the repo is "enriched" and each comment classifies into up to
	// MaxReviewThemesPerComment themes.
	MinReviewCommentsPerRepo int
	// MaxReviewThemesPerComment is the top-N themes per comment on the enriched path.
	MaxReviewThemesPerComment int
	// DataWindowDays is the read-window length the derivation reads session/PR signal over.
	DataWindowDays int
	// MinReviewThemesPerRepo is the admission floor: an enriched repo with fewer theme assignments
	// than this derives no artifacts. <=0 disables the theme-floor gate.
	MinReviewThemesPerRepo int
	// HiddenDedupRuns is the number of recent runs a hidden derived recommendation is deduplicated over.
	HiddenDedupRuns int
	// MinLowBucket / MinHighBucket are the minimum low-/high-quality session counts a repo needs
	// before the quality gate admits its derived artifacts.
	MinLowBucket  int
	MinHighBucket int
	// MaxHooksPerRun / MaxAgentsPerRun cap how many hook/agent artifacts one run persists.
	MaxHooksPerRun  int
	MaxAgentsPerRun int
}

// Built-in defaults the detectors fall back to when a field is unset.
var (
	// DefaultHardMarkers are the whole-word hard-gate tokens for the root-restatement detector.
	DefaultHardMarkers = []string{"hard-gated", "blocker"}
	// DefaultRuleLedgerBasename is the filename treated as the rule ledger.
	DefaultRuleLedgerBasename = "rule-index.md"
	// DefaultVendorSurfaceMarkers are the other-tool config paths that raise a vendor-surface finding.
	DefaultVendorSurfaceMarkers = []string{
		".cursorrules", ".cursor/rules", ".github/copilot-instructions",
		".kiro/steering", ".kiro/hooks", ".agents/agents", "gemini.md", ".codex/",
	}
)

// DefaultDetectorConfig returns a DetectorConfig with the neutral marker defaults set and
// behavioral bands left unset (the engine seeds its own default bands). A caller overrides
// any field before handing it to the engine.
func DefaultDetectorConfig() DetectorConfig {
	return DetectorConfig{
		HardMarkers:          append([]string(nil), DefaultHardMarkers...),
		RuleLedgerBasename:   DefaultRuleLedgerBasename,
		VendorSurfaceMarkers: append([]string(nil), DefaultVendorSurfaceMarkers...),
	}
}
