// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package dao defines the data-access interfaces the recommendations engine reads and writes
// against, plus the plain result/helper types those methods return. The implementations live in a
// separate layer; these are the contracts the engine depends on.
package dao

import (
	"strings"

	"gorm.io/gorm"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
)

// FilterOption narrows a query. It is a sealed interface — only this package constructs one (via
// NewFuncFilterOption / the With* helpers) — so a caller passes a built option but cannot smuggle an
// arbitrary gorm mutation through the DAO boundary.
type FilterOption interface {
	// ApplyInGorm applies the filter to the query.
	ApplyInGorm(db *gorm.DB) *gorm.DB
	sealed()
}

type funcFilterOption struct {
	fn func(db *gorm.DB) *gorm.DB
}

func (f *funcFilterOption) ApplyInGorm(db *gorm.DB) *gorm.DB { return f.fn(db) }
func (f *funcFilterOption) sealed()                          {}

// NewFuncFilterOption wraps a query-mutating function as a FilterOption.
func NewFuncFilterOption(fn func(db *gorm.DB) *gorm.DB) *funcFilterOption {
	return &funcFilterOption{fn: fn}
}

// ConnectorRepo identifies one physical repo by (connector_id, platform_repo_id). platform_repo_id
// alone is only unique WITHIN a connector, so callers that must not cross-match repos between
// connectors key on the pair, not the bare column.
type ConnectorRepo struct {
	ConnectorID    string
	PlatformRepoID string
}

// WithPullRequestRepoIdentities scopes a pull-request read to the given (connector_id,
// platform_repo_id) pairs. Empty/duplicate pairs are skipped; an all-empty input returns an
// unscoped option.
func WithPullRequestRepoIdentities(pairs []ConnectorRepo) FilterOption {
	return NewFuncFilterOption(func(db *gorm.DB) *gorm.DB {
		seen := make(map[ConnectorRepo]struct{}, len(pairs))
		clauses := make([]string, 0, len(pairs))
		args := make([]any, 0, len(pairs)*2)
		for _, pair := range pairs {
			if pair.ConnectorID == "" || pair.PlatformRepoID == "" {
				continue
			}
			if _, dup := seen[pair]; dup {
				continue
			}
			seen[pair] = struct{}{}
			clauses = append(clauses, "(connector_id = ? AND platform_repo_id = ?)")
			args = append(args, pair.ConnectorID, pair.PlatformRepoID)
		}
		if len(clauses) == 0 {
			return db
		}
		return db.Where("("+strings.Join(clauses, " OR ")+")", args...)
	})
}

// WithVCSRepos scopes a session-signal read to the given repo names, matching on the session's
// reported repo_name, falling back to project_name (basename) when repo_name is empty. Repo names
// are matched case-insensitively and, for a subgroup path, by their last-two-segment reduction. An
// empty input returns nil (no filter).
func WithVCSRepos(repoNames []string) FilterOption {
	if len(repoNames) == 0 {
		return nil
	}
	loweredNames := canonicalRepoKeys(repoNames)
	basenames := extractBasenames(repoNames)
	return NewFuncFilterOption(func(db *gorm.DB) *gorm.DB {
		return db.Where(
			"(LOWER(repo_name) IN ? OR ((repo_name IS NULL OR repo_name = '') AND LOWER(project_name) IN ?))",
			loweredNames, basenames,
		)
	})
}

// extractBasenames returns the lowercased last "/"-segment of each repo name.
func extractBasenames(repoNames []string) []string {
	basenames := make([]string, 0, len(repoNames))
	for _, name := range repoNames {
		parts := strings.Split(name, "/")
		basenames = append(basenames, strings.ToLower(parts[len(parts)-1]))
	}
	return basenames
}

// canonicalRepoKeys returns each repo name lowercased, plus — when the name has more than two
// "/"-segments (a subgroup path group/sub/repo) — its last-two-segment reduction (sub/repo), so a
// full subgroup name matches the sessions it produced. Results are deduped.
func canonicalRepoKeys(repoNames []string) []string {
	seen := make(map[string]struct{}, len(repoNames)*2)
	keys := make([]string, 0, len(repoNames)*2)
	add := func(key string) {
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for _, name := range repoNames {
		lowered := strings.ToLower(strings.Trim(name, "/"))
		add(lowered)
		if parts := strings.Split(lowered, "/"); len(parts) > 2 {
			add(strings.Join(parts[len(parts)-2:], "/"))
		}
	}
	return keys
}

// EvaluatedPartition is one (scope, scope_value, artifact_type) partition a DETECTED findings run
// FULLY evaluated — the unit the auto-close reconcile clears over even when the run produced zero
// findings for it. Every EvaluatedPartition reconciles under recommendation_source = DETECTED,
// disjoint from the org's NULL-source DERIVED rows.
type EvaluatedPartition struct {
	Scope        promptlibraryv1.ArtifactScope
	ScopeValue   string
	ArtifactType promptlibraryv1.ArtifactType
}

// InvokedSkillBehavior is the per-invoked-skill behavioral aggregate the behavioral-attribution
// bridge reads, keyed by the caller on the raw invoked-skill name. Each average is meaningful only
// when its paired coverage count is > 0.
type InvokedSkillBehavior struct {
	Invocations            int
	CompletionRate         float64
	AvgEffectiveContextUse float64
	ContextCoverageN       int
	AvgToolErrors          float64
	ToolErrorCoverageN     int
}

// PRMergeStats is one cohort's mean open→merge latency (hours) and merged-PR count. PRCount == 0
// means no merged PR matched the cohort in the window — "no signal", not a literal 0-hour latency.
type PRMergeStats struct {
	AvgMergeHours float64 `gorm:"column:avg_merge_hours"`
	PRCount       int     `gorm:"column:pr_count"`
}

// PersonScoreBuckets holds the person_id sets bucketed by each person's mean session score in the
// window — low when the mean ≤ lowScoreMax, high when ≥ highScoreMin.
type PersonScoreBuckets struct {
	LowPersonIDs  []string
	HighPersonIDs []string
}

// PersonalInvocationSummary is one engineer's in-window invocation total for one usage identity.
// ArtifactID is the resolved harness_artifacts id, or "" for a row still PENDING resolution (which
// carries only the inline identity IdeType/SourcePath/UserGlobal).
type PersonalInvocationSummary struct {
	PersonID    string           `gorm:"column:person_id"`
	ArtifactID  string           `gorm:"column:artifact_id"`
	IdeType     commonv1.IdeType `gorm:"column:ide_type"`
	SourcePath  string           `gorm:"column:source_path"`
	UserGlobal  bool             `gorm:"column:user_global"`
	Invocations int64            `gorm:"column:invocations"`
}

// SessionIssueGapCount is one issue's low/high session counts within the high-vs-low gap aggregate.
type SessionIssueGapCount struct {
	Slug      string `gorm:"column:slug"`
	LowCount  int    `gorm:"column:low_count"`
	HighCount int    `gorm:"column:high_count"`
}

// SessionGapBuckets is the org-grain high-vs-low gap aggregation: the per-issue low/high session
// counts plus the bucket session totals (the gap-% denominators).
type SessionGapBuckets struct {
	TotalLow  int
	TotalMid  int
	TotalHigh int
	Issues    []SessionIssueGapCount
}

// VCSArtifactContent is one live VCS artifact's name plus its current body's content hash and owning
// repo — the behavioral-attribution collision guard's input. A canonical name resolving to ≥2
// distinct ContentHash is unattributable and must be demoted.
type VCSArtifactContent struct {
	Name         string `gorm:"column:name"`
	RepositoryID string `gorm:"column:repository_id"`
	ContentHash  string `gorm:"column:content_hash"`
}

// LoopsPerPRResult is the AI vs non-AI mean review-loops-per-PR split over the window.
type LoopsPerPRResult struct {
	AvgAI        float64
	AvgNonAI     float64
	AIPRCount    int64
	NonAIPRCount int64
}
