// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/config"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// Behavioral-attribution lens — the Instruction-Intent behavioral-deviation sub-detector (spec
// harness-improve-detectors). It joins a committed skill/command/subagent's LLM-classified intent
// class to its org-wide invoked-skill behavior (completion, effective-context-use, tool-errors) and
// flags a skill whose measured behavior falls OUTSIDE its intent class's acceptable band.
//
// The bands, the classifier confidence floor, and the name-alias map below are engineer-chosen
// PROVISIONAL calibration seeds, NOT a settled decision: no in-repo reference implementation or eval
// calibrates them. These Go seeds are the detector's built-in defaults; a caller overrides any of them
// with a DetectorConfig (detector_config.go) passed to NewHarnessImproveEngine or set on
// ondisk.Options, and an unset field keeps its seed. Precision over recall: the bands are
// deliberately WIDE so the v1 lens under-fires. Each value changes which orgs' skills get flagged.

// harnessBehavioralNameMatchTotal meters whether a committed skill's canonical name found a
// non-demoted behavioral match (matched), had no behavioral row (unmatched), or was demoted for a
// canonical-name content collision (collision).
var harnessBehavioralNameMatchTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "recommendations",
	Subsystem: "harness_improve",
	Name:      "behavioral_name_match_total",
	Help:      "Harness-improve behavioral name-match outcomes, by outcome (matched|unmatched|collision).",
}, []string{"outcome"})

// harnessBehavioralDegradeTotal meters an intent classification dropped before it could drive a
// finding: below the confidence floor, an ambiguous class, or a classify call error.
var harnessBehavioralDegradeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "recommendations",
	Subsystem: "harness_improve",
	Name:      "behavioral_degrade_total",
	Help:      "Harness-improve intent-classify degrades, by reason (below_floor|ambiguous|llm_error).",
}, []string{"reason"})

// The outcome label values for harnessFixProduceTotal — the complete fix-producer routing/result set.
const (
	fixOutcomeDeterministic = "deterministic"
	fixOutcomeLLM           = "llm"
	fixOutcomeAdvisory      = "advisory"
	fixOutcomeLLMError      = "llm_error"
	fixOutcomeCapped        = "capped"
	// fixOutcomeAttachError: a grounded fix was produced but its suggested body could not be stored to
	// the content store, so the finding degrades to advisory (the run continues — one un-storable body never drops the
	// whole scan).
	fixOutcomeAttachError = "attach_error"
)

// harnessFixProduceTotal meters what the fix producer did with each finding: produced a deterministic
// broken-path patch, produced an LLM Restructure draft, left it advisory (no fix lane, or an ambiguous
// rename, or no fetched body), dropped an errored LLM generate, or dropped an LLM finding past the
// per-run cap. A shift in the mix (e.g. a spike in llm_error, or capped rising) is the producer's health
// signal alongside the behavioral counters.
var harnessFixProduceTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "recommendations",
	Subsystem: "harness_improve",
	Name:      "fix_produce_total",
	Help:      "Harness-improve fix-producer outcomes, by outcome (deterministic|llm|advisory|llm_error|capped).",
}, []string{"outcome"})

// registerBehavioralMetrics registers the behavioral/fix-producer metrics on reg. Opt-in (no
// import-time side effect): the package's RegisterMetrics calls it once at startup.
func registerBehavioralMetrics(reg prometheus.Registerer) {
	reg.MustRegister(harnessBehavioralNameMatchTotal, harnessBehavioralDegradeTotal, harnessFixProduceTotal)
	// Seed the full label sets so every outcome/reason shows on the dashboard from the first
	// scrape (a never-yet-observed label reads as 0, not an absent series).
	for _, outcome := range []string{"matched", "unmatched", "collision"} {
		harnessBehavioralNameMatchTotal.WithLabelValues(outcome).Add(0)
	}
	for _, reason := range []string{"below_floor", "ambiguous", "llm_error"} {
		harnessBehavioralDegradeTotal.WithLabelValues(reason).Add(0)
	}
	for _, outcome := range []string{fixOutcomeDeterministic, fixOutcomeLLM, fixOutcomeAdvisory, fixOutcomeLLMError, fixOutcomeCapped, fixOutcomeAttachError} {
		harnessFixProduceTotal.WithLabelValues(outcome).Add(0)
	}
}

// behavioralClassifyConfidenceFloor drops an intent classification below this confidence (0..1).
// PROVISIONAL seed; DetectorConfig.ClassifyConfidenceFloor overrides it.
const behavioralClassifyConfidenceFloor = 0.5

// behavioralNameMatchAliases maps a canonical invoked-skill name to the canonical harness-artifact
// name it should be attributed to, for renames the canonicalizer alone cannot bridge. Empty by
// default; DetectorConfig.NameMatchAliases overrides it.
var behavioralNameMatchAliases = map[string]string{}

// behavioralMetricRange is an inclusive [min,max] band for one behavioral metric.
type behavioralMetricRange struct {
	min float64
	max float64
}

// behavioralBand is one intent class's acceptable range per behavioral metric. A nil range means
// the metric is not scored for that class (mirrors config's pointer-per-metric convention).
type behavioralBand struct {
	// completion is the acceptable invoked-skill completion-rate range (0..1).
	completion *behavioralMetricRange
	// contextUse is the acceptable effective-context-use range (0.0..1.0, a fraction — matches the
	// effectiveContextUsagePercent proto contract).
	contextUse *behavioralMetricRange
	// toolError is the acceptable average-tool-errors-per-invocation range (>=0).
	toolError *behavioralMetricRange
}

// behavioralBandsByClass is the per-intent-class band set — the PROVISIONAL calibration seeds a
// DetectorConfig with BehavioralBands set replaces as a whole. WIDE by design (precision over recall). An unscored metric is simply absent from a class's band.
var behavioralBandsByClass = map[string]behavioralBand{
	"mechanical": {
		completion: &behavioralMetricRange{min: 0.5, max: 1.0},
		contextUse: &behavioralMetricRange{min: 0.0, max: 0.80},
		toolError:  &behavioralMetricRange{min: 0.0, max: 3.0},
	},
	"exploratory": {
		contextUse: &behavioralMetricRange{min: 0.0, max: 0.95},
		toolError:  &behavioralMetricRange{min: 0.0, max: 5.0},
	},
	"review": {
		completion: &behavioralMetricRange{min: 0.4, max: 1.0},
		contextUse: &behavioralMetricRange{min: 0.0, max: 0.85},
		toolError:  &behavioralMetricRange{min: 0.0, max: 3.0},
	},
	"orchestrator": {
		completion: &behavioralMetricRange{min: 0.2, max: 1.0},
		contextUse: &behavioralMetricRange{min: 0.0, max: 0.98},
		toolError:  &behavioralMetricRange{min: 0.0, max: 6.0},
	},
	"gate": {
		completion: &behavioralMetricRange{min: 0.5, max: 1.0},
		contextUse: &behavioralMetricRange{min: 0.0, max: 0.70},
		toolError:  &behavioralMetricRange{min: 0.0, max: 2.0},
	},
}

// detectorSettings is the RESOLVED behavioral-attribution calibration one engine instance runs on:
// the per-intent-class bands, the classifier confidence floor, the trailing behavioral window, the
// invoked-skill→artifact name-alias map, the per-run detector caps, and the vendor-surface markers.
// The engine holds one, populated once at construction from the injected DetectorConfig (or the
// Go-seed defaults), and threads its fields into the consumers as arguments — so a caller's tuned
// value actually reaches the detector.
type detectorSettings struct {
	bands      map[string]behavioralBand
	floor      float64
	windowDays int
	aliases    map[string]string
	// recentHiddenWindowRuns is how many distinct recent runs the findings gate deduplicates a
	// HIDDEN_FAILED_EVAL finding over (DetectorConfig.RecentHiddenWindowRuns), the
	// harness-improve counterpart of the derive flow's HiddenDedupRuns.
	recentHiddenWindowRuns int
	// maxDetectorBodies caps how many DISTINCT artifact bodies one scan fetches from object storage
	// (DetectorConfig.MaxDetectorBodiesPerRun).
	maxDetectorBodies int
	// maxBehavioralClassify caps how many DISTINCT-content intent classifications one scan issues
	// (DetectorConfig.MaxBehavioralClassifyPerRun).
	maxBehavioralClassify int
	// maxFixGen caps how many LLM fix-generate calls one scan issues
	// (DetectorConfig.MaxFixGenPerRun). The deterministic broken-path lane is free and uncapped;
	// only the paid Restructure-lane generate calls are bounded.
	maxFixGen int
	// vendorMarkers are the path-like markers the vendor-surface Conflict lens matches against a
	// Claude-loaded body (DetectorConfig.VendorSurfaceMarkers).
	vendorMarkers []string
	// hardMarkers are the whole-word tokens that mark a rule-ledger row as hard-gated, so a
	// root-level restatement of that rule is a finding (DetectorConfig.HardMarkers).
	hardMarkers []string
	// ruleLedgerBasename is the filename the root-restatement lens treats as the rule ledger
	// (DetectorConfig.RuleLedgerBasename).
	ruleLedgerBasename string
}

// defaultRecentHiddenWindowRuns mirrors the derive flow's HiddenDedupRuns default (8): the built-in
// recent-hidden dedup window the findings gate uses when the config leaves it unset. The single
// definition of the Go-seed default.
const defaultRecentHiddenWindowRuns = 8

// maxFixGenPerRun is the built-in cap on how many LLM fix-generate calls one scan issues when the
// config leaves MaxFixGenPerRun unset. The single definition of the Go-seed default.
const maxFixGenPerRun = 50

// defaultDetectorConfig is the built-in calibration — the Go-const seeds (behavioralBandsByClass,
// behavioralClassifyConfidenceFloor, harnessImproveBehavioralWindowDays, behavioralNameMatchAliases),
// which remain the single definition of the defaults. Used when the engine is constructed without a
// config, and field by field for every field a DetectorConfig leaves unset. DefaultDetectorConfig
// resolves to exactly these settings (TestDetectorSettingsFromConfig_PublicDefaultsResolveToSeeds).
func defaultDetectorConfig() detectorSettings {
	return detectorSettings{
		bands:                  behavioralBandsByClass,
		floor:                  behavioralClassifyConfidenceFloor,
		windowDays:             harnessImproveBehavioralWindowDays,
		aliases:                behavioralNameMatchAliases,
		recentHiddenWindowRuns: defaultRecentHiddenWindowRuns,
		maxDetectorBodies:      maxDetectorBodiesPerRun,
		maxBehavioralClassify:  maxBehavioralClassifyPerRun,
		maxFixGen:              maxFixGenPerRun,
		vendorMarkers:          vendorSurfaceMarkers,
		hardMarkers:            config.DefaultHardMarkers,
		ruleLedgerBasename:     config.DefaultRuleLedgerBasename,
	}
}

// detectorSettingsFromConfig converts a DetectorConfig into the resolved internal settings ONCE, at
// engine construction. A nil config resolves to defaultDetectorConfig. Otherwise each field resolves on
// its own: an unset field (no bands, no aliases, a non-positive number, an empty marker list or
// basename) keeps its Go seed, so a caller can tune one knob without restating the rest, and a
// partially-specified config never silently zeroes the detector.
func detectorSettingsFromConfig(cfg *DetectorConfig) detectorSettings {
	if cfg == nil {
		return defaultDetectorConfig()
	}
	bands := behavioralBandsByClass
	if len(cfg.BehavioralBands) > 0 {
		bands = make(map[string]behavioralBand, len(cfg.BehavioralBands))
		for class, band := range cfg.BehavioralBands {
			if band == nil {
				continue
			}
			bands[class] = behavioralBand{
				completion: metricRangeFromConfig(band.Completion),
				contextUse: metricRangeFromConfig(band.ContextUse),
				toolError:  metricRangeFromConfig(band.ToolError),
			}
		}
	}
	aliases := behavioralNameMatchAliases
	if len(cfg.NameMatchAliases) > 0 {
		aliases = make(map[string]string, len(cfg.NameMatchAliases))
		for from, to := range cfg.NameMatchAliases {
			aliases[from] = to
		}
	}
	windowDays := cfg.BehavioralWindowDays
	if windowDays <= 0 {
		windowDays = harnessImproveBehavioralWindowDays
	}
	recentHiddenWindowRuns := cfg.RecentHiddenWindowRuns
	if recentHiddenWindowRuns <= 0 {
		recentHiddenWindowRuns = defaultRecentHiddenWindowRuns
	}
	maxDetectorBodies := cfg.MaxDetectorBodiesPerRun
	if maxDetectorBodies <= 0 {
		maxDetectorBodies = maxDetectorBodiesPerRun
	}
	maxBehavioralClassify := cfg.MaxBehavioralClassifyPerRun
	if maxBehavioralClassify <= 0 {
		maxBehavioralClassify = maxBehavioralClassifyPerRun
	}
	maxFixGen := cfg.MaxFixGenPerRun
	if maxFixGen <= 0 {
		maxFixGen = maxFixGenPerRun
	}
	vendorMarkers := cfg.VendorSurfaceMarkers
	if len(vendorMarkers) == 0 {
		vendorMarkers = vendorSurfaceMarkers
	}
	hardMarkers := cfg.HardMarkers
	if len(hardMarkers) == 0 {
		hardMarkers = config.DefaultHardMarkers
	}
	ruleLedgerBasename := cfg.RuleLedgerBasename
	if ruleLedgerBasename == "" {
		ruleLedgerBasename = config.DefaultRuleLedgerBasename
	}
	// A non-positive floor is a partial-config omission, not an intent to disable the confidence
	// gate (a 0.0 floor would keep EVERY low-confidence classification, inverting precision-over-
	// recall). Fall back to the seed like every sibling field above.
	floor := cfg.ClassifyConfidenceFloor
	if floor <= 0 {
		floor = behavioralClassifyConfidenceFloor
	}
	return detectorSettings{
		bands:                  bands,
		floor:                  floor,
		windowDays:             windowDays,
		aliases:                aliases,
		recentHiddenWindowRuns: recentHiddenWindowRuns,
		maxDetectorBodies:      maxDetectorBodies,
		maxBehavioralClassify:  maxBehavioralClassify,
		maxFixGen:              maxFixGen,
		vendorMarkers:          vendorMarkers,
		hardMarkers:            hardMarkers,
		ruleLedgerBasename:     ruleLedgerBasename,
	}
}

// metricRangeFromConfig maps a config MetricRange onto the internal band range; a nil config range
// stays nil (the metric is not scored for that class).
func metricRangeFromConfig(r *MetricRange) *behavioralMetricRange {
	if r == nil {
		return nil
	}
	return &behavioralMetricRange{min: r.Min, max: r.Max}
}

// classifiedIntent is one artifact's surviving intent classification — its class and the
// classifier's confidence, which becomes the finding's ConfidenceScore (deterministic detectors
// carry 1.0; a behavioral finding carries the model's confidence instead).
type classifiedIntent struct {
	class      string
	confidence float64
}

// behavioralCollisionTypes is the artifact-type set the org-wide collision guard reads — the same
// skill/command/subagent set the behavioral lens scores, so a colliding name of any scored type is
// demoted (the demotion protects attribution for every type the lens can flag).
var behavioralCollisionTypes = []commonv1.HarnessArtifactType{
	commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
	commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND,
	commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT,
}

// demotedCollisionNames returns the canonical names that cannot be attributed to a single committed
// artifact. The invariant: a canonical name mapping to ≥2 distinct committed contents (≥2 distinct
// ContentHash) is unattributable — demote. Two distinct committed bodies sharing one canonical name
// are indistinguishable to the org-wide behavioral pool whether they live in one repo or across
// several, so the behavioral lens skips such a name. One body replicated across many repos is a
// single ContentHash and stays un-demoted (it attributes cleanly to that one content).
func demotedCollisionNames(contents []dao.VCSArtifactContent, aliases map[string]string) map[string]struct{} {
	hashesByCanonical := map[string]map[string]struct{}{}
	for _, content := range contents {
		canonical := resolveCanonicalName(content.Name, aliases)
		hashes := hashesByCanonical[canonical]
		if hashes == nil {
			hashes = map[string]struct{}{}
			hashesByCanonical[canonical] = hashes
		}
		hashes[content.ContentHash] = struct{}{}
	}
	demoted := map[string]struct{}{}
	for canonical, hashes := range hashesByCanonical {
		if len(hashes) >= 2 {
			demoted[canonical] = struct{}{}
		}
	}
	return demoted
}

// isBehavioralCandidate reports whether an artifact is a skill/command/subagent the behavioral lens
// classifies and scores — the types whose intent-vs-behavior mismatch this detector measures.
func isBehavioralCandidate(a *model.HarnessArtifact) bool {
	switch a.Type {
	case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND,
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT:
		return true
	default:
		return false
	}
}

// bodyRequiredForBehavioral reports whether an artifact's body is input to the behavioral lens's
// intent classifier — the skill/command/subagent bodies it sends to tetriz-py. SKILL/COMMAND
// bodies are already fetched for the Conflict lens; this widens the union to subagents (AGENT).
func bodyRequiredForBehavioral(a *model.HarnessArtifact) bool {
	return isBehavioralCandidate(a)
}

// The classify_harness_intent entity_type vocabulary tetriz-py reads. Distinct from the
// artifactTypeLabel/UI vocabulary (which spells AGENT "agent", not "subagent"), so this is its
// own set of constants, not shared with them.
const (
	behavioralEntityTypeSkill    = "skill"
	behavioralEntityTypeCommand  = "command"
	behavioralEntityTypeSubagent = "subagent"
)

// behavioralEntityType maps a committed artifact's type onto the classify_harness_intent
// entity_type vocabulary (skill|command|subagent) tetriz-py reads; "" for a non-candidate type.
func behavioralEntityType(t commonv1.HarnessArtifactType) string {
	switch t {
	case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL:
		return behavioralEntityTypeSkill
	case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND:
		return behavioralEntityTypeCommand
	case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT:
		return behavioralEntityTypeSubagent
	default:
		return ""
	}
}

// canonicalArtifactName normalizes an invoked-skill name or a harness-artifact name into a shared
// comparison key: strip a leading `/` or `$` (pure command/skill invocation sigils — `/deploy` and
// `deploy` are the same artifact), lowercase, and trim. A `plugin:<ns>:` prefix is left INTACT
// (only lowercased): an external plugin skill (`plugin:ci:deploy`) is a DIFFERENT artifact from a
// local committed `deploy` and must never fold into it — the plugin skill is never in the committed
// VCS set, so the collision guard cannot catch a wrong attribution. Leaving the prefix intact means
// it matches only a committed artifact literally named `plugin:<ns>:<name>` (rare → safe). Both
// sides of the invoked-skill↔committed-artifact match run through it so the two vocabularies align.
func canonicalArtifactName(name string) string {
	name = strings.TrimSpace(name)
	if len(name) > 0 && (name[0] == '/' || name[0] == '$') {
		name = name[1:]
	}
	return strings.ToLower(strings.TrimSpace(name))
}

// resolveCanonicalName canonicalizes a name and then applies the resolved alias map, so a renamed
// skill still attributes to its committed artifact. The result is the shared match key. The aliases
// come from the engine's detectorSettings (config-driven), not a package global.
func resolveCanonicalName(name string, aliases map[string]string) string {
	canonical := canonicalArtifactName(name)
	if target, ok := aliases[canonical]; ok {
		// Normalize the alias target too, so a target written with a sigil or mixed case
		// (`/Deploy`) resolves to the same key the committed side canonicalizes to (`deploy`).
		return canonicalArtifactName(target)
	}
	return canonical
}

// behavioralContentKey is the dedup key for the intent-classify fan-out: sha256 of the committed
// body, so distinct artifacts sharing one body content are classified once. A hash (not the raw
// body) keeps both the group map key and the deterministic cap-sort cheap regardless of body size.
func behavioralContentKey(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// mergeInvokedSkillBehavior folds two behavioral rows that canonicalize to the SAME name into one,
// invocation- / coverage-weighting each mean so the merged average is the true pooled mean rather
// than a mean-of-means. Coverage counts sum. Two distinct raw invokedSkills names collapsing to one
// canonical key is the case this handles.
func mergeInvokedSkillBehavior(dst, src dao.InvokedSkillBehavior) dao.InvokedSkillBehavior {
	return dao.InvokedSkillBehavior{
		Invocations:            dst.Invocations + src.Invocations,
		CompletionRate:         weightedMean(dst.CompletionRate, dst.Invocations, src.CompletionRate, src.Invocations),
		AvgEffectiveContextUse: weightedMean(dst.AvgEffectiveContextUse, dst.ContextCoverageN, src.AvgEffectiveContextUse, src.ContextCoverageN),
		ContextCoverageN:       dst.ContextCoverageN + src.ContextCoverageN,
		AvgToolErrors:          weightedMean(dst.AvgToolErrors, dst.ToolErrorCoverageN, src.AvgToolErrors, src.ToolErrorCoverageN),
		ToolErrorCoverageN:     dst.ToolErrorCoverageN + src.ToolErrorCoverageN,
	}
}

// weightedMean pools two means by their sample counts; zero total weight yields 0.
func weightedMean(aVal float64, aWeight int, bVal float64, bWeight int) float64 {
	total := aWeight + bWeight
	if total == 0 {
		return 0
	}
	return (aVal*float64(aWeight) + bVal*float64(bWeight)) / float64(total)
}

// behavioralDeviationFindings emits one Instruction-Intent behavioral-deviation finding per
// committed skill/command/subagent whose measured behavior falls outside its classified intent
// class's band. PURE over the already-assembled aggregate + the enrich phase's intent map (no I/O),
// so it slots into the detectAndGrade append chain. An artifact contributes only when it has a
// surviving intent entry, a non-demoted canonical-name behavioral match with invocations, and a
// known intent-class band; a metric with zero coverage is neither in nor out of band (not scored),
// and only a PRESENT metric outside its band drives the finding. Insufficient data (demoted name or
// no behavioral row) yields no finding.
func behavioralDeviationFindings(orgID, repoID, taskExecutionID string, agg *harnessImproveAggregate, intentByArtifactID map[string]classifiedIntent, bands map[string]behavioralBand, aliases map[string]string) []*model.PromptLibraryItem {
	var findings []*model.PromptLibraryItem
	for _, artifact := range agg.committed {
		if !isBehavioralCandidate(artifact) {
			continue
		}
		intent, classified := intentByArtifactID[artifact.ID]
		if !classified {
			continue
		}
		canonical := resolveCanonicalName(artifact.Name, aliases)
		if _, demoted := agg.nameCollisionDemoted[canonical]; demoted {
			continue
		}
		behavior, matched := agg.behavioralByName[canonical]
		if !matched || behavior.Invocations == 0 {
			// Insufficient data — no behavioral evidence for this skill, so no finding.
			continue
		}
		band, known := bands[intent.class]
		if !known {
			continue
		}
		deviations := evaluateBands(behavior, band)
		if len(deviations) == 0 {
			continue
		}
		findings = append(findings, newBehavioralFinding(orgID, taskExecutionID, repoID, artifact,
			intent.class, intent.confidence, strings.Join(deviations, "; ")))
	}
	return findings
}

// evaluateBands returns a human phrase per PRESENT metric that falls outside its band. Completion is
// present for every invocation (its coverage is Invocations); effective-context-use and tool-errors
// are present only when their coverage count is > 0. A nil band range means the metric is not scored
// for the class. Empty result means every present, scored metric is within band.
func evaluateBands(behavior dao.InvokedSkillBehavior, band behavioralBand) []string {
	var deviations []string
	if band.completion != nil && outOfBand(behavior.CompletionRate, band.completion) {
		deviations = append(deviations, fmt.Sprintf("completion %.2f outside [%.2f,%.2f]",
			behavior.CompletionRate, band.completion.min, band.completion.max))
	}
	if behavior.ContextCoverageN > 0 && band.contextUse != nil && outOfBand(behavior.AvgEffectiveContextUse, band.contextUse) {
		// The value and band are 0.0..1.0 fractions (the effectiveContextUsagePercent proto contract);
		// the comparison above is on that scale. Render as a human percent (×100) for the finding.
		deviations = append(deviations, fmt.Sprintf("effective-context-use %.0f%% outside [%.0f%%,%.0f%%]",
			behavior.AvgEffectiveContextUse*100, band.contextUse.min*100, band.contextUse.max*100))
	}
	if behavior.ToolErrorCoverageN > 0 && band.toolError != nil && outOfBand(behavior.AvgToolErrors, band.toolError) {
		deviations = append(deviations, fmt.Sprintf("tool-errors %.2f outside [%.2f,%.2f]",
			behavior.AvgToolErrors, band.toolError.min, band.toolError.max))
	}
	return deviations
}

// outOfBand reports whether value falls outside the inclusive [min,max] range.
func outOfBand(value float64, r *behavioralMetricRange) bool {
	return value < r.min || value > r.max
}

// newBehavioralFinding builds one Instruction-Intent behavioral-deviation finding. It mirrors
// newDeterministicFinding's field set but carries the CLASSIFIER's confidence (not the deterministic
// 1.0) and an Evidence projection with both the reference_point and the intent_class, so the surface
// can show why the skill was judged off-intent. MEDIUM priority, PROJECT scope on the repo.
func newBehavioralFinding(orgID, taskExecutionID, repoID string, artifact *model.HarnessArtifact,
	intentClass string, confidence float64, referencePoint string,
) *model.PromptLibraryItem {
	dimension := promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_INSTRUCTION_INTENT
	source := promptlibraryv1.ArtifactRecommendationSource_ARTIFACT_RECOMMENDATION_SOURCE_DETECTED
	fc := findingCopyFor(behavioralFamilyDeviation, referencePoint, intentClass)
	return &model.PromptLibraryItem{
		OrganizationID:       orgID,
		TaskExecutionID:      taskExecutionID,
		Dimension:            &dimension,
		RecommendationSource: &source,
		HarnessArtifactID:    &artifact.ID,
		Priority:             priorityMedium,
		ConfidenceScore:      float32(confidence),
		SurfaceState:         promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE,
		ClientStatus:         promptlibraryv1.ArtifactClientStatus_ARTIFACT_CLIENT_STATUS_UNSPECIFIED,
		ArtifactType:         instructionArtifactType(artifact.Type),
		Scope:                promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT,
		ScopeValue:           repoID,
		Slug:                 behavioralDeviationSlug(repoID, artifact.ID, intentClass),
		Title:                "Skill behaves off its stated intent",
		// Advisory by default; attachGroundedFixes flips it false when a grounded fix is attached.
		Evidence: &promptlibraryv1.EvidenceProjection{
			ReferencePoint:    referencePoint,
			IntentClass:       intentClass,
			Summary:           fc.Summary,
			RecommendedAction: fc.RecommendedAction,
			Advisory:          true,
			SourcePath:        artifact.SourcePath,
			SourceName:        artifact.Name,
		},
	}
}

// behavioralDeviationSlug builds the stable finding slug
// "<repoID>:instruction-intent:behavioral-deviation:<hash>" where hash is the first 12 hex of
// sha256(artifactID + the family + intentClass) — stable per (artifact, class) so re-runs converge.
func behavioralDeviationSlug(repoID, artifactID, intentClass string) string {
	sum := sha256.Sum256([]byte(artifactID + ":instruction-intent:" + behavioralFamilyDeviation + ":" + intentClass))
	shortHash := hex.EncodeToString(sum[:])[:12]
	return fmt.Sprintf("%s:instruction-intent:%s:%s", repoID, behavioralFamilyDeviation, shortHash)
}
