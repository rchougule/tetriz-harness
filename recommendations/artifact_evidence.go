// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"math"
	"strings"

	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
)

// buildArtifactEvidence assembles the full ArtifactEvidence stored in the content store by
// joining the artifact's derive output with the run's aggregated stats: each
// addressed taxonomy issue is grounded in the org-grain high-vs-low gap, the
// project's review themes are carried as supporting evidence, and the org-grain
// PR correlation (same value for every artifact) is attached.
func buildArtifactEvidence(gradedArtifact GradedArtifact, aggCtx *promptlibraryv1.AggregatedContext, prCorrelation *promptlibraryv1.PRCorrelation) *promptlibraryv1.ArtifactEvidence {
	artifact := gradedArtifact.Artifact
	lowTotal := int(aggCtx.GetSessionSummary().GetQualityDistribution()["low"])

	taxonomyIssues := make([]*promptlibraryv1.TaxonomyIssueEvidence, 0, len(artifact.AddressesIssues))
	for _, slug := range artifact.AddressesIssues {
		gap := issueGapBySlug(aggCtx, slug)
		if gap == nil {
			// Security slugs come from security_flags, a separate source from the
			// session quality gap, so they never appear in HighVsLowGap — and a
			// securityOnly run strips the general signals the gap path reads. Project
			// the slug's incident count from SecuritySummary into incident_count (a
			// discrete event count, NOT sessions_affected) so a surfaced security
			// rule's card shows a real badge + "N incidents" without implying a ratio
			// against total_sessions (which is 0 on a securityOnly run).
			if incidents := securityIncidentCount(aggCtx.GetSecurityIncidents(), slug); incidents > 0 {
				taxonomyIssues = append(taxonomyIssues, &promptlibraryv1.TaxonomyIssueEvidence{
					// Store the CANONICAL kebab-case slug, not the raw addresses_issues
					// string: the gate accepts case/whitespace variants, but downstream
					// badges/filters key on the taxonomy slug, so a noncanonical value
					// (e.g. "  API-Key-Leak ") would miss them.
					Slug:          strings.ToLower(strings.TrimSpace(slug)),
					IncidentCount: incidents,
				})
			}
			continue // a slug with no matching org-grain gap (and no incident) is not fabricated
		}
		taxonomyIssues = append(taxonomyIssues, &promptlibraryv1.TaxonomyIssueEvidence{
			Slug:             slug,
			HighSessionPct:   gap.GetHighPct(),
			LowSessionPct:    gap.GetLowPct(),
			GapPp:            gap.GetGapPp(),
			SessionsAffected: affectedSessions(gap.GetLowPct(), lowTotal),
		})
	}
	// The card headline (gap_pp + affected) describes the primary quality gap — the
	// first taxonomy issue with a non-zero cohort split, selected by the same
	// firstQualityGap helper buildEvidenceProjection uses, so the headline, the inline
	// percentages, and the top issue all describe ONE issue (a leading security/incident
	// slug, or a degenerate all-zero gap, is skipped).
	var primaryGapPp float32
	var primaryAffected int32
	if pg := firstQualityGap(taxonomyIssues); pg != nil {
		primaryGapPp = pg.GetGapPp()
		primaryAffected = pg.GetSessionsAffected()
	}

	reviewThemes := artifactReviewThemes(aggCtx.GetReviewThemes())

	return &promptlibraryv1.ArtifactEvidence{
		TaxonomyIssues:   taxonomyIssues,
		ReviewThemes:     reviewThemes,
		TotalSessions:    aggCtx.GetSessionSummary().GetTotalSessions(),
		AffectedSessions: primaryAffected,
		HighLowGapPp:     primaryGapPp,
		PrCorrelation:    prCorrelation,
		Source:           evidenceSource,
		Summary:          artifact.EvidenceSummary,
	}
}

// artifactReviewThemes selects the review themes an artifact's evidence carries: the run's
// full set of classified themes, which the artifact's body was written from. The UI renders
// them as the example occurrences behind the artifact's why-text.
func artifactReviewThemes(themes []*promptlibraryv1.ReviewThemeSummary) []*promptlibraryv1.ReviewThemeEvidence {
	evidence := make([]*promptlibraryv1.ReviewThemeEvidence, 0, len(themes))
	for _, theme := range themes {
		evidence = append(evidence, &promptlibraryv1.ReviewThemeEvidence{
			Theme:    theme.GetTheme(),
			Count:    theme.GetCount(),
			Examples: theme.GetExamples(),
		})
	}
	return evidence
}

// buildEvidenceProjection derives the small inline card projection from the full
// evidence (so the two never disagree) and points its s3_ref at the full object.
func buildEvidenceProjection(gradedArtifact GradedArtifact, evidence *promptlibraryv1.ArtifactEvidence, evidenceKey string) *promptlibraryv1.EvidenceProjection {
	// Take the top issue from the GROUNDED taxonomy evidence, not the raw
	// addresses_issues — an ungrounded slug (dropped from the evidence because it
	// has no org-grain gap) must not lead the card while the full evidence points
	// elsewhere. Empty when nothing grounded.
	// The projection headline must describe the SAME issue as the inline gap numbers:
	// HighLowGapPp / AffectedSessions come from the first grounded QUALITY gap (set in
	// buildArtifactEvidence), but a security slug can occupy taxonomy slot 0 with only
	// an incident count and no gap. Sourcing low/high pct from grounded[0] would then
	// pair a real gap (non-zero HighLowGapPp) with zero percentages and a mismatched
	// top issue, so the inline "X% low vs Y% high" line vanishes exactly for security-
	// first artifacts. Lead with the first quality gap; fall back to grounded[0] only
	// for a security/incident-only artifact, which has no per-cohort gap to show.
	topIssue := ""
	var incidentCount int32
	var lowPct, highPct float32
	grounded := evidence.GetTaxonomyIssues()
	if primaryGap := firstQualityGap(grounded); primaryGap != nil {
		topIssue = primaryGap.GetSlug()
		lowPct = primaryGap.GetLowSessionPct()
		highPct = primaryGap.GetHighSessionPct()
	} else if len(grounded) > 0 {
		topIssue = grounded[0].GetSlug()
		incidentCount = grounded[0].GetIncidentCount()
	}
	var prDeltaPct float32
	if evidence.GetPrCorrelation() != nil {
		prDeltaPct = evidence.GetPrCorrelation().GetDeltaPct()
	}
	return &promptlibraryv1.EvidenceProjection{
		Summary:          capChars(evidence.GetSummary(), evidenceProjectionSummaryMaxChars),
		TopIssue:         topIssue,
		TotalSessions:    evidence.GetTotalSessions(),
		AffectedSessions: evidence.GetAffectedSessions(),
		HighLowGapPp:     evidence.GetHighLowGapPp(),
		PrDeltaPct:       prDeltaPct,
		IncidentCount:    incidentCount,
		LowPct:           lowPct,
		HighPct:          highPct,
		Source:           evidenceSource,
		Ref:              &promptlibraryv1.ContentRef{Key: evidenceKey},
	}
}

// firstQualityGap returns the first taxonomy issue carrying an org-grain quality gap
// (a cohort split: gap_pp / low / high / affected sessions), skipping discrete
// security-incident slugs that have only an incident_count. nil when the artifact
// grounds no quality gap. Mirrors the primary-gap selection in buildArtifactEvidence
// so the projection's headline and its inline gap numbers describe the same issue.
func firstQualityGap(issues []*promptlibraryv1.TaxonomyIssueEvidence) *promptlibraryv1.TaxonomyIssueEvidence {
	for _, issue := range issues {
		if issue.GetGapPp() != 0 || issue.GetLowSessionPct() != 0 || issue.GetHighSessionPct() != 0 || issue.GetSessionsAffected() != 0 {
			return issue
		}
	}
	return nil
}

// issueGapBySlug returns the org-grain gap for the taxonomy slug, or nil.
func issueGapBySlug(aggCtx *promptlibraryv1.AggregatedContext, slug string) *promptlibraryv1.IssueGap {
	for _, gap := range aggCtx.GetHighVsLowGap() {
		if gap.GetIssueSlug() == slug {
			return gap
		}
	}
	return nil
}

// securityIncidentCount returns the windowed incident count for a security
// taxonomy slug, or 0 if the slug is not a security slug (or has no incidents).
// Security slugs come from security_flags, not the session quality gap, so they
// never appear in HighVsLowGap — a security rule's card evidence is projected from
// the SecuritySummary instead. Nil-safe (nil summary → 0). The slug is normalized
// (lower + trim) to match the evidence-grading path (addressesSecurityEvidence).
func securityIncidentCount(security *promptlibraryv1.SecuritySummary, slug string) int32 {
	switch strings.ToLower(strings.TrimSpace(slug)) {
	case slugAPIKeyLeak:
		return security.GetApiKeyLeak()
	case slugPIIInjection:
		return security.GetPiiInjection()
	case slugDBCredentialLeak:
		return security.GetDbCredentialLeak()
	default:
		return 0
	}
}

// affectedSessions estimates how many low-bucket sessions exhibited an issue from
// its low-cohort presence rate and the low-bucket total. Zero total → zero.
func affectedSessions(lowPct float32, lowTotal int) int32 {
	if lowTotal <= 0 {
		return 0
	}
	return int32(math.Round(float64(lowPct) / 100 * float64(lowTotal)))
}

// capChars truncates s to at most maxChars runes (not bytes, so a multibyte
// character is never split).
func capChars(s string, maxChars int) string {
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	return string(runes[:maxChars])
}
