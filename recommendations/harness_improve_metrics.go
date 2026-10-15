// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// Harness-improve finding-quality lane — the counterpart to scan_runs_total (run outcomes) and the
// behavioral_* counters (bridge precision). These meter WHAT a scan produced: which dimensions ×
// severities actually surface, how many findings the suppression gate hides and why, and how long a
// successful scan takes. No org_id label — a per-org cardinality blow-up no consumer of these needs
// (org-grain drill-down belongs in operator logs, not a Prometheus metric). These mirror the existing
// package convention (NewCounterVec/NewHistogram declared at package scope and registered via the
// opt-in RegisterMetrics, full-label seeding) rather than promauto, so all harness_improve series are
// declared the same way.

// The dimension label values for findings_emitted_total — the complete set, reversing
// dimensionLabel. UNSPECIFIED and a nil dimension both fold to "unspecified".
const (
	dimensionConflict          = "conflict"
	dimensionSecurity          = "security"
	dimensionInstructionIntent = "instruction_intent"
	dimensionUnspecified       = "unspecified"
)

// The severity label values for findings_emitted_total — the complete set, reversing
// priorityToSeverityLabel. An unrecognized priority folds to "unspecified" (distinct from the
// priorityToInt default of medium: a metric label must not silently attribute an unknown rank).
const (
	severityCritical    = "critical"
	severityHigh        = "high"
	severityMedium      = "medium"
	severityLow         = "low"
	severityUnspecified = "unspecified"
)

// The reason label values for gate_hidden_total — the two persisted-hide causes, mirroring
// isSuppressed's precedence (an admin dismissal is checked before the recent-hidden set).
const (
	gateHiddenReasonDismissed      = "dismissed"
	gateHiddenReasonRecentlyHidden = "recently_hidden"
)

// harnessImproveFindingsEmittedTotal meters findings that actually SURFACE (VISIBLE at persist),
// split by dimension and severity, so the mix of what a scan tells an org — and a shift in it (e.g. a
// security spike, or a dimension flatlining to zero after a detector regression) — is visible.
var harnessImproveFindingsEmittedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "recommendations",
	Subsystem: "harness_improve",
	Name:      "findings_emitted_total",
	Help:      "Harness-improve VISIBLE findings that persisted, by dimension and severity.",
}, []string{"dimension", "severity"})

// harnessImproveGateHiddenTotal meters findings the suppression gate flipped VISIBLE→HIDDEN, by
// reason (dismissed = an admin-dismissed slug; recently_hidden = a recent-hidden (type, slug)). A
// rising share means the gate is hiding more of what the detectors produce. Incremented only on the
// committed terminal (via meterGateHidden), so it is attributed to the same committed runs as
// findings_emitted_total — never to a superseded or failed attempt.
var harnessImproveGateHiddenTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "recommendations",
	Subsystem: "harness_improve",
	Name:      "gate_hidden_total",
	Help:      "Harness-improve findings suppressed by the gate, by reason (dismissed|recently_hidden).",
}, []string{"reason"})

// harnessImproveScanDurationSeconds records wall-clock of SUCCESSFUL scans only (Run/RunOrgScan entry
// to the COMPLETED/COMPLETED_NO_VISIBLE write), mirroring derivationDurationSeconds: a failed scan can
// be a fast early error and a reaped/skipped scan does no full work, so observing only the success
// path reflects true scan cost. Buckets mirror derivationDurationSeconds.
var harnessImproveScanDurationSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
	Namespace: "recommendations",
	Subsystem: "harness_improve",
	Name:      "scan_duration_seconds",
	Help:      "Wall-clock of a successful harness-improve scan (Run/RunOrgScan entry to the terminal write).",
	Buckets:   []float64{10, 30, 60, 120, 300, 600, 900, 1200, 1800},
})

// registerHarnessImproveFindingMetrics registers the harness-improve finding/gate metrics on
// reg. Opt-in (no import-time side effect): the package's RegisterMetrics calls it at startup.
func registerHarnessImproveFindingMetrics(reg prometheus.Registerer) {
	reg.MustRegister(harnessImproveFindingsEmittedTotal, harnessImproveGateHiddenTotal, harnessImproveScanDurationSeconds)
	// Seed the full label sets so every cell shows on the dashboard from the first scrape (a
	// never-yet-observed label reads as 0, not an absent series).
	for _, dimension := range []string{dimensionConflict, dimensionSecurity, dimensionInstructionIntent} {
		for _, severity := range []string{severityCritical, severityHigh, severityMedium, severityLow} {
			harnessImproveFindingsEmittedTotal.WithLabelValues(dimension, severity).Add(0)
		}
	}
	for _, reason := range []string{gateHiddenReasonDismissed, gateHiddenReasonRecentlyHidden} {
		harnessImproveGateHiddenTotal.WithLabelValues(reason).Add(0)
	}
}

// dimensionLabel maps a finding's dimension enum to its metric label. A nil pointer or the UNSPECIFIED
// value both fold to "unspecified" — an ORG/behavioral finding always carries one, but the label must
// never panic on a nil or emit a raw enum name.
func dimensionLabel(dimension *promptlibraryv1.HarnessFindingDimension) string {
	if dimension == nil {
		return dimensionUnspecified
	}
	switch *dimension {
	case promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_CONFLICT:
		return dimensionConflict
	case promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_SECURITY:
		return dimensionSecurity
	case promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_INSTRUCTION_INTENT:
		return dimensionInstructionIntent
	default:
		return dimensionUnspecified
	}
}

// priorityToSeverityLabel maps a finding's priority INT to its severity metric label — the reverse of
// priorityToInt (critical=30/high=20/medium=10/low=5). Unlike priorityToInt (which defaults an unknown
// value to medium for ordering), an unrecognized rank here folds to "unspecified": a metric label must
// not silently attribute an unstamped/zero row to a real severity.
func priorityToSeverityLabel(priority int32) string {
	switch priority {
	case priorityCritical:
		return severityCritical
	case priorityHigh:
		return severityHigh
	case priorityMedium:
		return severityMedium
	case priorityLow:
		return severityLow
	default:
		return severityUnspecified
	}
}

// meterFindingsEmitted increments findings_emitted_total for each VISIBLE finding in the persisted set,
// keyed by dimension × severity. It is called only on the successful terminal (the run that owned the
// transition and committed), so it counts findings that actually persisted. VISIBLE-only: a finding the
// gate flipped to HIDDEN is metered by gate_hidden_total, not here.
func meterFindingsEmitted(gated []*model.PromptLibraryItem) {
	for _, finding := range gated {
		if finding.SurfaceState != promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE {
			continue
		}
		harnessImproveFindingsEmittedTotal.WithLabelValues(
			dimensionLabel(finding.Dimension), priorityToSeverityLabel(finding.Priority)).Inc()
	}
}

// meterGateHidden increments gate_hidden_total once per suppressed finding this run, by reason. Called
// only on the committed terminal (alongside meterFindingsEmitted), off the reason labels gateFindings
// collected — so a superseded or failed attempt, which never commits, is never counted, keeping
// gate_hidden attributed to the same committed runs as findings_emitted.
func meterGateHidden(reasons []string) {
	for _, reason := range reasons {
		harnessImproveGateHiddenTotal.WithLabelValues(reason).Inc()
	}
}

// harnessGateHiddenReason returns the gate_hidden_total reason label when the slug is suppressed, or ""
// when it is not — the reason-carrying parallel of isSuppressed, with the SAME precedence (an admin
// dismissal wins over the recent-hidden set). It reuses the gate's already-read suppression sets, so it
// issues no DB read of its own.
func harnessGateHiddenReason(normalizedSlug string, artifactType promptlibraryv1.ArtifactType, dismissed map[string]struct{}, hidden map[typedSlugKey]struct{}) string {
	if normalizedSlug == "" {
		return "" // an empty slug is handled by validatorSlugMissing, not suppressed here
	}
	if _, ok := dismissed[normalizedSlug]; ok {
		return gateHiddenReasonDismissed
	}
	if _, ok := hidden[typedSlugKey{artifactType: artifactType, slug: normalizedSlug}]; ok {
		return gateHiddenReasonRecentlyHidden
	}
	return ""
}

// observeScanDuration records a successful scan's wall-clock since start. Called only on the success
// terminal (mirrors derivationDurationSeconds); never on a failed/skipped/reaped run.
func observeScanDuration(start time.Time) {
	harnessImproveScanDurationSeconds.Observe(time.Since(start).Seconds())
}
