// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// PromptLibraryRepo implements IPromptLibraryDAO over PostgreSQL via GORM. Single-tenant: the org is
// bound in every query, and every matching record is read. Enum columns are compared by their full proto value NAME
// string — the serializers write the name on insert, so every WHERE binds `.String()`, never the raw
// enum value (which would bind the ordinal and match nothing).
type PromptLibraryRepo struct {
	db *gorm.DB
}

// NewPromptLibraryRepo builds the repo over db.
func NewPromptLibraryRepo(db *gorm.DB) *PromptLibraryRepo {
	return &PromptLibraryRepo{db: db}
}

var _ IPromptLibraryDAO = (*PromptLibraryRepo)(nil)

// PersistArtifacts atomically persists a DERIVED run's graded artifacts and reconciles which
// generation is "latest" for each partition in the batch. In one transaction it hard-deletes any rows
// already written for this task_execution_id (delete-then-reinsert idempotency on retry), inserts the
// batch, then per distinct (org, scope, scope_value, artifact_type, recommendation_source) recomputes
// is_latest so it marks exactly the run that owns the most-recent VISIBLE artifact for that partition —
// this partition only, never a sibling's rows, another type's rows, or the other provenance's rows.
//
// Bucketing by artifact_type is load-bearing: drop-not-persist makes a run partial, so reconciling
// project-wide would clear is_latest on a type the run never emitted and retract its prior visible
// generation. Callers run this inside the terminal-write transaction (see Transactor), so a persist
// failure rolls back the terminal status claim.
func (r *PromptLibraryRepo) PersistArtifacts(ctx context.Context, orgID, taskExecutionID string, items []*model.PromptLibraryItem) error {
	if err := validateBatch(orgID, taskExecutionID, items); err != nil {
		return fmt.Errorf("persist artifacts: %w", err)
	}
	// Every DERIVED partition is fully evaluated (a derive run persists every type it emits), so each
	// reconciles via promote — the reconcile set is exactly the batch's item partitions.
	if err := r.persistAndReconcile(ctx, orgID, taskExecutionID, items, distinctScopes(items)); err != nil {
		return fmt.Errorf("persist artifacts: %w", err)
	}
	return nil
}

// PersistFindings persists a DETECTED scan's findings and reconciles is_latest over the EVALUATED
// partitions only — never the item partitions. A finding kept in a partially-evaluated partition (in
// items but deliberately absent from evaluatedPartitions, e.g. a degraded lens) is still inserted, but
// its is_latest is left un-reconciled so the prior generation stays latest and this run's row stays
// is_latest=false — never retracting a standing finding the run did not fully re-evaluate. Each
// evaluated partition then promotes if it has a this-run item, or auto-closes (clear without
// re-promote) if it produced none (the N→0 case). Every finding is DETECTED-source, so its reconcile
// partition is disjoint from the org's NULL-source DERIVED rows.
func (r *PromptLibraryRepo) PersistFindings(ctx context.Context, orgID, taskExecutionID string, items []*model.PromptLibraryItem, evaluatedPartitions []EvaluatedPartition) error {
	if orgID == "" {
		return errors.New("persist findings: orgID cannot be empty")
	}
	if taskExecutionID == "" {
		return errors.New("persist findings: taskExecutionID cannot be empty")
	}
	// A run with no items AND no evaluated partitions has nothing to do — an unambiguous caller bug (a
	// scan always evaluates ≥1 partition or emits ≥1 finding), so fail loud rather than silently.
	if len(items) == 0 && len(evaluatedPartitions) == 0 {
		return errors.New("persist findings: items and evaluatedPartitions cannot both be empty")
	}
	if err := validateItems(orgID, taskExecutionID, items); err != nil {
		return fmt.Errorf("persist findings: %w", err)
	}
	reconcilePartitions := make([]scopeKey, 0, len(evaluatedPartitions))
	for _, partition := range evaluatedPartitions {
		reconcilePartitions = append(reconcilePartitions, scopeKey{
			artifactScope:           partition.Scope,
			scopeValue:              partition.ScopeValue,
			artifactType:            partition.ArtifactType,
			recommendationSource:    promptlibraryv1.ArtifactRecommendationSource_ARTIFACT_RECOMMENDATION_SOURCE_DETECTED,
			hasRecommendationSource: true,
		})
	}
	if err := r.persistAndReconcile(ctx, orgID, taskExecutionID, items, reconcilePartitions); err != nil {
		return fmt.Errorf("persist findings: %w", err)
	}
	return nil
}

// persistAndReconcile is the shared atomic persist body: hard-delete this run's prior rows (retry
// idempotency), insert the batch (skipped when empty — the reconcile-only findings path), then
// reconcile is_latest over exactly the reconcilePartitions the caller names. Per partition it PROMOTES
// via reconcileLatest when this run wrote ≥1 row for it, or AUTO-CLOSES via clearLatestForPartition
// (clear without re-promote) when the run produced no row for it. An item whose partition is NOT in
// reconcilePartitions is inserted but never reconciled, so it stays is_latest=false beside the untouched
// prior generation. It joins the caller's transaction when one is on the context (the terminal-write
// path), else opens its own.
func (r *PromptLibraryRepo) persistAndReconcile(ctx context.Context, orgID, taskExecutionID string, items []*model.PromptLibraryItem, reconcilePartitions []scopeKey) error {
	fillArtifactDefaults(items)
	conn := connFromContext(ctx, r.db)
	if err := conn.Transaction(func(tx *gorm.DB) error {
		// Hard-delete (Unscoped) any rows already written for this run so a retry deterministically
		// replaces them rather than leaving a stale/soft-deleted mix. Safe for an empty batch.
		if err := tx.Unscoped().
			Where("organization_id = ? AND task_execution_id = ?", orgID, taskExecutionID).
			Delete(&model.PromptLibraryItem{}).Error; err != nil {
			return fmt.Errorf("deleting prior rows for run %s: %w", taskExecutionID, err)
		}
		if len(items) > 0 {
			if err := tx.Create(items).Error; err != nil {
				return fmt.Errorf("inserting %d artifacts: %w", len(items), err)
			}
		}
		// Which partitions this run actually wrote a row for — a reconcile partition with a matching
		// item PROMOTES, one without AUTO-CLOSES. The reconcile sees both the inserted batch and the
		// prior generations.
		itemPartitions := make(map[scopeKey]struct{})
		for _, scope := range distinctScopes(items) {
			itemPartitions[scope] = struct{}{}
		}
		reconciled := make(map[scopeKey]struct{})
		for _, scope := range reconcilePartitions {
			if _, done := reconciled[scope]; done {
				continue
			}
			reconciled[scope] = struct{}{}
			if _, hasItem := itemPartitions[scope]; hasItem {
				if err := reconcileLatest(tx, orgID, scope); err != nil {
					return fmt.Errorf("reconciling latest for org %s scope_value %s: %w", orgID, scope.scopeValue, err)
				}
				continue
			}
			if err := clearLatestForPartition(tx, orgID, scope); err != nil {
				return fmt.Errorf("auto-close reconcile for org %s scope_value %s: %w", orgID, scope.scopeValue, err)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("persist-and-reconcile transaction for run %s: %w", taskExecutionID, err)
	}
	return nil
}

// reconcileLatest points is_latest at the run owning the most-recent VISIBLE artifact for one partition
// (org, scope, scope_value, artifact_type, recommendation_source): every currently-latest row of that
// partition is cleared, then that run's rows of that partition are set latest. The recommendation_source
// half of the key isolates a DETECTED finding's reconcile from the org's NULL-source DERIVED rows of the
// same type; bucketing by artifact_type keeps a partial run from retracting a type it did not emit. A
// partition whose live rows are all hidden ends with no latest row — correct, nothing of it to surface.
//
// Correctness precondition: runs of one (org, partition) never persist concurrently (the single-in-flight
// run guarantee), so their created_at values are strictly ordered and "most recent" is unambiguous. The
// task_execution_id tie-break keeps it deterministic on a created_at tie. Must run inside the persist
// transaction.
func reconcileLatest(tx *gorm.DB, orgID string, scope scopeKey) error {
	scoped := func() *gorm.DB { return partitionScoped(tx, orgID, scope) }
	if err := scoped().Where("is_latest = ?", true).Update("is_latest", false).Error; err != nil {
		return fmt.Errorf("clearing latest for org %s scope_value %s: %w", orgID, scope.scopeValue, err)
	}
	visible := promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE.String()
	var latestVisibleRun []string
	if err := scoped().
		Where("surface_state = ?", visible).
		Order("created_at DESC, task_execution_id DESC").
		Limit(1).
		Pluck("task_execution_id", &latestVisibleRun).Error; err != nil {
		return fmt.Errorf("finding latest visible run for org %s scope_value %s: %w", orgID, scope.scopeValue, err)
	}
	if len(latestVisibleRun) == 0 {
		return nil
	}
	if err := scoped().Where("task_execution_id = ?", latestVisibleRun[0]).Update("is_latest", true).Error; err != nil {
		return fmt.Errorf("promoting latest visible run for org %s scope_value %s: %w", orgID, scope.scopeValue, err)
	}
	return nil
}

// clearLatestForPartition clears is_latest across one partition WITHOUT re-promoting a prior visible
// generation — the auto-close reconcile for a partition the run fully evaluated but produced NO row for
// (the N→0 case: a prior DETECTED finding whose condition no longer detects). A partition that DID
// produce a row this run is reconciled by reconcileLatest instead, so this only ever runs over an empty
// partition. Must run inside the persist transaction.
func clearLatestForPartition(tx *gorm.DB, orgID string, scope scopeKey) error {
	if err := partitionScoped(tx, orgID, scope).Where("is_latest = ?", true).Update("is_latest", false).Error; err != nil {
		return fmt.Errorf("auto-closing latest for org %s scope_value %s: %w", orgID, scope.scopeValue, err)
	}
	return nil
}

// partitionScoped narrows a query to the one reconcile partition. recommendation_source is matched with
// NULL-safe equality (IS NOT DISTINCT FROM) bound via recommendationSourceBind: nil isolates the
// NULL-source bucket (every DERIVED row), while the DETECTED bucket isolates findings so a DETECTED
// reconcile never clears a live DERIVED row's is_latest, and vice versa. scope and artifact_type bind
// their full value NAME (`.String()`) to match the text columns. The single definition of the partition
// predicate so reconcileLatest and clearLatestForPartition cannot drift.
func partitionScoped(tx *gorm.DB, orgID string, scope scopeKey) *gorm.DB {
	return tx.Model(&model.PromptLibraryItem{}).
		Where("organization_id = ? AND scope = ? AND scope_value = ? AND artifact_type = ? AND recommendation_source IS NOT DISTINCT FROM ?",
			orgID, scope.artifactScope.String(), scope.scopeValue, scope.artifactType.String(), recommendationSourceBind(scope))
}

// ListLiveDetectedPartitions returns the DISTINCT (scope, scope_value, artifact_type) partitions of the
// org's current live DETECTED findings for one scope/scopeValue — a row is live when is_latest=true AND
// surface_state=VISIBLE AND recommendation_source=DETECTED (soft-deleted rows are excluded by GORM's
// default scope). It is the vanished-type auto-close input. Excludes NULL-source DERIVED rows (the
// null-safe match), and superseded (is_latest=false) rows. scopeValue filters to one project when
// non-empty.
func (r *PromptLibraryRepo) ListLiveDetectedPartitions(ctx context.Context, orgID string, scope promptlibraryv1.ArtifactScope, scopeValue string) ([]EvaluatedPartition, error) {
	if orgID == "" {
		return nil, errors.New("listing live detected partitions: orgID cannot be empty")
	}
	detected := promptlibraryv1.ArtifactRecommendationSource_ARTIFACT_RECOMMENDATION_SOURCE_DETECTED.String()
	visible := promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE.String()
	query := connFromContext(ctx, r.db).Model(&model.PromptLibraryItem{}).
		Where("organization_id = ?", orgID).
		Where("scope = ?", scope.String()).
		Where("is_latest = ?", true).
		Where("surface_state = ?", visible).
		Where("recommendation_source IS NOT DISTINCT FROM ?", detected)
	if scopeValue != "" {
		query = query.Where("scope_value = ?", scopeValue)
	}
	// Scan into string columns and convert: EvaluatedPartition is a plain engine type with no proto
	// serializer, and the enum columns store the value NAME.
	var rows []struct {
		Scope        string `gorm:"column:scope"`
		ScopeValue   string `gorm:"column:scope_value"`
		ArtifactType string `gorm:"column:artifact_type"`
	}
	if err := query.
		Distinct("scope", "scope_value", "artifact_type").
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("listing live detected partitions for org %s scope_value %s: %w", orgID, scopeValue, err)
	}
	partitions := make([]EvaluatedPartition, 0, len(rows))
	for _, row := range rows {
		scopeEnum, ok := promptlibraryv1.ArtifactScope_value[row.Scope]
		if !ok {
			return nil, fmt.Errorf("listing live detected partitions: unknown scope %q", row.Scope)
		}
		typeEnum, ok := promptlibraryv1.ArtifactType_value[row.ArtifactType]
		if !ok {
			return nil, fmt.Errorf("listing live detected partitions: unknown artifact_type %q", row.ArtifactType)
		}
		partitions = append(partitions, EvaluatedPartition{
			Scope:        promptlibraryv1.ArtifactScope(scopeEnum),
			ScopeValue:   row.ScopeValue,
			ArtifactType: promptlibraryv1.ArtifactType(typeEnum),
		})
	}
	return partitions, nil
}

// ListRecentHiddenFailedSlugs returns the distinct (artifact_type, slug) pairs of HIDDEN_FAILED_EVAL
// artifacts from the last nRuns runs for (org, scope, scope_value), for the pre-LLM dedup gate. The
// window is by DISTINCT task_execution_id (most-recent run first), NOT a row LIMIT — one run emits many
// hidden rows, so a row cap would silently shorten the window. nRuns ≤ 0 returns nil. Empty slugs are
// excluded. recommendationSource partitions the dedup lane: nil scopes to NULL-source (DERIVED), a
// non-nil value to that source (DETECTED), so the two lanes never suppress or age each other out. The
// SAME source filter binds both the run-window and the hidden-slug query.
func (r *PromptLibraryRepo) ListRecentHiddenFailedSlugs(ctx context.Context, orgID string, scope promptlibraryv1.ArtifactScope, scopeValue string, recommendationSource *promptlibraryv1.ArtifactRecommendationSource, nRuns int) ([]model.ArtifactSlugRef, error) {
	if nRuns <= 0 {
		return nil, nil
	}
	db := connFromContext(ctx, r.db)
	hidden := promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_HIDDEN_FAILED_EVAL.String()
	sourceScoped := func(q *gorm.DB) *gorm.DB {
		if recommendationSource == nil {
			return q.Where("recommendation_source IS NULL")
		}
		return q.Where("recommendation_source = ?", recommendationSource.String())
	}
	// Inner query: the last nRuns distinct run ids for this (org, scope, scope_value, source) across ALL
	// surface states, most-recent first — so clean runs push an old failure out of the window.
	recentRunIDs := sourceScoped(db.Model(&model.PromptLibraryItem{}).
		Select("task_execution_id").
		Where("organization_id = ? AND scope = ? AND scope_value = ?", orgID, scope.String(), scopeValue)).
		Group("task_execution_id").
		Order("MAX(created_at) DESC, task_execution_id DESC").
		Limit(nRuns)
	var rows []model.ArtifactSlugRef
	if err := sourceScoped(db.Model(&model.PromptLibraryItem{}).
		Where("organization_id = ? AND scope = ? AND scope_value = ? AND surface_state = ? AND slug <> ''", orgID, scope.String(), scopeValue, hidden)).
		Where("task_execution_id IN (?)", recentRunIDs).
		Group("artifact_type, slug").
		Select("artifact_type", "slug").
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("listing recent hidden-failed slugs for org %s scope_value %s: %w", orgID, scopeValue, err)
	}
	return rows, nil
}

// ListDismissedSlugs returns the distinct slugs of all admin-dismissed artifacts for (org, scope,
// scope_value), all-history (an admin dismissal is honored until un-dismissed).
func (r *PromptLibraryRepo) ListDismissedSlugs(ctx context.Context, orgID string, scope promptlibraryv1.ArtifactScope, scopeValue string) ([]string, error) {
	slugs, err := r.listSlugsByClientStatus(ctx, orgID, scope, scopeValue, promptlibraryv1.ArtifactClientStatus_ARTIFACT_CLIENT_STATUS_DISMISSED)
	if err != nil {
		return nil, fmt.Errorf("listing dismissed slugs: %w", err)
	}
	return slugs, nil
}

// ListPinnedSlugs returns the distinct (artifact_type, slug) pairs of admin-pinned artifacts for (org,
// scope, scope_value), so a re-derivation preserves the pinned copy instead of emitting a second row
// beside it. Typed, not slug-only: a pin preserves one specific artifact, so a pinned rule must not
// suppress a same-slug hook.
func (r *PromptLibraryRepo) ListPinnedSlugs(ctx context.Context, orgID string, scope promptlibraryv1.ArtifactScope, scopeValue string) ([]model.ArtifactSlugRef, error) {
	pinned := promptlibraryv1.ArtifactClientStatus_ARTIFACT_CLIENT_STATUS_PINNED.String()
	var rows []model.ArtifactSlugRef
	if err := connFromContext(ctx, r.db).Model(&model.PromptLibraryItem{}).
		Where("organization_id = ? AND scope = ? AND scope_value = ? AND client_status = ? AND slug <> ''",
			orgID, scope.String(), scopeValue, pinned).
		Group("artifact_type, slug").
		Select("artifact_type", "slug").
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("listing pinned slugs for org %s scope_value %s: %w", orgID, scopeValue, err)
	}
	return rows, nil
}

// listSlugsByClientStatus returns the distinct non-empty slugs for (org, scope, scope_value) carrying
// the given client_status.
func (r *PromptLibraryRepo) listSlugsByClientStatus(ctx context.Context, orgID string, scope promptlibraryv1.ArtifactScope, scopeValue string, status promptlibraryv1.ArtifactClientStatus) ([]string, error) {
	var slugs []string
	if err := connFromContext(ctx, r.db).Model(&model.PromptLibraryItem{}).
		Where("organization_id = ? AND scope = ? AND scope_value = ? AND client_status = ? AND slug <> ''", orgID, scope.String(), scopeValue, status.String()).
		Group("slug").
		Pluck("slug", &slugs).Error; err != nil {
		return nil, fmt.Errorf("querying %s slugs for org %s scope_value %s: %w", status, orgID, scopeValue, err)
	}
	return slugs, nil
}

// ListVisibleByOrg returns the org's list-contract artifacts (the dashboard set): surface_state=VISIBLE
// AND ((is_latest AND client_status != DISMISSED) OR client_status = PINNED), restricted to the
// member-visible scopes. scopeValue filters to one project when non-empty. Ordered priority DESC,
// confidence_score DESC, id — deterministic for client-side paging.
//
// The dark-gate: DETECTED-source findings are excluded from the served list unless includeDetected (the
// org has findings serving enabled). The null-safe DETECTED exclusion (IS DISTINCT FROM) keeps a
// NULL-source DERIVED row in the list; a plain `<>` would drop it under three-valued logic.
func (r *PromptLibraryRepo) ListVisibleByOrg(ctx context.Context, orgID, scopeValue string, includeDetected bool) ([]*model.PromptLibraryItem, error) {
	if orgID == "" {
		return nil, errors.New("listing visible artifacts: orgID cannot be empty")
	}
	visible := promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE.String()
	dismissed := promptlibraryv1.ArtifactClientStatus_ARTIFACT_CLIENT_STATUS_DISMISSED.String()
	pinned := promptlibraryv1.ArtifactClientStatus_ARTIFACT_CLIENT_STATUS_PINNED.String()
	query := connFromContext(ctx, r.db).Model(&model.PromptLibraryItem{}).
		Where("organization_id = ?", orgID).
		Where("surface_state = ?", visible).
		Where("(is_latest = ? AND client_status <> ?) OR client_status = ?", true, dismissed, pinned).
		// Member-visible scopes only — a PERSON row (one engineer's own finding) is excluded regardless
		// of includeDetected (the dark-gate governs org-canonical DETECTED rows only).
		Where("scope IN ?", memberVisibleScopeNames())
	if !includeDetected {
		detected := promptlibraryv1.ArtifactRecommendationSource_ARTIFACT_RECOMMENDATION_SOURCE_DETECTED.String()
		query = query.Where("recommendation_source IS DISTINCT FROM ?", detected)
	}
	if scopeValue != "" {
		query = query.Where("scope_value = ?", scopeValue)
	}
	var items []*model.PromptLibraryItem
	if err := query.Order("priority DESC, confidence_score DESC, id").Find(&items).Error; err != nil {
		return nil, fmt.Errorf("listing visible artifacts for org %s: %w", orgID, err)
	}
	return items, nil
}

// memberVisibleScopeNames is model.MemberVisibleScopes as the value-NAME strings the text column stores.
func memberVisibleScopeNames() []string {
	names := make([]string, 0, len(model.MemberVisibleScopes))
	for _, scope := range model.MemberVisibleScopes {
		names = append(names, scope.String())
	}
	return names
}

// scopeKey is the (scope, scope_value, artifact_type, recommendation_source) tuple the is_latest
// reconcile is bucketed by. artifact_type is part of the key because drop-not-persist makes a run
// PARTIAL, so the reconcile must recompute is_latest ONLY for the types the run actually persisted.
// recommendationSource isolates a DETECTED finding from the org's NULL-source DERIVED rows of the same
// type; it is a VALUE + presence bool (not a pointer) so scopeKey stays comparable as a map key.
// hasRecommendationSource = false is the NULL-source bucket (every DERIVED row).
//
// The marketplace discriminator (is_marketplace / global_artifact_id) is out of scope and dropped
// here: marketplace is not included, so the key is the five-tuple, not six.
type scopeKey struct {
	artifactScope           promptlibraryv1.ArtifactScope
	scopeValue              string
	artifactType            promptlibraryv1.ArtifactType
	recommendationSource    promptlibraryv1.ArtifactRecommendationSource
	hasRecommendationSource bool
}

// recommendationSourceBind resolves a scopeKey's provenance to the bind value for the reconcile's
// null-safe partition predicate (recommendation_source IS NOT DISTINCT FROM ?): nil for the NULL-source
// bucket (DERIVED), else the full generated enum NAME string (…DETECTED). The bind is a string, never
// the raw enum value, because the enums carry no driver.Valuer — a raw enum would bind its
// ordinal and match nothing.
func recommendationSourceBind(scope scopeKey) any {
	if !scope.hasRecommendationSource {
		return nil
	}
	return scope.recommendationSource.String()
}

// distinctScopes returns the unique (scope, scope_value, artifact_type, recommendation_source) tuples
// present in the batch, preserving first-seen order — the buckets whose is_latest the reconcile must
// recompute. A type dropped by drop-not-persist is absent from the batch, hence absent here, so its
// prior generation's is_latest is left untouched.
func distinctScopes(items []*model.PromptLibraryItem) []scopeKey {
	seen := make(map[scopeKey]struct{}, len(items))
	keys := make([]scopeKey, 0, len(items))
	for _, item := range items {
		key := scopeKey{
			artifactScope: item.Scope,
			scopeValue:    item.ScopeValue,
			artifactType:  item.ArtifactType,
		}
		if item.RecommendationSource != nil {
			key.recommendationSource = *item.RecommendationSource
			key.hasRecommendationSource = true
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

// validateBatch guards PersistArtifacts' preconditions: a non-empty org/run id and a NON-EMPTY item
// batch every one of which is stamped with the same task_execution_id. PersistFindings does NOT reuse
// this — its reconcile-only path allows empty items — so the empty check lives here.
func validateBatch(orgID, taskExecutionID string, items []*model.PromptLibraryItem) error {
	if orgID == "" {
		return errors.New("orgID cannot be empty")
	}
	if taskExecutionID == "" {
		return errors.New("taskExecutionID cannot be empty")
	}
	if len(items) == 0 {
		return errors.New("items cannot be empty")
	}
	if err := validateItems(orgID, taskExecutionID, items); err != nil {
		return fmt.Errorf("validating batch items: %w", err)
	}
	return nil
}

// validateItems guards the per-item invariant shared by both batch writers: every item is stamped with
// the batch's task_execution_id and organization_id, so the delete-then-reinsert idempotency key is
// unambiguous and the reconcile's orgID filter agrees with each inserted row.
func validateItems(orgID, taskExecutionID string, items []*model.PromptLibraryItem) error {
	for _, item := range items {
		if item.TaskExecutionID != taskExecutionID {
			return fmt.Errorf("item task_execution_id %q does not match batch %q", item.TaskExecutionID, taskExecutionID)
		}
		if item.OrganizationID != orgID {
			return fmt.Errorf("item organization_id %q does not match batch %q", item.OrganizationID, orgID)
		}
	}
	return nil
}

// fillArtifactDefaults normalizes items for insert: it forces is_latest=false (making the reconcile the
// SOLE authority on is_latest — the column DEFAULTs true and the model carries no gorm default tag, so
// an un-reconciled inserted row would otherwise stand as a second latest) and sets a non-nil empty
// proto message on the NOT NULL JSONB columns any item left nil. Mutates in place.
func fillArtifactDefaults(items []*model.PromptLibraryItem) {
	for _, item := range items {
		item.IsLatest = false
		if item.ArtifactContent == nil {
			item.ArtifactContent = &promptlibraryv1.ContentRef{}
		}
		if item.Evidence == nil {
			item.Evidence = &promptlibraryv1.EvidenceProjection{}
		}
		if item.AdminInteraction == nil {
			item.AdminInteraction = &promptlibraryv1.AdminInteraction{}
		}
	}
}
