// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"gorm.io/gorm"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/logger"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

const (
	// themeExamplesTopN is how many highest-confidence comments per review theme
	// are carried as examples so a derived skill/rule can quote the org's actual
	// review comments. A reasonable default, tunable later without a
	// contract change — selection lives here, not in the classify-themes service.
	themeExamplesTopN = 3
	// fdrQThreshold is the Benjamini-Hochberg q-value cutoff: q < threshold is a
	// primary recommendation. A gap clearing raw p < pValuePrimary
	// but failing FDR is surfaced as low_confidence rather than dropped.
	fdrQThreshold  = 0.10
	pValuePrimary  = 0.05
	dateLayout     = "2006-01-02"
	otherThemeSlug = "other"
	// defaultMaxReviewBodies is the floor the derivation flow enforces on the
	// review-body safety valve when DerivationConfig.MaxReviewBodies is non-positive
	// (a config typo / a dynamic push that zeroed it). The valve exists to bound
	// memory on a pathological unlimited-window repo, so it must not be disable-able
	// from config — a non-positive value clamps here rather than reading uncapped.
	defaultMaxReviewBodies = 100000

	// maxThemesPerCommentCeiling bounds the enriched-path top-N request to the classify
	// theme-set size. tetriz-py's classify-themes validates max_themes_per_comment to
	// [1, 13] (the engineering-theme count), so a dynamic-config value above it would 422
	// every enriched call and fail the whole run; clamp defensively at the call site.
	maxThemesPerCommentCeiling = 13

	// Security taxonomy slugs — the kebab-case category_slugs tetriz-py emits
	// (analyze_prompt.py) and security_flags.flag_type stores verbatim. They map
	// onto SecuritySummary's fixed per-incident fields (buildSecuritySummary) AND
	// gate which derived artifacts count as security-governance on a securityOnly
	// run (securitySlugsWithEvidence). The lookup keys MUST be kebab-case, not
	// underscored, or every count silently reads 0.
	slugAPIKeyLeak       = "api-key-leak"
	slugPIIInjection     = "pii-injection"
	slugDBCredentialLeak = "db-credential-leak"
)

// repoTreeTruncatedTotal counts RepoTree encodings emitted incomplete, labeled by
// reason (vendor | token_cap) — a warn signal that an org has an outlier repo or
// a vendor-truncated tree (design notes #49/#50). Registered once at package init.
var repoTreeTruncatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "recommendations_derivation_repo_tree_truncated_total",
	Help: "Count of RepoTree encodings truncated, by reason (vendor|token_cap).",
}, []string{"reason"})

// reviewBodiesTruncatedTotal counts derivation runs whose review-body read hit
// the MaxReviewBodies safety valve — a signal that a project tripped the OOM
// backstop and its theme classification saw only the most-recent N bodies. A
// non-zero value means the valve should be raised (or the org investigated), so
// the truncation is never silent.
var reviewBodiesTruncatedTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "recommendations_derivation_review_bodies_truncated_total",
	Help: "Count of derivation runs whose review-body read hit the MaxReviewBodies valve.",
})

// registerAggregatorMetrics registers the aggregator's metrics on reg. Opt-in (no
// import-time side effect): the package's RegisterMetrics calls it once at startup.
func registerAggregatorMetrics(reg prometheus.Registerer) {
	reg.MustRegister(repoTreeTruncatedTotal, reviewBodiesTruncatedTotal)
	// Seed both reason series at 0 so each exists before its first truncation. A
	// CounterVec series is created lazily, so otherwise the first scrape for a
	// reason could already read value 1 and increase()[6h] would be 0 — the
	// truncation warn alert would stay silent until the second truncation. Add(0)
	// materializes the child at 0 explicitly.
	for _, reason := range []TruncationReason{TruncationVendor, TruncationTokenCap} {
		repoTreeTruncatedTotal.WithLabelValues(string(reason)).Add(0)
	}
}

// DerivationParams are the DerivationConfig knobs the aggregator reads (decision
// 18/I5/#50). Supplied via a provider func so the engine can re-read the dynamic
// config per run rather than snapshot it at construction.
type DerivationParams struct {
	LowScoreMax       int // overallScore ≤ this is a "low" session
	HighScoreMin      int // overallScore ≥ this is a "high" session
	RepoTreeMaxTokens int // per-repo/per-call RepoTree encoding cap (0 = uncapped)
	MaxReviewBodies   int // per-project review-body safety valve (0 = uncapped)
	// ReviewClassifyLookbackDays bounds the review-comment classify window to the
	// trailing N days (clamped to the derivation-window floor); ≤0 = no extra clamp.
	ReviewClassifyLookbackDays int
	// ReviewClassifyMaxComments caps the review corpus fed to classification,
	// most-recent-first; ≤0 = fall back to MaxReviewBodies.
	ReviewClassifyMaxComments int
	// MinReviewCommentsPerRepo is the comment count at/above which a repo classifies
	// 1:1 (one theme per comment). Below it (and above the gate's enriched floor) the
	// repo is "enriched": each comment classifies into up to MaxReviewThemesPerComment
	// themes, so a bulky multi-topic comment credits every theme it spans.
	MinReviewCommentsPerRepo int
	// MaxReviewThemesPerComment is the top-N themes per comment on the enriched path
	// (the standard 1:1 path always uses 1).
	MaxReviewThemesPerComment int
}

// Aggregator builds the AggregatedContext that the derivation LLM consumes. Every
// session signal (quality gap + FDR, session summary, per-task-type, IDE
// breakdown) is scoped to the run's project via the session's reported repo, as is
// the project grounding (repo tree + review themes + admin feedback), so a per-repo
// derivation reflects that repo's own patterns. The security-incident summary remains
// org-grain (security_flags carries no repo dimension).
type Aggregator struct {
	insightDAO     dao.ISessionInsightDAO
	pullRequestDAO dao.IPullRequestDAO
	rawRecordDAO   dao.IRawRecordDAO
	securityDAO    dao.ISecurityFlagDAO
	repoDAO        dao.IRepositoryDAO
	repoProfileDAO dao.IVCSRepoProfileDAO
	libraryDAO     dao.IPromptLibraryDAO
	llmClient      *LLMClient
	// platformIdentityDAO resolves person_id → platform_identity_id for the PR-correlation join
	// (replacing the cross-service identity gRPC). May be nil — PRCorrelation then degrades to no signal.
	platformIdentityDAO dao.IPlatformIdentityMapDAO
	params              func() DerivationParams
}

// NewAggregator wires the aggregator. platformIdentityDAO may be nil — PRCorrelation then degrades to
// no signal rather than failing (the person↔platform_identity mapping is unavailable). params returns
// the current derivation knobs.
func NewAggregator(
	insightDAO dao.ISessionInsightDAO,
	pullRequestDAO dao.IPullRequestDAO,
	rawRecordDAO dao.IRawRecordDAO,
	securityDAO dao.ISecurityFlagDAO,
	repoDAO dao.IRepositoryDAO,
	repoProfileDAO dao.IVCSRepoProfileDAO,
	libraryDAO dao.IPromptLibraryDAO,
	llmClient *LLMClient,
	platformIdentityDAO dao.IPlatformIdentityMapDAO,
	params func() DerivationParams,
) *Aggregator {
	return &Aggregator{
		insightDAO:          insightDAO,
		pullRequestDAO:      pullRequestDAO,
		rawRecordDAO:        rawRecordDAO,
		securityDAO:         securityDAO,
		repoDAO:             repoDAO,
		repoProfileDAO:      repoProfileDAO,
		libraryDAO:          libraryDAO,
		llmClient:           llmClient,
		platformIdentityDAO: platformIdentityDAO,
		params:              params,
	}
}

// BuildContext assembles the AggregatedContext for a single ARTIFACT_SCOPE_PROJECT
// derivation run over [from, to]. Every session signal (session summary,
// high-vs-low gap + FDR, per-task-type, IDE) is scoped to the run's repo via the
// session's reported repo, as is the project grounding (repo tree, review themes,
// admin feedback) resolved by id from `repoID` (the run's scope_value); a missing
// repo fails the run. The security-incident summary stays org-grain (security_flags
// carries no repo dimension). securityOnly steers to a
// security-governance-only context: it skips the general quality
// signals (session summary, gap, per-task-type, IDE, per-project) and review-theme
// classification at the READ — not computing then discarding them —
// keeping only the security evidence + repo tree + admin feedback to ground on.
//
// The DB reads carry no open transaction across the classify HTTP call: the
// classify step runs after the reads return, so no tx is held across the round-trip.
//
// newTypesEnabled gates the hook/agent grounding reads: they run only when the
// org is in the hooks-and-agents feature, since those signals feed only the new artifact
// types. It reflects the live feature verdict the caller resolved for this run.
//
// The returned enriched bool is true when the repo was below MinReviewCommentsPerRepo and
// its review comments were therefore classified into up to MaxReviewThemesPerComment themes
// each (the enriched path); it is false for a standard (≥floor, 1:1) or securityOnly run.
func (a *Aggregator) BuildContext(ctx context.Context, orgID, repoID string, from, to time.Time, securityOnly, newTypesEnabled bool) (*promptlibraryv1.AggregatedContext, bool, int, error) {
	params := a.params()
	// The valve must not be disable-able from config — clamp a non-positive value
	// to the built-in floor so a pathological repo can't OOM the run.
	maxReviewBodies := params.MaxReviewBodies
	if maxReviewBodies <= 0 {
		maxReviewBodies = defaultMaxReviewBodies
	}
	// The classify cap (most-recent-first) tightens the corpus below the safety
	// valve when set — a monthly regen classifies recent themes, not all history.
	if params.ReviewClassifyMaxComments > 0 && params.ReviewClassifyMaxComments < maxReviewBodies {
		maxReviewBodies = params.ReviewClassifyMaxComments
	}
	// Narrow the review-body window to the trailing lookback (shared with the admission
	// per-repo gate so the gate's review-count can't drift from the corpus this read
	// classifies). Applied ONLY to the review-body read — the quality and security
	// reads keep the broad [from, to] window.
	reviewFrom := ReviewClassifyWindow(from, to, params.ReviewClassifyLookbackDays)

	// Gather every DB-sourced input in one committed read scope; the content store read and
	// the classify HTTP call below run only AFTER it closes.
	var (
		gapBuckets     *dao.SessionGapBuckets
		sessionSummary *promptlibraryv1.SessionSummary
		perTaskType    []*promptlibraryv1.TaskTypeSummary
		ideBreakdown   []*promptlibraryv1.IDESummary
		security       *promptlibraryv1.SecuritySummary
		repo           *model.Repository
		adminFeedback  *promptlibraryv1.AdminFeedback
		reviewBodies   []string
		// reviewBodyCount is the count compared against the standard/enriched floor. It is
		// len(reviewBodies) except when the read truncated at a classify cap set below the
		// floor, where the read closure resolves it to the uncapped count.
		reviewBodyCount int
		// Grounding signals for the new hook/agent artifact types. Populated below in a
		// best-effort scope only when the hooks-and-agents feature is enabled and the run
		// is not securityOnly; otherwise the fields stay zero.
		signals groundingSignals
	)
	if err := scopedRead(ctx, func(sCtx context.Context) error {
		var err error
		// Resolve the project repo FIRST: its full_name scopes the session reads below
		// to this repo's sessions (repo-specific derivation), and grounding binds to
		// exactly the queued repo — a full_name shared across connectors (e.g. GitHub +
		// GitLab both exposing owner/repo) can't make a name match ambiguous and silently
		// drop the tree / themes / admin feedback while artifacts still persist under that
		// scope_value. A missing repo (deleted mid-window) fails the run.
		repo, err = a.repoDAO.GetByID(sCtx, orgID, repoID)
		if err != nil {
			return fmt.Errorf("resolving repo %s for org %s: %w", repoID, orgID, err)
		}
		// Scope every session signal to this repo's sessions so a per-repo derivation
		// reflects that repo's own patterns, not org-pooled data. A low-volume repo
		// naturally yields an empty gap (both cohorts must be populated) rather than
		// noise; the quality gate hides ungrounded output.
		//
		// Scope key caveat: unlike the grounding above (bound by repo ID via GetByID),
		// this filter matches only the session's reported repoName/projectName —
		// session_analyses has no connector_id — so sessions pool under one project's
		// derivation when two repos share the matched key: the SAME full_name under two
		// connectors (GitHub + GitLab both "owner/repo"), or two repos whose full_names
		// reduce to the same last-two segments (a/team/api and b/team/api — the desktop
		// stores only that reduced key; see WithVCSRepos). Accepted as a known limitation
		// (like the org-grain security summary below); disambiguating needs a connector
		// dimension on the session data.
		repoFilter := dao.WithVCSRepos([]string{repo.FullName})

		// General quality signals are read only when NOT securityOnly. A securityOnly
		// run steers to a security-governance-only context: a below-threshold
		// org reaches Stage 3 only on a security incident, so these signals are
		// statistically underpowered and must not prompt general rules. Skipping the
		// reads (rather than computing then discarding them) is also why a below-threshold
		// derive doesn't pay for aggregations it never uses. The quality gate is the
		// backstop that hides any general artifact the model still emits.
		if !securityOnly {
			// The gap query (a taxonomyIssues unnest) is computed ONCE and shared: its
			// bucket totals feed the session quality distribution and its per-issue counts
			// feed the FDR pass — running it twice would re-scan and risk read-skew.
			gapBuckets, err = a.insightDAO.AggregateHighVsLowGap(sCtx, orgID, from, to, params.LowScoreMax, params.HighScoreMin, repoFilter)
			if err != nil {
				return fmt.Errorf("aggregating high-vs-low gap for org %s: %w", orgID, err)
			}
			sessionSummary, err = a.buildSessionSummary(sCtx, orgID, from, to, gapBuckets, repoFilter)
			if err != nil {
				return fmt.Errorf("building session summary for org %s: %w", orgID, err)
			}
			perTaskType, err = a.buildTaskTypeBreakdown(sCtx, orgID, from, to, repoFilter)
			if err != nil {
				return fmt.Errorf("building task-type breakdown for org %s: %w", orgID, err)
			}
			ideBreakdown, err = a.buildIDEBreakdown(sCtx, orgID, from, to, repoFilter)
			if err != nil {
				return fmt.Errorf("building ide breakdown for org %s: %w", orgID, err)
			}
		}
		// security_incidents is ORG-grain: security_flags carries no repo dimension and
		// CountByFlagType takes no repo filter, so it cannot be repo-scoped the way the
		// session reads above are. The incident counts are a coarse governance signal,
		// not the per-repo quality gap.
		security, err = a.buildSecuritySummary(sCtx, orgID, from, to)
		if err != nil {
			return fmt.Errorf("building security summary for org %s: %w", orgID, err)
		}
		adminFeedback, err = a.buildAdminFeedback(sCtx, orgID, repo.ID)
		if err != nil {
			return fmt.Errorf("building admin feedback for repo %s in org %s: %w", repoID, orgID, err)
		}
		if !securityOnly {
			reviewBodies, err = a.rawRecordDAO.ListReviewBodies(sCtx, orgID, []string{repo.FullName}, []string{repo.ConnectorID}, reviewFrom, to, maxReviewBodies)
			if err != nil {
				return fmt.Errorf("listing review bodies for repo %s in org %s: %w", repoID, orgID, err)
			}
			// The standard/enriched boundary is decided on the count. len(reviewBodies) is
			// capped at maxReviewBodies, so it is exact for the boundary only while the cap is
			// ≥ the floor (the only real config). When the read truncated at a cap set BELOW the
			// floor, len understates the true count — resolve it with the uncapped count the
			// admission gate uses (same window) so the enriched decision is exact regardless of how
			// the cap is tuned. Unreachable under the real config (cap ≫ floor).
			reviewBodyCount = len(reviewBodies)
			if reviewBodyCount == maxReviewBodies && maxReviewBodies < params.MinReviewCommentsPerRepo {
				reviewBodyCount, err = a.gateReader().CountReviewBodiesForRepo(sCtx, orgID, repo.FullName, repo.ConnectorID, reviewFrom, to)
				if err != nil {
					return fmt.Errorf("counting review bodies for enriched decision, repo %s in org %s: %w", repoID, orgID, err)
				}
			}
		}
		return nil
	}); err != nil {
		return nil, false, 0, fmt.Errorf("gathering aggregated context reads for org %s: %w", orgID, err)
	}
	project := repo.FullName

	// The per-project session map is best-effort grounding (not load-bearing
	// evidence), so it runs in its OWN scope: AggregateSessionsByProject can fail
	// and abort its transaction, and isolating it keeps that abort from poisoning
	// the required-reads tx above and failing the whole run. A failure (the query
	// or the scope's own commit) degrades to a minimal map, never an error. It is a
	// general signal, so a securityOnly run skips it (left nil).
	var projectMap []*promptlibraryv1.ProjectSummary
	if !securityOnly {
		projectMap = []*promptlibraryv1.ProjectSummary{{Project: project, RepoFullName: project}}
		if err := scopedRead(ctx, func(sCtx context.Context) error {
			projectMap = a.buildProjectMap(sCtx, orgID, project, project, from, to)
			return nil
		}); err != nil {
			logger.WarnWithCtx(ctx, "prompt-library: per-project session map unavailable, using minimal map",
				zap.String(logger.ORG_ID, orgID), zap.String("project", project), zap.Error(err))
			projectMap = []*promptlibraryv1.ProjectSummary{{Project: project, RepoFullName: project}}
		}
	}

	// Hook/agent grounding runs only when the feature is enabled for the org: these
	// signals feed only the new artifact types, so an org without the feature neither
	// pays for the extra PR/bug/usage-metadata reads nor risks its rule/template/skill
	// derivation failing on one of them. Like the per-project map above it is best-effort
	// in its OWN scope — a read failure degrades the hook/agent grounding to empty and is
	// logged, never failing the run (and never poisoning the required-reads tx). A
	// securityOnly run skips it (that run only surfaces the secret hook, which needs none).
	if newTypesEnabled && !securityOnly {
		if err := scopedRead(ctx, func(sCtx context.Context) error {
			// PR outcome reads scope by the physical repo (connector_id, platform_repo_id)
			// so a display-name collision across connectors or a rename can't pool another
			// repo's PRs; the session reads keep the full_name scope (session_analyses
			// carries no connector dimension — the documented same-key pooling limit).
			prIdentity := dao.WithPullRequestRepoIdentities([]dao.ConnectorRepo{{ConnectorID: repo.ConnectorID, PlatformRepoID: repo.PlatformRepoID}})
			var scopedErr error
			signals, scopedErr = a.readGroundingSignals(sCtx, orgID, repo.ID, repo.FullName, prIdentity, from, to, dao.WithVCSRepos([]string{repo.FullName}))
			if scopedErr != nil {
				return fmt.Errorf("reading grounding signals for repo %s: %w", repo.ID, scopedErr)
			}
			return nil
		}); err != nil {
			logger.WarnWithCtx(ctx, "prompt-library: hook/agent grounding signals unavailable, deriving without them",
				zap.String(logger.ORG_ID, orgID), zap.String("project", project), zap.Error(err))
			signals = groundingSignals{}
		}
	}

	// Reads are committed; the content store read + classify HTTP run with no tx open.
	if len(reviewBodies) == maxReviewBodies {
		reviewBodiesTruncatedTotal.Inc()
		logger.WarnWithCtx(ctx, "prompt-library: review-body read hit the MaxReviewBodies valve; classification saw only the most-recent bodies",
			zap.String(logger.ORG_ID, orgID), zap.String("project", project), zap.Int("max_review_bodies", maxReviewBodies))
	}

	aggregated := &promptlibraryv1.AggregatedContext{
		OrgId: orgID,
		DateRange: &commonv1.DateRange{
			DateFrom: from.Format(dateLayout),
			DateTo:   to.Format(dateLayout),
		},
		SessionSummary:    sessionSummary,
		HighVsLowGap:      gapsWithFDR(gapBuckets),
		PerTaskType:       perTaskType,
		IdeBreakdown:      ideBreakdown,
		SecurityIncidents: security,
		PerProject:        projectMap,
		SubagentUsage:     signals.subagentUsage,
		Languages:         signals.languages,
		ModelsUsed:        signals.modelsUsed,
		McpServers:        signals.mcpServers,
		PrSignals:         signals.prSignals,
		BugSignal:         signals.bugSignal,
		Friction:          signals.friction,
	}

	primaryLanguage := ""
	if repo.PrimaryLanguage != nil {
		primaryLanguage = *repo.PrimaryLanguage
	}
	treeContent := ""
	if repo.TreeContent != nil {
		treeContent = *repo.TreeContent
	}
	repoTree, hasTree, err := a.buildRepoTree(ctx, orgID, repo.FullName, primaryLanguage, treeContent, params)
	if err != nil {
		return nil, false, 0, fmt.Errorf("building repo tree for project %q in org %s: %w", project, orgID, err)
	}
	if hasTree {
		aggregated.RepoTrees = []*promptlibraryv1.RepoTree{repoTree}
	}
	// Resolve the single dominant AI tool once up front: ide_breakdown
	// is the primary signal, a checked-in tool-config dir in the RepoTree overrides
	// it. It grounds only the hook/agent format, so it is set only when the feature is
	// enabled — an org without the feature must see an unchanged AggregatedContext so its
	// existing rule/template/skill output is not perturbed. Read by the gate + target
	// resolver so they read one authoritative tool instead of guessing from the
	// LLM-authored target_file. On a securityOnly run ide_breakdown is nil, so this
	// resolves from the RepoTree alone. Set after the tree is attached so the override
	// sees the repo's checked-in dirs.
	if newTypesEnabled {
		aggregated.ResolvedTargetTool = resolveDominantTool(ideBreakdown, firstRepoTree(aggregated))
	}
	aggregated.AdminFeedback = adminFeedback

	// A repo below the comment floor is "enriched": classify each comment into up to
	// MaxReviewThemesPerComment themes so a bulky multi-topic comment credits every theme it
	// spans, and signal that to the engine (which applies the theme-count floor). A standard
	// (≥floor) repo stays 1:1 and enriched=false. The decision uses reviewBodyCount — the
	// UNCAPPED count when a below-floor cap truncated the read, else len(reviewBodies) — so it
	// is exact regardless of how the classify cap is tuned. It is derived from the count alone
	// (NOT gated on len>0): a below-floor repo whose derive-time read came back empty — e.g.
	// the classify window shifted after admission it — stays enriched with zero themes,
	// so the engine terminates it REPO_FEW_THEMES instead of paying for a derive with no
	// review evidence. Only the classify HTTP call is guarded on len>0 (nothing to classify).
	enriched := !securityOnly && params.MinReviewCommentsPerRepo > 0 && reviewBodyCount < params.MinReviewCommentsPerRepo
	if !securityOnly && len(reviewBodies) > 0 {
		maxThemes := 1
		if enriched {
			maxThemes = params.MaxReviewThemesPerComment
			if maxThemes < 1 {
				maxThemes = 1
			} else if maxThemes > maxThemesPerCommentCeiling {
				maxThemes = maxThemesPerCommentCeiling
			}
		}
		themes, err := a.classifyReviewBodies(ctx, orgID, reviewBodies, maxThemes)
		if err != nil {
			return nil, false, 0, fmt.Errorf("building review themes for project %q in org %s: %w", project, orgID, err)
		}
		aggregated.ReviewThemes = themes
	}

	return aggregated, enriched, reviewBodyCount, nil
}

// buildSessionSummary aggregates the repo-scoped session totals, dimension averages, and the
// low/mid/high quality distribution. All three bucket counts come from the gap
// query so the distribution and the gap agree on what "low"/"high"/"mid" mean
// (I5) — and they count only SCORED sessions. TotalSessions stays the full
// session count, so a session with no overallScore is in TotalSessions but in
// none of the three buckets (low+mid+high ≤ TotalSessions); the LLM reads the
// gap from the scored population without unscored sessions inflating "mid".
func (a *Aggregator) buildSessionSummary(ctx context.Context, orgID string, from, to time.Time, buckets *dao.SessionGapBuckets, filters ...dao.FilterOption) (*promptlibraryv1.SessionSummary, error) {
	orgSummary, err := a.insightDAO.AggregateByOrg(ctx, orgID, from, to, filters...)
	if err != nil {
		return nil, fmt.Errorf("aggregating org summary: %w", err)
	}
	dims, err := a.insightDAO.AggregateDimensionsByOrg(ctx, orgID, from, to, filters...)
	if err != nil {
		return nil, fmt.Errorf("aggregating org dimensions: %w", err)
	}

	return &promptlibraryv1.SessionSummary{
		TotalSessions: int32(orgSummary.TotalConversations),
		QualityDistribution: map[string]int32{
			"low":  int32(buckets.TotalLow),
			"mid":  int32(buckets.TotalMid),
			"high": int32(buckets.TotalHigh),
		},
		DimensionAverages: map[string]float32{
			"clarity":          float32(dims.Clarity),
			"specificity":      float32(dims.Specificity),
			"contextProvision": float32(dims.ContextProvision),
			"actionability":    float32(dims.Actionability),
			"completeness":     float32(dims.Completeness),
			"efficiency":       float32(dims.Efficiency),
		},
	}, nil
}

// isSecurityGapSlug reports whether a taxonomy slug belongs to the security layer
// (api-key-leak / db-credential-leak / pii-injection). These score the user's own
// prompt text rather than a harness-governable code standard, so they are excluded
// from the quality-gap evidence fed to derivation; a harness security rule is
// derived from the `security` VCS review theme instead. The slug is LLM-sourced, so
// match it case/whitespace-insensitively (as securityIncidentCount does) — a raw
// compare would let a variant slip past into HighVsLowGap.
func isSecurityGapSlug(slug string) bool {
	s := strings.ToLower(strings.TrimSpace(slug))
	return s == slugAPIKeyLeak || s == slugPIIInjection || s == slugDBCredentialLeak
}

// gapsWithFDR turns the shared repo-scoped gap buckets into per-issue IssueGap
// evidence and runs the Benjamini-Hochberg FDR pass. A gap needs
// both cohorts populated; with either empty there is no two-sided comparison (and
// the z-test would divide by zero), so it returns nil.
//
// Only issues more common in the LOW cohort (lowPct > highPct) are kept: these
// are the candidate weaknesses to remediate. An inverse issue (more common among
// HIGH scorers) is not an anti-pattern, yet its two-tailed p-value is just as
// small, so leaving it in would let it lead the q-sorted evidence. Filtering
// before the FDR pass also makes the multiple-testing family exactly the
// candidate weaknesses (the two-tailed p stays — it is conservative for this
// one-directional use, which only errs toward flagging less).
func gapsWithFDR(buckets *dao.SessionGapBuckets) []*promptlibraryv1.IssueGap {
	// Nil on a securityOnly run, which skips the gap read entirely; an empty
	// cohort also yields no gap to remediate.
	if buckets == nil || buckets.TotalLow == 0 || buckets.TotalHigh == 0 {
		return nil
	}
	gaps := make([]*promptlibraryv1.IssueGap, 0, len(buckets.Issues))
	for _, issue := range buckets.Issues {
		if isSecurityGapSlug(issue.Slug) {
			// Keep security slugs out of HighVsLowGap (the invariant
			// securityIncidentCount relies on): otherwise a below-threshold org with
			// no real `security` review theme mints api-key-leak / pii rules from
			// these prompt-quality gaps. See isSecurityGapSlug for why they are not a
			// derivable gap; dropping here also keeps them out of the FDR family.
			continue
		}
		highPct := pct(issue.HighCount, buckets.TotalHigh)
		lowPct := pct(issue.LowCount, buckets.TotalLow)
		if lowPct <= highPct {
			continue // inverse/zero gap — not a low-cohort weakness to remediate
		}
		gaps = append(gaps, &promptlibraryv1.IssueGap{
			IssueSlug: issue.Slug,
			HighPct:   float32(highPct),
			LowPct:    float32(lowPct),
			GapPp:     float32(lowPct - highPct),
			PValue:    float32(twoProportionPValue(issue.HighCount, buckets.TotalHigh, issue.LowCount, buckets.TotalLow)),
		})
	}
	applyBenjaminiHochberg(gaps)
	// The DAO's per-issue query has no ORDER BY, so buckets.Issues arrives in
	// nondeterministic order. This slice becomes LLM input (and golden-test
	// fixtures), so sort it deterministically: most-significant gap first
	// (fdr_q asc), then issue_slug to break q-value ties.
	sort.Slice(gaps, func(i, j int) bool {
		if gaps[i].GetFdrQ() != gaps[j].GetFdrQ() {
			return gaps[i].GetFdrQ() < gaps[j].GetFdrQ()
		}
		return gaps[i].GetIssueSlug() < gaps[j].GetIssueSlug()
	})
	return gaps
}

// buildSecuritySummary maps the windowed per-flag-type counts onto the proto's
// fixed incident fields.
func (a *Aggregator) buildSecuritySummary(ctx context.Context, orgID string, from, to time.Time) (*promptlibraryv1.SecuritySummary, error) {
	counts, err := a.securityDAO.CountByFlagType(ctx, orgID, from, to)
	if err != nil {
		return nil, fmt.Errorf("counting security flags: %w", err)
	}
	// SecuritySummary has a fixed field per known security slug. If tetriz-py's
	// taxonomy grows a new security slug, it would otherwise vanish here silently;
	// warn so the gap is observable until a field is added for it.
	for slug := range counts {
		if slug != slugAPIKeyLeak && slug != slugPIIInjection && slug != slugDBCredentialLeak {
			logger.WarnWithCtx(ctx, "prompt-library: unmapped security flag_type dropped from SecuritySummary",
				zap.String(logger.ORG_ID, orgID), zap.String("flag_type", slug))
		}
	}
	return &promptlibraryv1.SecuritySummary{
		ApiKeyLeak:       int32(counts[slugAPIKeyLeak]),
		PiiInjection:     int32(counts[slugPIIInjection]),
		DbCredentialLeak: int32(counts[slugDBCredentialLeak]),
	}, nil
}

// buildProjectMap returns the run's project as a single ProjectSummary mapping it
// to its repo (the "project map"). Session count comes from the existing
// per-project aggregate; the repo-scoped session stats carry the quality
// evidence, so the per-project quality fields are not recomputed here.
func (a *Aggregator) buildProjectMap(ctx context.Context, orgID, project, repoFullName string, from, to time.Time) []*promptlibraryv1.ProjectSummary {
	summary := &promptlibraryv1.ProjectSummary{Project: project, RepoFullName: repoFullName}
	// This aggregator has no IntelligenceConfig and reads only the per-project
	// session count, never context_saturation_count — so the saturation threshold
	// is immaterial here. Pass the service-fallback default (0.85) rather than wire
	// config through a path that ignores the column.
	const contextCriticalPctFallback = 0.85
	projects, err := a.insightDAO.AggregateSessionsByProject(ctx, orgID, from, to, contextCriticalPctFallback)
	if err != nil {
		// A project-map session count is best-effort grounding, not load-bearing
		// evidence — log and emit the map without it rather than fail the run.
		logger.WarnWithCtx(ctx, "prompt-library: per-project session count unavailable",
			zap.String(logger.ORG_ID, orgID), zap.String("project", project), zap.Error(err))
		return []*promptlibraryv1.ProjectSummary{summary}
	}
	for _, projectSummary := range projects {
		if strings.EqualFold(projectSummary.ProjectName, project) {
			summary.Sessions = int32(projectSummary.SessionCount)
			break
		}
	}
	return []*promptlibraryv1.ProjectSummary{summary}
}

// buildTaskTypeBreakdown maps the repo-scoped per-task-type aggregate onto the proto,
// deriving each row's weakest dimension from its six dimension means.
func (a *Aggregator) buildTaskTypeBreakdown(ctx context.Context, orgID string, from, to time.Time, filters ...dao.FilterOption) ([]*promptlibraryv1.TaskTypeSummary, error) {
	rows, err := a.insightDAO.AggregateByTaskType(ctx, orgID, from, to, filters...)
	if err != nil {
		return nil, fmt.Errorf("aggregating sessions by task type: %w", err)
	}
	taskSummaries := make([]*promptlibraryv1.TaskTypeSummary, 0, len(rows))
	for _, row := range rows {
		weakestDim, weakestVal := weakestDimension(row)
		taskSummaries = append(taskSummaries, &promptlibraryv1.TaskTypeSummary{
			TaskType:   row.TaskType,
			Sessions:   int32(row.Sessions),
			Avg:        float32(row.AvgOverall),
			WeakestDim: weakestDim,
			WeakestVal: weakestVal,
		})
	}
	return taskSummaries, nil
}

// buildIDEBreakdown maps the repo-scoped per-IDE aggregate onto the proto.
func (a *Aggregator) buildIDEBreakdown(ctx context.Context, orgID string, from, to time.Time, filters ...dao.FilterOption) ([]*promptlibraryv1.IDESummary, error) {
	rows, err := a.insightDAO.AggregateByIDE(ctx, orgID, from, to, filters...)
	if err != nil {
		return nil, fmt.Errorf("aggregating sessions by ide: %w", err)
	}
	ideSummaries := make([]*promptlibraryv1.IDESummary, 0, len(rows))
	for _, row := range rows {
		ideSummaries = append(ideSummaries, &promptlibraryv1.IDESummary{
			Ide:      row.Ide,
			Sessions: int32(row.Sessions),
			Avg:      float32(row.AvgOverall),
		})
	}
	return ideSummaries, nil
}

// weakestDimension returns the lowest-scoring quality dimension (camelCase key, to
// match SessionSummary.DimensionAverages) and its mean. When the six means carry no
// distinguishable weakness — all equal, including the all-zero "no scored data" case
// — it returns ("", 0) rather than misleadingly flag whichever dimension sorts first.
func weakestDimension(s *model.SessionTaskTypeSummary) (string, float32) {
	dims := []struct {
		name string
		val  float64
	}{
		{"clarity", s.Clarity},
		{"specificity", s.Specificity},
		{"contextProvision", s.ContextProvision},
		{"actionability", s.Actionability},
		{"completeness", s.Completeness},
		{"efficiency", s.Efficiency},
	}
	weakest, minVal, maxVal := dims[0].name, dims[0].val, dims[0].val
	for _, dim := range dims[1:] {
		if dim.val < minVal {
			minVal, weakest = dim.val, dim.name
		}
		if dim.val > maxVal {
			maxVal = dim.val
		}
	}
	if minVal == maxVal {
		return "", 0
	}
	return weakest, float32(minVal)
}

// buildRepoTree compacts the project repo's paths-only tree, read from the
// vcs_repositories.tree_content column (populated by the VCS connector). Skip
// semantics: an absent tree (empty content) skips the repo (returns
// nil, nil — grounding degrades) rather than failing the run. A truncated encoding
// increments the warn metric.
func (a *Aggregator) buildRepoTree(ctx context.Context, orgID, repoFullName, primaryLanguage, treeContent string, params DerivationParams) (*promptlibraryv1.RepoTree, bool, error) {
	if treeContent == "" {
		return nil, false, nil
	}
	tree, reason, err := EncodeRepoTree([]byte(treeContent), repoFullName, primaryLanguage, params.RepoTreeMaxTokens)
	if err != nil {
		return nil, false, fmt.Errorf("encoding repo tree for %s: %w", repoFullName, err)
	}
	if reason != TruncationNone {
		repoTreeTruncatedTotal.WithLabelValues(string(reason)).Inc()
		logger.WarnWithCtx(ctx, "prompt-library: repo tree truncated",
			zap.String(logger.ORG_ID, orgID), zap.String("repo", repoFullName), zap.String("reason", string(reason)))
	}
	return tree, true, nil
}

// buildAdminFeedback loads the dismissed and pinned artifact slugs for the project.
// Slug is the stable identity the LLM is told to reuse, so suppression matches across
// runs even as the descriptive title varies.
//
// Only the dismissed set acts: it is suppressed both mechanically (QualityGate's
// suppressionSlugs) and by an explicit do-not-regenerate instruction in the prompt.
// The pinned set reaches the prompt payload carrying neither, so a pinned slug can be
// re-derived and surface as a second row beside the pinned one.
func (a *Aggregator) buildAdminFeedback(ctx context.Context, orgID, repoID string) (*promptlibraryv1.AdminFeedback, error) {
	dismissed, err := a.libraryDAO.ListDismissedSlugs(ctx, orgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, repoID)
	if err != nil {
		return nil, fmt.Errorf("listing dismissed slugs: %w", err)
	}
	pinned, err := a.libraryDAO.ListPinnedSlugs(ctx, orgID, promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT, repoID)
	if err != nil {
		return nil, fmt.Errorf("listing pinned slugs: %w", err)
	}
	// The payload field is slug-only; the type is carried separately to the gate, which is
	// what enforces the preservation. The prompt only needs the names to avoid re-proposing.
	pinnedSlugs := make([]string, 0, len(pinned))
	for _, ref := range pinned {
		pinnedSlugs = append(pinnedSlugs, ref.Slug)
	}
	return &promptlibraryv1.AdminFeedback{
		DismissedRuleSlugs: dismissed,
		PinnedRuleSlugs:    pinnedSlugs,
	}, nil
}

// classifyReviewBodies classifies the already-read review/comment bodies via
// tetriz-py and buckets the result into per-theme summaries with the top-N
// highest-confidence comments as examples. The "other" fallback
// theme is excluded — it is not an engineering theme. The bodies are read inside
// BuildContext's committed scope; this runs unscoped (it is an HTTP call, so no
// DB tx may be open).
func (a *Aggregator) classifyReviewBodies(ctx context.Context, orgID string, bodies []string, maxThemes int) ([]*promptlibraryv1.ReviewThemeSummary, error) {
	if len(bodies) == 0 {
		return nil, nil
	}
	result, err := a.llmClient.ClassifyReviewThemes(ctx, orgID, bodies, maxThemes)
	if err != nil {
		return nil, fmt.Errorf("classifying review themes: %w", err)
	}
	return bucketThemes(result.Classifications, bodies), nil
}

// ComputePRCorrelation produces the two-sided PRCorrelation evidence: it buckets
// the org's persons by their mean session overallScore, maps those person_ids to
// platform_identity_ids via the identity map (the person-to-platform identity
// resolution is a separate lookup, not a single SQL join), and compares the mean
// PR merge latency of each cohort. delta_pct is how much slower the low-
// session cohort merges relative to the high cohort. Returns a zero correlation
// (no signal) when the platform-identity map DAO is unavailable or a cohort has no merged PRs. This
// evidence is consumed by the engine's per-artifact evidence assembly (slice 7);
// it is not part of AggregatedContext.
//
// Grain: ORG, not project. The cohorts are scored over the whole org and
// their merge hours pool every repo's PRs in the window — there is no repo
// filter, so the same values attach to all of an org's project artifacts (it
// belongs to the org-grain evidence block, alongside the gap/FDR/security stats
// pooled at the org grain, not to project-grain grounding). This matches the
// per-author framing; per-repo cohorts (high/low scorers AND a merged PR in
// one repo) collapse below the two-sided threshold for most projects. A future
// project-scoped variant is an additive change (a repo predicate on the merge-
// hours query) if real data shows per-repo cohorts are dense enough.
// The person-bucket read and the per-cohort merge-hour reads carry no open
// transaction across the platform-identity mapping between them, so no DB tx is
// held across that lookup.
func (a *Aggregator) ComputePRCorrelation(ctx context.Context, orgID string, from, to time.Time) (*promptlibraryv1.PRCorrelation, error) {
	if a.platformIdentityDAO == nil {
		return &promptlibraryv1.PRCorrelation{}, nil
	}
	params := a.params()
	var buckets *dao.PersonScoreBuckets
	if err := scopedRead(ctx, func(sCtx context.Context) error {
		var err error
		buckets, err = a.insightDAO.AggregatePersonScoreBuckets(sCtx, orgID, from, to, params.LowScoreMax, params.HighScoreMin)
		if err != nil {
			return fmt.Errorf("person score buckets: %w", err)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("aggregating person score buckets for org %s: %w", orgID, err)
	}
	// Identity resolution is non-fatal: a failed lookup yields an empty id set, which flows to
	// the empty-cohort guard below as "no signal" rather than a run error.
	highIDs := a.platformIdentitiesForPersons(ctx, orgID, buckets.HighPersonIDs)
	lowIDs := a.platformIdentitiesForPersons(ctx, orgID, buckets.LowPersonIDs)
	var highStats, lowStats *dao.PRMergeStats
	if err := scopedRead(ctx, func(sCtx context.Context) error {
		var err error
		highStats, err = a.pullRequestDAO.AggregateMergeHoursForIdentities(sCtx, orgID, highIDs, from, to)
		if err != nil {
			return fmt.Errorf("aggregating high-cohort merge hours for org %s: %w", orgID, err)
		}
		lowStats, err = a.pullRequestDAO.AggregateMergeHoursForIdentities(sCtx, orgID, lowIDs, from, to)
		if err != nil {
			return fmt.Errorf("aggregating low-cohort merge hours for org %s: %w", orgID, err)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("aggregating cohort merge hours for org %s: %w", orgID, err)
	}
	// Both cohorts need merged PRs for a two-sided signal. With either empty there
	// is no comparison — and an empty cohort's AvgMergeHours is COALESCE'd to 0,
	// which would otherwise read as a real −100% delta. Return no signal instead.
	if highStats.PRCount == 0 || lowStats.PRCount == 0 {
		return &promptlibraryv1.PRCorrelation{}, nil
	}
	correlation := &promptlibraryv1.PRCorrelation{
		HighSessionsAvgMergeHrs: float32(highStats.AvgMergeHours),
		LowSessionsAvgMergeHrs:  float32(lowStats.AvgMergeHours),
	}
	if highStats.AvgMergeHours > 0 {
		correlation.DeltaPct = float32((lowStats.AvgMergeHours - highStats.AvgMergeHours) / highStats.AvgMergeHours * 100)
	}
	return correlation, nil
}

// vcsPlatforms are the VCS platforms a person's PR identities can live on. The
// identity lookup is platform-scoped (platform is a required field), so identities
// are resolved per platform and unioned — the lookup returns nothing for a
// platform a person has no identity on, so iterating all three is correct and
// captures cross-platform contributors.
var vcsPlatforms = []string{"github", "gitlab", "bitbucket"}

// maxPersonsPerIdentityBatch bounds a single platform-identity lookup's person_id
// set, so a large cohort is read in chunks rather than one unbounded IN clause.
const maxPersonsPerIdentityBatch = 500

// platformIdentitiesForPersons resolves a person_id set to its platform_identity_id
// set via the platform-identity map DAO. The lookup is platform-scoped, so this
// resolves per VCS platform in bounded chunks and unions the results. A failed
// chunk is logged and skipped — the mapping being unavailable degrades
// PRCorrelation to no signal via the empty-cohort guard, it does not fail the run.
// Empty input returns no ids.
func (a *Aggregator) platformIdentitiesForPersons(ctx context.Context, orgID string, personIDs []string) []string {
	if len(personIDs) == 0 {
		return nil
	}
	var identityIDs []string
	for _, platform := range vcsPlatforms {
		for start := 0; start < len(personIDs); start += maxPersonsPerIdentityBatch {
			end := start + maxPersonsPerIdentityBatch
			if end > len(personIDs) {
				end = len(personIDs)
			}
			idsByPerson, err := a.platformIdentityDAO.BatchGetIdentityIDsForPersons(ctx, orgID, personIDs[start:end], platform)
			if err != nil {
				logger.WarnWithCtx(ctx, "prompt-library: platform-identity lookup failed; skipping chunk",
					zap.String(logger.ORG_ID, orgID), zap.String("platform", platform), zap.Error(err))
				continue
			}
			for _, identityID := range idsByPerson {
				if identityID != "" {
					identityIDs = append(identityIDs, identityID)
				}
			}
		}
	}
	return identityIDs
}

// bucketThemes groups classifications by theme (excluding "other"), counting
// each theme and selecting the top-N highest-confidence comment bodies as
// examples. Themes are returned in descending count order (then name) for
// stable output.
func bucketThemes(classifications []ThemeClassification, bodies []string) []*promptlibraryv1.ReviewThemeSummary {
	type scoredExample struct {
		confidence float64
		body       string
	}
	examplesByTheme := make(map[string][]scoredExample)
	for _, classification := range classifications {
		if classification.Theme == "" || classification.Theme == otherThemeSlug {
			continue
		}
		if classification.Index < 0 || classification.Index >= len(bodies) {
			continue // defensive: a malformed index can't map to a body
		}
		examplesByTheme[classification.Theme] = append(examplesByTheme[classification.Theme], scoredExample{
			confidence: classification.Confidence,
			body:       bodies[classification.Index],
		})
	}

	summaries := make([]*promptlibraryv1.ReviewThemeSummary, 0, len(examplesByTheme))
	for theme, scored := range examplesByTheme {
		// Body breaks confidence ties so which examples are quoted (and their
		// order) is deterministic — bodies arrive in no guaranteed order.
		sort.Slice(scored, func(i, j int) bool {
			if scored[i].confidence != scored[j].confidence {
				return scored[i].confidence > scored[j].confidence
			}
			return scored[i].body < scored[j].body
		})
		examples := make([]string, 0, themeExamplesTopN)
		for _, example := range scored {
			if len(examples) >= themeExamplesTopN {
				break
			}
			examples = append(examples, example.body)
		}
		summaries = append(summaries, &promptlibraryv1.ReviewThemeSummary{
			Theme:    theme,
			Count:    int32(len(scored)),
			Examples: examples,
		})
	}
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].GetCount() != summaries[j].GetCount() {
			return summaries[i].GetCount() > summaries[j].GetCount()
		}
		return summaries[i].GetTheme() < summaries[j].GetTheme()
	})
	return summaries
}

// applyBenjaminiHochberg fills FdrQ and LowConfidence on the gaps in place
// . It ranks the issues by ascending p-value, computes the BH
// q-value q_i = p_i · m / i, enforces monotonicity from the largest rank down,
// then flags q < fdrQThreshold as primary and a gap clearing raw p < pValuePrimary
// but failing FDR as low_confidence.
func applyBenjaminiHochberg(gaps []*promptlibraryv1.IssueGap) {
	m := len(gaps)
	if m == 0 {
		return
	}
	order := make([]int, m)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		return gaps[order[i]].GetPValue() < gaps[order[j]].GetPValue()
	})

	// q-values in ascending-p order, with the monotonicity sweep applied right→left.
	qByRank := make([]float64, m)
	for rank, idx := range order {
		qByRank[rank] = float64(gaps[idx].GetPValue()) * float64(m) / float64(rank+1)
	}
	for rank := m - 2; rank >= 0; rank-- {
		if qByRank[rank] > qByRank[rank+1] {
			qByRank[rank] = qByRank[rank+1]
		}
	}
	for rank, idx := range order {
		q := math.Min(qByRank[rank], 1.0)
		gaps[idx].FdrQ = float32(q)
		gaps[idx].LowConfidence = q >= fdrQThreshold && float64(gaps[idx].GetPValue()) < pValuePrimary
	}
}

// pct returns count/total as a percentage (0–100); total 0 → 0.
func pct(count, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(count) / float64(total) * 100
}

// CountSecurityIncidents returns the total count of OPEN security flags over
// [from, to] across all flag types — the security signal that lets an org below
// the quality threshold still derive (security-only).
func (a *Aggregator) CountSecurityIncidents(ctx context.Context, orgID string, from, to time.Time) (int, error) {
	counts, err := a.securityDAO.CountByFlagType(ctx, orgID, from, to)
	if err != nil {
		return 0, fmt.Errorf("counting security incidents for org %s: %w", orgID, err)
	}
	total := 0
	for _, count := range counts {
		total += count
	}
	return total, nil
}

// ListInScopeProjects returns the repos the org's derivation fans out over:
// active (non-archived) repos that carry a fetched tree (tree_content set), since
// derivation grounds artifacts against the repo tree and a treeless repo yields
// no groundable output (the tree gate — treeless repos are dropped here, never
// enumerated). The per-repo admission gate then decides per repo whether to derive
// or SKIP using the repo's full_name + connector_id, so the full
// repo objects are returned, not just ids — each repo's id is its scope_value /
// dedup_key.
func (a *Aggregator) ListInScopeProjects(ctx context.Context, orgID string) ([]*model.Repository, error) {
	repos, err := a.gateReader().ListInScopeProjects(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("aggregator in-scope projects: %w", err)
	}
	return repos, nil
}

// CountReviewBodiesForRepo returns the count of in-window review/PR comments for
// one repo — the per-repo gate's review-signal floor input.
func (a *Aggregator) CountReviewBodiesForRepo(ctx context.Context, orgID, repoFullName, connectorID string, from, to time.Time) (int, error) {
	count, err := a.gateReader().CountReviewBodiesForRepo(ctx, orgID, repoFullName, connectorID, from, to)
	if err != nil {
		return 0, fmt.Errorf("aggregator review-body count: %w", err)
	}
	return count, nil
}

// gateReader adapts the aggregator's two gate-relevant DAOs to the GateReader that
// owns the single implementation of the in-scope-repo filter and the review-body
// count. Delegating (rather than keeping a second copy here) is what lets an
// out-of-band operator read report the same numbers the live gate computes.
func (a *Aggregator) gateReader() *GateReader {
	return NewGateReader(a.repoDAO, a.rawRecordDAO)
}

// groundingSignals is the assembled PR/issue + usage-metadata grounding for the hook/agent
// artifact types, returned by readGroundingSignals. Every field is best-effort: a zero value
// means the signal was absent, never an error. Adding a signal is one new field here plus its
// read below; the error sites all return a zero groundingSignals unchanged.
type groundingSignals struct {
	prSignals     []*promptlibraryv1.PRSignal
	bugSignal     *promptlibraryv1.BugSignal
	subagentUsage *promptlibraryv1.SubagentUsageSummary
	languages     []string
	modelsUsed    []string
	mcpServers    []string
	friction      *promptlibraryv1.FrictionSummary
}

// readGroundingSignals reads and assembles the PR/issue + usage-metadata grounding
// for the hook/agent types. It runs inside
// BuildContext's committed read scope. prRepoFilter scopes the pull_requests reads to the physical repo (connector +
// platform_repo_id, rename/connector-safe); repoFilter scopes the session
// reads by full_name (session_analyses has no connector dimension). A missing
// vcs_repo_profile row (never detected) is not an error — merge_enforcement is left
// empty and the rest of the signal still grounds.
func (a *Aggregator) readGroundingSignals(ctx context.Context, orgID, repoID, repoFullName string, prRepoFilter dao.FilterOption, from, to time.Time, repoFilter dao.FilterOption) (groundingSignals, error) {
	var signals groundingSignals
	// Repo-level PR outcome row (module empty): review latency, large-PR rate, review
	// loops, and the repo's merge-enforcement policy — grounds PR-size / commit-convention
	// hooks and the code-reviewer agent.
	pickup, err := a.pullRequestDAO.AggregatePickupTime(ctx, orgID, from, to, prRepoFilter)
	if err != nil {
		return groundingSignals{}, fmt.Errorf("aggregating pickup time for repo %s: %w", repoFullName, err)
	}
	prSize, err := a.pullRequestDAO.AggregatePRSize(ctx, orgID, from, to, prRepoFilter)
	if err != nil {
		return groundingSignals{}, fmt.Errorf("aggregating pr size for repo %s: %w", repoFullName, err)
	}
	// nil aiPRKeys → every merged PR falls in the non-AI partition, so AvgNonAI is the
	// repo's overall mean loops-per-PR (no AI/non-AI split is needed for grounding).
	loops, err := a.pullRequestDAO.AggregateAvgLoopsPerPR(ctx, orgID, from, to, nil, prRepoFilter)
	if err != nil {
		return groundingSignals{}, fmt.Errorf("aggregating avg loops per pr for repo %s: %w", repoFullName, err)
	}
	profile, err := a.repoProfileDAO.GetByRepoID(ctx, orgID, repoID)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return groundingSignals{}, fmt.Errorf("getting vcs repo profile for repo %s org %s: %w", repoID, orgID, err)
	}

	repoRow := &promptlibraryv1.PRSignal{
		Iterations:       loops.AvgNonAI,
		LargePrPct:       pct(prSize.LargePRCount, prSize.PRCount),
		MergeEnforcement: mergeEnforcementLabel(profile),
	}
	if pickup.AvgHrs != nil {
		repoRow.AvgReviewHrs = float32(*pickup.AvgHrs)
	}
	signals.prSignals = append(signals.prSignals, repoRow)

	// Per-module churn rows — grounds the domain-specialist agent toward the modules
	// the repo actually churns.
	churnRows, err := a.pullRequestDAO.AggregatePerModuleChurn(ctx, orgID, repoID, from, to)
	if err != nil {
		return groundingSignals{}, fmt.Errorf("aggregating per-module churn for repo %s: %w", repoID, err)
	}
	for _, row := range churnRows {
		signals.prSignals = append(signals.prSignals, &promptlibraryv1.PRSignal{Module: row.Module, ChurnLines: int32(row.ChurnLines)})
	}

	// Repo-attributed bug rollup — grounds the debugger agent.
	bug, err := a.pullRequestDAO.AggregateBugSignal(ctx, orgID, repoID, from, to)
	if err != nil {
		return groundingSignals{}, fmt.Errorf("aggregating bug signal for repo %s: %w", repoID, err)
	}
	if bug != nil && (bug.BugCount > 0 || bug.HighPriority > 0) {
		signals.bugSignal = &promptlibraryv1.BugSignal{BugCount: int32(bug.BugCount), HighPriority: int32(bug.HighPriority)}
	}

	// Usage-metadata rollups — cheap summaries of the repo's sessions:
	// delegation demand (subagent), the org's stack (languages), model tier
	// (models_used), and MCP tool scope. Reuse the wired AI-Leverage aggregates
	// (repo-scoped via repoFilter); the saturation-pct arg is immaterial here (no
	// saturation column is read), so pass the service-fallback default.
	const contextCriticalPctFallback = 0.85
	langRows, err := a.insightDAO.AggregateLanguageBreakdown(ctx, orgID, from, to, contextCriticalPctFallback, repoFilter)
	if err != nil {
		return groundingSignals{}, fmt.Errorf("aggregating language breakdown for repo %s: %w", repoFullName, err)
	}
	for _, row := range langRows {
		if row.Language != "" {
			signals.languages = append(signals.languages, row.Language)
		}
	}
	modelRows, err := a.insightDAO.AggregateCostByModel(ctx, orgID, from, to, repoFilter)
	if err != nil {
		return groundingSignals{}, fmt.Errorf("aggregating models used for repo %s: %w", repoFullName, err)
	}
	for _, row := range modelRows {
		if row.Model != "" {
			signals.modelsUsed = append(signals.modelsUsed, row.Model)
		}
	}
	mcpRows, err := a.insightDAO.AggregateMCPBreakdown(ctx, orgID, from, to, repoFilter)
	if err != nil {
		return groundingSignals{}, fmt.Errorf("aggregating mcp servers for repo %s: %w", repoFullName, err)
	}
	for _, row := range mcpRows {
		if row.ServerName != "" {
			signals.mcpServers = append(signals.mcpServers, row.ServerName)
		}
	}
	// subagent demand: the number of subagent children actually invoked across the
	// window (subagentUsage.childCount summed over all sessions) is the grassroots
	// agent-demand signal — not the per-session 'delegated' flag, which counts ≤1 per
	// delegated session and misses children spawned from interactive/assisted ones.
	// archetype_counts has no org-scoped source in the current DAO surface, so it is
	// left empty — total_delegations is the load-bearing signal.
	totalDelegations, err := a.insightDAO.AggregateSubagentDelegations(ctx, orgID, from, to, repoFilter)
	if err != nil {
		return groundingSignals{}, fmt.Errorf("aggregating subagent delegations for repo %s: %w", repoFullName, err)
	}
	if totalDelegations > 0 {
		signals.subagentUsage = &promptlibraryv1.SubagentUsageSummary{TotalDelegations: int32(totalDelegations)}
	}

	// Friction demand: the org's recurring session frictions (frictionCounts summed by
	// label) ground a hook that automates a repeated friction away.
	frictionCounts, err := a.insightDAO.AggregateFrictionCounts(ctx, orgID, from, to, repoFilter)
	if err != nil {
		return groundingSignals{}, fmt.Errorf("aggregating friction counts for repo %s: %w", repoFullName, err)
	}
	if len(frictionCounts) > 0 {
		counts := make(map[string]int32, len(frictionCounts))
		for label, n := range frictionCounts {
			counts[label] = int32(n)
		}
		signals.friction = &promptlibraryv1.FrictionSummary{FrictionCounts: counts}
	}

	return signals, nil
}

// mergeEnforcementLabel maps a repo profile's merge-enforcement enum to its string
// label for the PRSignal, or "" when the profile or the enum is unset (a never-detected
// repo inherits no policy).
func mergeEnforcementLabel(profile *model.VCSRepoProfile) string {
	if profile == nil || profile.MergeEnforcement == nil {
		return ""
	}
	return *profile.MergeEnforcement
}

// twoProportionPValue is the two-tailed p-value of a two-proportion z-test for
// "issue present" rates in the high vs low cohorts. With either cohort empty it
// returns 1 (no evidence). The two-tailed p collapses to erfc(|z|/√2).
func twoProportionPValue(highCount, highTotal, lowCount, lowTotal int) float64 {
	if highTotal == 0 || lowTotal == 0 {
		return 1
	}
	p1 := float64(highCount) / float64(highTotal)
	p2 := float64(lowCount) / float64(lowTotal)
	pooled := float64(highCount+lowCount) / float64(highTotal+lowTotal)
	denom := pooled * (1 - pooled) * (1/float64(highTotal) + 1/float64(lowTotal))
	if denom <= 0 {
		return 1 // a 0% or 100% pooled rate carries no discriminating signal
	}
	z := (p1 - p2) / math.Sqrt(denom)
	return math.Erfc(math.Abs(z) / math.Sqrt2)
}
