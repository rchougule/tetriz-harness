// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// finding_copy.go is the single home for the USER-FACING copy of a DETECTED finding: the plain-English
// "why this" summary and the recommended action. Both are STATIC per-family text (a pure function of the
// finding's family + its own reference_point/intent_class) — no LLM. The detector already sets the
// precise, machine-readable reference_point; this file translates it into something a non-technical user
// reads. The map is keyed by the finding family (slugFamily); families are unique across all dimensions,
// so a flat family→copy map suffices. Every family a builder can emit MUST have an entry (a table test
// enforces it) — a missing family degrades to an empty summary/action, which reads as a bare finding.

// behavioralFamilyDeviation is the behavioral-deviation finding family (the 3rd slug segment) — the one
// family whose builder (behavioralDeviationSlug) lives in behavioral.go rather than beside a conflict/
// security/instruction family const. The slug builder, the copy map, and findingCopyFor's special-case
// all reference this const, so the family label can never drift between where it is emitted and where
// it is matched.
const behavioralFamilyDeviation = "behavioral-deviation"

// findingCopy is the user-facing copy for one finding family. Summary is a `{ref}`-templated "why this"
// sentence (the raw reference_point is still shown separately in the UI as the precise anchor);
// RecommendedAction is the plain-English remedy.
type findingCopy struct {
	Summary           string
	RecommendedAction string
}

// findingCopyByFamily maps every finding family to its copy. Summary carries a `{ref}` placeholder that
// findingCopyFor fills with the finding's reference_point. behavioral-deviation is the exception:
// findingCopyFor derives its Summary from the band numbers (humanizeBehavioralBand) and reads only this
// entry's RecommendedAction, so the behavioral Summary below is never read at runtime (a documented
// fallback, not a value the completeness test depends on — that test runs through findingCopyFor). Keep
// this wording plain and free of detector jargon: a Summary that both names what was found AND says why
// it matters (the risk/consequence, not just the mechanism), and a concrete, actionable RecommendedAction.
// Aim to stay within evidenceProjectionSummaryMaxChars (~500) so the inline card is not truncated.
var findingCopyByFamily = map[string]findingCopy{
	conflictFamilyDeadGlob: {
		Summary:           "This rule includes a file-scoping pattern (`{ref}`) that matches no committed file in the repo. That pattern selects nothing — if it is the rule's only pattern the rule never loads at all, and if there are others it is dead weight that obscures the rule's real scope.",
		RecommendedAction: "Fix the pattern so it matches the files you intend (for example, correct a renamed directory), or remove it if it is no longer needed.",
	},
	conflictFamilyBrokenPath: {
		Summary:           "This file points to `{ref}`, which is not a committed file in the repo. When your agent follows that reference it finds nothing, so whatever guidance or context the link was meant to supply is silently missing.",
		RecommendedAction: "Update the reference to the file's current path, or remove it if the target no longer exists.",
	},
	conflictFamilyDupID: {
		Summary:           "A rule ID is defined in more than one file (`{ref}`). When the same ID carries different text in two places, your agent can load conflicting instructions with no way to tell which one should win.",
		RecommendedAction: "Keep a single definition as the source of truth and delete the duplicate, or give the other rule its own distinct ID.",
	},
	conflictFamilyRootRestatement: {
		Summary:           "This always-loaded file repeats a rule (`{ref}`) that is already enforced automatically elsewhere. It adds length to a file that loads on every task without adding any coverage, and the two copies can drift out of sync over time.",
		RecommendedAction: "Remove the restated rule from this file — the existing enforcement already covers it.",
	},
	conflictFamilyVendorSurface: {
		Summary:           "This file is loaded by Claude but points to another tool's configuration (`{ref}`) as the source of authority. Your agent can be steered by config it does not actually run under, producing guidance that does not match its real behavior.",
		RecommendedAction: "Remove the reference, or move it into a clearly labeled cross-tool compatibility note so it reads as context rather than instruction.",
	},
	securityFamilyPermissionDefaultMode: {
		Summary:           "The default permission mode is set to `{ref}`, which auto-approves agent actions that would otherwise wait for human review. A relaxed default applies more broadly than any single grant, so it weakens the human check wherever that mode takes effect.",
		RecommendedAction: "Set the default mode back to a prompting mode so actions are reviewed before they run, and grant narrow exceptions only where you need them.",
	},
	securityFamilyPermissionGrant: {
		Summary:           "This project grants `{ref}`, letting the agent act without approval and across a wider scope than a typical task needs. One over-broad grant means a single mistaken or unexpected instruction can reach far more than the files or commands the task was actually about.",
		RecommendedAction: "Narrow the grant to the specific commands, paths, or domains the agent actually needs (for example, `Edit(src/**)` or a single `WebFetch(domain:...)`), or move it to the deny list if it should not be auto-approved at all.",
	},
	securityFamilyMcpProdPointed: {
		Summary:           "An MCP server in use points at a production environment (`{ref}`). An agent connected to it can read or act on live systems and real data, so a wrong step here has real-world consequences instead of staying contained to a test setup.",
		RecommendedAction: "Repoint this server at a non-production environment, or restrict it so agents cannot reach production through it.",
	},
	securityFamilyMcpSprawl: {
		Summary:           "Several near-duplicate MCP servers are in use (`{ref}`). When the same capability is configured more than once, it is easy to update one and miss the others, leaving stale or inconsistent settings that are hard to reason about.",
		RecommendedAction: "Consolidate the duplicates down to one canonical server and remove the rest.",
	},
	securityFamilyCoverageGap: {
		Summary:           "Your org has had risky content flagged before, and these repos (`{ref}`) have no hook that screens prompts before they run. Without that check, a risky prompt is only caught after it has been sent, not before.",
		RecommendedAction: "Add a prompt-screening hook (UserPromptSubmit) to each of these repos so prompts are checked before they execute.",
	},
	instructionIntentFamilyTokenFloor: {
		Summary:           "This file loads on every task and is large (`{ref}`). Everything in an always-loaded file competes for your agent's context on every request, so an oversized one crowds out the task's own material and can dilute the guidance that matters.",
		RecommendedAction: "Trim it to the essentials, or split the rarely needed parts into files that load only when they are relevant.",
	},
	instructionIntentFamilyTokPerLine: {
		Summary:           "This file packs a lot into very few lines (`{ref}` — denser than most files in the repo). Long, dense lines are harder for your agent to parse reliably, so individual instructions are more likely to be missed.",
		RecommendedAction: "Break the long lines into short, scannable bullets so each instruction stands on its own.",
	},
	instructionIntentFamilyDirectiveShare: {
		Summary:           "This file is mostly prose with few actionable directives (`{ref}`). When the instructions your agent is meant to act on are buried in explanation, they are easy to overlook.",
		RecommendedAction: "Restructure the prose into clear, imperative instructions and move the background explanation out of the way.",
	},
	behavioralFamilyDeviation: {
		// Fallback only — findingCopyFor derives the real Summary from the band numbers and never reads
		// this string (see the map doc above). Kept as a sensible default should that special-case go away.
		Summary:           "This skill behaves off its stated intent, based on how it's actually used.",
		RecommendedAction: "Check whether the skill is doing the job its description claims — either the stated purpose is wrong, or it is behaving off-intent and needs fixing.",
	},
	personalFamilyNeverUsed: {
		Summary:           "This personal artifact is installed and enabled but has not been used (`{ref}`). An unused skill, command, agent or hook still costs attention and context every time the tool loads it, and it may have quietly stopped fitting how you work.",
		RecommendedAction: "Try it on a task it was meant for, or remove it if it no longer earns its place in your setup.",
	},
	personalFamilyShadowsCommitted: {
		Summary:           "Your personal copy shares its name with the team's committed version (`{ref}`) but has different content. The tool resolves one of the two by name, so you may be running a version that has drifted from what the team maintains.",
		RecommendedAction: "Compare your copy with the committed one and either adopt the team's version or propose your changes upstream, then delete the personal copy.",
	},
}

// findingCopyFor returns the user-facing copy for a family, with the reference_point (and intent_class,
// for behavioral) interpolated. An unknown family returns empty copy (advisory bare finding).
func findingCopyFor(family, referencePoint, intentClass string) findingCopy {
	if family == behavioralFamilyDeviation {
		return findingCopy{
			Summary:           humanizeBehavioralBand(referencePoint, intentClass),
			RecommendedAction: findingCopyByFamily[behavioralFamilyDeviation].RecommendedAction,
		}
	}
	copyForFamily, ok := findingCopyByFamily[family]
	if !ok {
		return findingCopy{}
	}
	return findingCopy{
		Summary:           strings.ReplaceAll(copyForFamily.Summary, "{ref}", referencePoint),
		RecommendedAction: copyForFamily.RecommendedAction,
	}
}

// behavioralBandClause matches one clause of a behavioral reference_point as emitted by
// evaluateBands (behavioral.go), e.g. "completion 0.00 outside [0.40,1.00]" or the percent-suffixed
// "effective-context-use 75% outside [10%,50%]". The value/bound groups capture the optional trailing
// '%' so the humanized phrase keeps each metric's unit — keep this in lockstep with evaluateBands'
// format strings (TestHumanizeBehavioralBand pins the exact strings it produces).
var behavioralBandClause = regexp.MustCompile(`([a-z-]+)\s+([0-9.]+%?)\s+outside\s+\[([0-9.]+%?),\s*([0-9.]+%?)\]`)

// behavioralMetric describes how one band metric is humanized: the noun phrase, the scale applied to the
// raw band number, and the unit suffix. completion is a 0..1 rate shown as a percent (×100 + "%");
// tool-errors is a raw count (×1, no unit); effective-context-use is already a percent in the band
// (×1 + "%"). Keep the keys exactly as evaluateBands (behavioral.go) emits them.
type behavioralMetric struct {
	phrase string
	scale  float64
	unit   string
}

var behavioralMetricByName = map[string]behavioralMetric{
	"completion":            {phrase: "its task-completion rate", scale: 100, unit: "%"},
	"tool-errors":           {phrase: "its tool-error count", scale: 1, unit: ""},
	"effective-context-use": {phrase: "its context usage", scale: 1, unit: "%"},
}

// formatBandNumber renders a band number with its unit and NO trailing zeros — "0.00" → "0", "1.00" →
// "1", "0.40"×100 → "40", "99.00" → "99" — so the summary reads "0%" / "99" / "40%–100%" instead of the
// unitless, decimal-padded raw band. Formatting to 2 decimals first ROUNDS away the float artifacts
// scaling introduces (0.4×100 is 40.00000000000001, not 40); the band source is already ≤2-decimal, so
// no real precision is lost. An unparseable value degrades to itself (minus any '%') + unit.
func formatBandNumber(raw string, metric behavioralMetric) string {
	// A malformed band number is a deliberate degrade to its raw text (see the doc above): this pure
	// formatter has no error return or ctx to log, and evaluateBands — the sole producer — always emits a
	// well-formed number, so the error path is unreachable in practice.
	n, err := strconv.ParseFloat(strings.TrimSuffix(raw, "%"), 64)
	if err != nil {
		return strings.TrimSuffix(raw, "%") + metric.unit
	}
	return trimTrailingZeros(strconv.FormatFloat(n*metric.scale, 'f', 2, 64)) + metric.unit
}

// trimTrailingZeros drops a decimal number's trailing zeros and a dangling dot ("40.00" → "40",
// "37.50" → "37.5", "0" → "0"); a value with no decimal point is returned unchanged.
func trimTrailingZeros(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}

// humanizeBehavioralBand turns a behavioral finding's machine band string into a plain sentence, with
// each metric's proper unit and no decimal padding. The band string is detector-produced (e.g.
// "completion 0.00 outside [0.40,1.00]; tool-errors 99.00 outside [0.00,3.00]") → "…its task-completion
// rate is 0%, outside the healthy range 40%–100%; its tool-error count is 99, outside the healthy range
// 0–3." An unknown metric falls back to its raw name + unscaled number. If nothing parses, it degrades
// to a generic sentence rather than dumping the raw band.
func humanizeBehavioralBand(band, intentClass string) string {
	subject := "This skill"
	if intentClass != "" {
		subject = fmt.Sprintf("This %s skill", intentClass)
	}
	var clauses []string
	for _, match := range behavioralBandClause.FindAllStringSubmatch(band, -1) {
		metricName, value, low, high := match[1], match[2], match[3], match[4]
		metric, ok := behavioralMetricByName[metricName]
		if !ok {
			metric = behavioralMetric{phrase: metricName, scale: 1}
		}
		clauses = append(clauses, fmt.Sprintf("%s is %s, outside the healthy range %s–%s",
			metric.phrase, formatBandNumber(value, metric), formatBandNumber(low, metric), formatBandNumber(high, metric)))
	}
	if len(clauses) == 0 {
		return fmt.Sprintf("%s is behaving unlike a typical %s skill, based on how it's actually used.", subject, defaultIntent(intentClass))
	}
	return fmt.Sprintf("%s is behaving unlike a typical %s skill: %s.", subject, defaultIntent(intentClass), strings.Join(clauses, "; "))
}

// defaultIntent returns the intent class for prose, or a neutral word when unset.
func defaultIntent(intentClass string) string {
	if intentClass == "" {
		return "skill of its kind"
	}
	return intentClass
}
