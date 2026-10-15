// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

//go:generate go tool mockgen -source=dao.go -destination=mocks/mock_dao.go -package=mocks

import (
	"context"
	"time"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// ISessionInsightDAO reads the per-session judged-quality aggregates (the session-signal table).
// These are the fourteen aggregate readers the DERIVED aggregator and the DETECTED behavioral bridge
// consume; each is scoped to an org and a window, optionally narrowed to a repo set via FilterOption.
type ISessionInsightDAO interface {
	// AggregateByOrg returns the org-grain session rollup over the window.
	AggregateByOrg(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) (*model.SessionOrgSummary, error)
	// AggregateDimensionsByOrg returns the org-grain mean per quality dimension.
	AggregateDimensionsByOrg(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) (*model.SessionDimensionScores, error)
	// AggregateSessionsByProject returns the per-project rollup; saturationThresholdPct sets the
	// context-saturation cutoff.
	AggregateSessionsByProject(ctx context.Context, orgID string, from, to time.Time, saturationThresholdPct float64, filters ...FilterOption) ([]*model.SessionProjectSummary, error)
	// AggregateByTaskType returns the per-task-type rollup.
	AggregateByTaskType(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) ([]*model.SessionTaskTypeSummary, error)
	// AggregateByIDE returns the per-IDE rollup.
	AggregateByIDE(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) ([]*model.SessionIDESummary, error)
	// AggregateHighVsLowGap returns the per-issue low/high session counts and bucket totals; a session
	// is low when its mean score ≤ lowScoreMax, high when ≥ highScoreMin.
	AggregateHighVsLowGap(ctx context.Context, orgID string, from, to time.Time, lowScoreMax, highScoreMin int, filters ...FilterOption) (*SessionGapBuckets, error)
	// AggregatePersonScoreBuckets returns the person_id sets bucketed by each person's mean score.
	AggregatePersonScoreBuckets(ctx context.Context, orgID string, from, to time.Time, lowScoreMax, highScoreMin int, filters ...FilterOption) (*PersonScoreBuckets, error)
	// AggregateCostByModel returns the per-model cost rollup.
	AggregateCostByModel(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) ([]*model.AILeverageCostByModel, error)
	// AggregateMCPBreakdown returns the per-MCP-server user count.
	AggregateMCPBreakdown(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) ([]*model.AILeverageMCPBreakdown, error)
	// AggregateSubagentDelegations returns the total subagent delegations over the window.
	AggregateSubagentDelegations(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) (int, error)
	// AggregateLanguageBreakdown returns the per-language rollup; criticalPct is the saturation cutoff.
	AggregateLanguageBreakdown(ctx context.Context, orgID string, from, to time.Time, criticalPct float64, filters ...FilterOption) ([]*model.AILeverageLanguageBreakdown, error)
	// AggregateFrictionCounts returns the friction-type → count map.
	AggregateFrictionCounts(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) (map[string]int, error)
	// AggregateInvokedSkillBehavior returns the per-invoked-skill behavioral aggregate, keyed by the
	// raw invoked-skill name.
	AggregateInvokedSkillBehavior(ctx context.Context, orgID string, from, to time.Time) (map[string]InvokedSkillBehavior, error)
	// DistinctMcpServerNames returns the distinct MCP server names seen in the window.
	DistinctMcpServerNames(ctx context.Context, orgID string, from, to time.Time) ([]string, error)
}

// IPromptLibraryDAO reads and writes the prompt_library rows — the engine's output and the
// suppression/lifecycle reads that gate it.
type IPromptLibraryDAO interface {
	// PersistArtifacts writes a DERIVED run's artifacts and recomputes is_latest for the run's
	// (org, scope, scope_value) partition, atomically with the caller's terminal status write.
	PersistArtifacts(ctx context.Context, orgID, taskExecutionID string, items []*model.PromptLibraryItem) error
	// PersistFindings writes a DETECTED run's findings and runs the evaluated-partition auto-close
	// reconcile, atomically with the caller's terminal status write.
	PersistFindings(ctx context.Context, orgID, taskExecutionID string, items []*model.PromptLibraryItem, evaluatedPartitions []EvaluatedPartition) error
	// ListLiveDetectedPartitions returns the partitions currently holding a live DETECTED finding for
	// an (org, scope, scope_value).
	ListLiveDetectedPartitions(ctx context.Context, orgID string, scope promptlibraryv1.ArtifactScope, scopeValue string) ([]EvaluatedPartition, error)
	// ListDismissedSlugs returns the admin-dismissed slugs for an (org, scope, scope_value).
	ListDismissedSlugs(ctx context.Context, orgID string, scope promptlibraryv1.ArtifactScope, scopeValue string) ([]string, error)
	// ListPinnedSlugs returns the admin-pinned (type, slug) refs for an (org, scope, scope_value).
	ListPinnedSlugs(ctx context.Context, orgID string, scope promptlibraryv1.ArtifactScope, scopeValue string) ([]model.ArtifactSlugRef, error)
	// ListRecentHiddenFailedSlugs returns the (type, slug) refs hidden as failed-eval over the last
	// nRuns runs, optionally scoped to a recommendation source.
	ListRecentHiddenFailedSlugs(ctx context.Context, orgID string, scope promptlibraryv1.ArtifactScope, scopeValue string, recommendationSource *promptlibraryv1.ArtifactRecommendationSource, nRuns int) ([]model.ArtifactSlugRef, error)
	// ListVisibleByOrg returns the org's surfaceable rows; includeDetected relaxes the dark-gate on
	// DETECTED findings for an org whose findings serving is enabled.
	ListVisibleByOrg(ctx context.Context, orgID, scopeValue string, includeDetected bool) ([]*model.PromptLibraryItem, error)
}

// ITaskExecutionDAO owns the run lifecycle: single-in-flight create, terminal compare-and-set, the
// stale-run reaper, and payload updates.
type ITaskExecutionDAO interface {
	// CreateIfNoneRunning inserts a RUNNING row only if no RUNNING row shares its (org, task_type,
	// dedup_key); returns false when a concurrent run already holds the slot.
	CreateIfNoneRunning(ctx context.Context, exec *model.TaskExecution) (bool, error)
	// GetByID loads one run by id within the org.
	GetByID(ctx context.Context, orgID, id string) (*model.TaskExecution, error)
	// Update writes the masked columns of a run.
	Update(ctx context.Context, exec *model.TaskExecution, masks []taskexecutionsv1.TaskExecutionFieldMask) error
	// MarkTerminalIfRunning atomically stamps the terminal status (and substatus/completed_at/error)
	// only while the row is still RUNNING; returns false if the row was already terminal (e.g. reaped).
	MarkTerminalIfRunning(ctx context.Context, orgID, id string, status taskexecutionsv1.TaskExecutionStatus, substatus taskexecutionsv1.TaskExecutionSubStatus, completedAt time.Time, errorMessage string) (bool, error)
	// ReapStaleRunning fails RUNNING rows of a task type older than staleAfter; returns the count reaped.
	ReapStaleRunning(ctx context.Context, orgID string, taskType taskexecutionsv1.TaskType, staleAfter time.Duration) (int64, error)
}

// IHarnessArtifactDAO reads the committed-harness inventory the DETECTED detectors evaluate.
type IHarnessArtifactDAO interface {
	// ListVCSByRepo returns the live org-canonical (VCS-source) artifacts for one repo.
	ListVCSByRepo(ctx context.Context, orgID, repoID string) ([]*model.HarnessArtifact, error)
	// ListRepoIDsWithLiveVCSArtifacts returns the repo ids that have ≥1 live VCS artifact.
	ListRepoIDsWithLiveVCSArtifacts(ctx context.Context, orgID string) ([]string, error)
	// ListVCSArtifactContentByType returns the live VCS artifacts of the given types with their
	// content-hash + owning repo — the behavioral-attribution collision guard's input.
	ListVCSArtifactContentByType(ctx context.Context, orgID string, types []commonv1.HarnessArtifactType) ([]VCSArtifactContent, error)
	// ListPersonalByPersons returns the per-person (DESKTOP) artifacts for the personal lane.
	ListPersonalByPersons(ctx context.Context, orgID string, personIDs []string) ([]*model.HarnessArtifact, error)
}

// IHarnessFileDAO reads content-addressed artifact bodies by id.
type IHarnessFileDAO interface {
	// ListByIDs returns the harness_files rows for the given ids within the org.
	ListByIDs(ctx context.Context, orgID string, ids []string) ([]*model.HarnessFile, error)
}

// IHarnessUsageRecordsDAO reads per-engineer usage for the usage-informed detectors.
type IHarnessUsageRecordsDAO interface {
	// ListPrimaryRepoPersons returns the persons whose primary repo is repoID (so each person's
	// personal harness is evaluated in exactly one repo run per window).
	ListPrimaryRepoPersons(ctx context.Context, orgID, repoID string, windowStart, windowEnd time.Time) ([]string, error)
	// ListInvocationsForPersons returns each person's in-window invocation totals per usage identity.
	ListInvocationsForPersons(ctx context.Context, orgID string, personIDs []string, windowStart, windowEnd time.Time) ([]PersonalInvocationSummary, error)
}

// IPullRequestDAO reads the PR-outcome aggregates the DERIVED flow grounds signals on.
type IPullRequestDAO interface {
	// AggregateMergeHoursForIdentities returns the mean open→merge latency + merged-PR count for a
	// cohort of platform identities.
	AggregateMergeHoursForIdentities(ctx context.Context, orgID string, platformIdentityIDs []string, from, to time.Time) (*PRMergeStats, error)
	// AggregatePickupTime returns the open→first-review pickup-wait distribution.
	AggregatePickupTime(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) (*model.PickupTimeResult, error)
	// AggregatePRSize returns the PR-size distribution.
	AggregatePRSize(ctx context.Context, orgID string, from, to time.Time, filters ...FilterOption) (*model.PRSizeResult, error)
	// AggregateAvgLoopsPerPR returns the AI vs non-AI mean review-loops-per-PR split; aiPRKeys names
	// the AI-attributed PR keys.
	AggregateAvgLoopsPerPR(ctx context.Context, orgID string, from, to time.Time, aiPRKeys []string, filters ...FilterOption) (LoopsPerPRResult, error)
	// AggregatePerModuleChurn returns per-top-level-module summed changed lines for one repo.
	AggregatePerModuleChurn(ctx context.Context, orgID, repositoryID string, from, to time.Time) ([]model.PerModuleChurnRow, error)
	// AggregateBugSignal returns the repo-attributed bug rollup for one repo.
	AggregateBugSignal(ctx context.Context, orgID, repositoryID string, from, to time.Time) (*model.BugSignalResult, error)
}

// IRawRecordDAO reads review/PR-comment body text from the landing records.
type IRawRecordDAO interface {
	// CountReviewBodies returns the count of in-window review/PR-comment bodies for one repo.
	CountReviewBodies(ctx context.Context, orgID, repoFullName, connectorID string, from, to time.Time) (int, error)
	// ListReviewBodies returns up to maxBodies in-window review/PR-comment body texts, most-recent
	// first, across the given repos.
	ListReviewBodies(ctx context.Context, orgID string, repoFullNames, connectorIDs []string, from, to time.Time, maxBodies int) ([]string, error)
}

// ISecurityFlagDAO reads the open security-flag counts.
type ISecurityFlagDAO interface {
	// CountByFlagType returns the open-flag count per flag_type over the window.
	CountByFlagType(ctx context.Context, orgID string, from, to time.Time) (map[string]int, error)
}

// IRepositoryDAO reads the repository registry.
type IRepositoryDAO interface {
	// GetByID loads one repository by id within the org.
	GetByID(ctx context.Context, orgID, id string) (*model.Repository, error)
	// ListByIDs loads the repositories for the given ids within the org (an unresolvable id is omitted).
	ListByIDs(ctx context.Context, orgID string, ids []string) ([]*model.Repository, error)
	// ListAllActiveForOrg returns the org's non-archived, non-deleted repositories.
	ListAllActiveForOrg(ctx context.Context, orgID string) ([]*model.Repository, error)
}

// IVCSRepoProfileDAO reads per-repo policy context (merge enforcement).
type IVCSRepoProfileDAO interface {
	// GetByRepoID returns the repo's profile; a missing row is non-fatal to the caller.
	GetByRepoID(ctx context.Context, orgID, repoID string) (*model.VCSRepoProfile, error)
}

// IPlatformIdentityMapDAO resolves a person to their per-platform identity. It replaces a
// cross-service identity lookup a host application would otherwise make over gRPC.
type IPlatformIdentityMapDAO interface {
	// BatchGetIdentityIDsForPersons returns person_id → platform_identity_id for the given persons on
	// one platform within the org. A person with no mapping is omitted from the result.
	BatchGetIdentityIDsForPersons(ctx context.Context, orgID string, personIDs []string, platform string) (map[string]string, error)
}

// Transactor runs a function inside a single database transaction. The terminal-write path uses it to
// keep the compare-and-set status write and the artifact/finding persist atomic in ONE transaction,
// so a persist failure rolls back the status claim and no half-written is_latest partition can commit.
// The DAO methods invoked inside fn participate in the transaction carried on the passed context.
type Transactor interface {
	// InTx runs fn inside one transaction, committing on nil and rolling back on error.
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}
