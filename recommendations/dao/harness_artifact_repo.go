// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// Artifact-source and scope discriminator values, bound as the full proto value NAME the text columns
// store (the generated enums carry no driver.Valuer).
var (
	vcsArtifactSource     = commonv1.HarnessArtifactSource_HARNESS_ARTIFACT_SOURCE_VCS.String()
	desktopArtifactSource = commonv1.HarnessArtifactSource_HARNESS_ARTIFACT_SOURCE_DESKTOP.String()
	// personalArtifactScopes are the two scopes an engineer places themselves, with no org-canonical VCS
	// master: USER_GLOBAL (machine-wide, repository_id NULL) and REPO_LOCAL (uncommitted). REPO_COMMITTED
	// desktop rows are working copies of the committed set and belong to the VCS-baseline path, not here.
	personalArtifactScopes = []string{
		commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_USER_GLOBAL.String(),
		commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_REPO_LOCAL.String(),
	}
)

// HarnessArtifactRepo implements IHarnessArtifactDAO over the observed-artifact inventory, scoped to the
// org-canonical VCS rows for the detectors and the per-person DESKTOP rows for the personal lane.
// Single-tenant; every read binds artifact_source so a VCS read never surfaces desktop rows.
type HarnessArtifactRepo struct {
	db *gorm.DB
}

// NewHarnessArtifactRepo builds the repo over db.
func NewHarnessArtifactRepo(db *gorm.DB) *HarnessArtifactRepo {
	return &HarnessArtifactRepo{db: db}
}

var _ IHarnessArtifactDAO = (*HarnessArtifactRepo)(nil)

// ListVCSByRepo returns the live org-canonical (VCS-source, person-less) artifacts for one repo, ordered
// by source_path.
func (r *HarnessArtifactRepo) ListVCSByRepo(ctx context.Context, orgID, repoID string) ([]*model.HarnessArtifact, error) {
	if orgID == "" || repoID == "" {
		return nil, errors.New("listing VCS harness artifacts: orgID and repoID cannot be empty")
	}
	var out []*model.HarnessArtifact
	if err := connFromContext(ctx, r.db).
		Where("organization_id = ? AND artifact_source = ? AND person_id IS NULL AND repository_id = ?",
			orgID, vcsArtifactSource, repoID).
		Order("source_path ASC").
		Find(&out).Error; err != nil {
		return nil, fmt.Errorf("listing VCS harness artifacts for org %s repo %s: %w", orgID, repoID, err)
	}
	return out, nil
}

// ListRepoIDsWithLiveVCSArtifacts returns the distinct repository ids holding ≥1 live person-less VCS
// artifact — the harness-improve scan eligibility set. The UNSPECIFIED-typed settings convergence
// sentinel is excluded (it is a re-fetch marker, not a real committed artifact).
func (r *HarnessArtifactRepo) ListRepoIDsWithLiveVCSArtifacts(ctx context.Context, orgID string) ([]string, error) {
	if orgID == "" {
		return nil, errors.New("listing repo ids with live VCS artifacts: orgID cannot be empty")
	}
	var out []string
	if err := connFromContext(ctx, r.db).
		Model(&model.HarnessArtifact{}).
		Where("organization_id = ? AND artifact_source = ? AND person_id IS NULL AND type <> ?",
			orgID, vcsArtifactSource, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_UNSPECIFIED.String()).
		Distinct().
		Pluck("repository_id", &out).Error; err != nil {
		return nil, fmt.Errorf("listing repo ids with live VCS harness artifacts for org %s: %w", orgID, err)
	}
	return out, nil
}

// ListVCSArtifactContentByType returns the live VCS artifacts of the given types with their current
// body's content hash and owning repo — the behavioral-attribution collision guard's input (a canonical
// name resolving to ≥2 distinct content hashes is unattributable). Empty types returns nil, nil.
func (r *HarnessArtifactRepo) ListVCSArtifactContentByType(ctx context.Context, orgID string, types []commonv1.HarnessArtifactType) ([]VCSArtifactContent, error) {
	if orgID == "" {
		return nil, errors.New("listing VCS artifact content by type: orgID cannot be empty")
	}
	if len(types) == 0 {
		return nil, nil
	}
	typeNames := make([]string, len(types))
	for i, artifactType := range types {
		typeNames[i] = artifactType.String()
	}
	var out []VCSArtifactContent
	if err := connFromContext(ctx, r.db).
		Table("harness_artifacts AS ha").
		Joins("JOIN harness_files AS hf ON hf.id = ha.current_file_id").
		Where("ha.organization_id = ? AND ha.artifact_source = ? AND ha.person_id IS NULL AND ha.deleted_at IS NULL AND ha.type IN ?",
			orgID, vcsArtifactSource, typeNames).
		Select("ha.name AS name, ha.repository_id AS repository_id, hf.content_hash AS content_hash").
		Order("ha.repository_id ASC, ha.name ASC").
		Scan(&out).Error; err != nil {
		return nil, fmt.Errorf("listing VCS artifact content by type for org %s: %w", orgID, err)
	}
	return out, nil
}

// ListPersonalByPersons returns the live per-person (DESKTOP) artifacts in the personal scopes for the
// given persons — the personal lane's input. Empty personIDs returns nil, nil.
func (r *HarnessArtifactRepo) ListPersonalByPersons(ctx context.Context, orgID string, personIDs []string) ([]*model.HarnessArtifact, error) {
	if orgID == "" {
		return nil, errors.New("listing personal harness artifacts: orgID cannot be empty")
	}
	if len(personIDs) == 0 {
		return nil, nil
	}
	var out []*model.HarnessArtifact
	if err := connFromContext(ctx, r.db).
		Where("organization_id = ? AND artifact_source = ? AND person_id IN ? AND scope IN ?",
			orgID, desktopArtifactSource, personIDs, personalArtifactScopes).
		Order("person_id ASC, scope ASC, source_path ASC, id ASC").
		Find(&out).Error; err != nil {
		return nil, fmt.Errorf("listing personal harness artifacts for org %s: %w", orgID, err)
	}
	return out, nil
}
