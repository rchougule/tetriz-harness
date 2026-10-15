// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// RepositoryRepo implements IRepositoryDAO over the vcs_repositories registry. Single-tenant: the org is
// bound in every query, and every matching record is read.
type RepositoryRepo struct {
	db *gorm.DB
}

// NewRepositoryRepo builds the repo over db.
func NewRepositoryRepo(db *gorm.DB) *RepositoryRepo {
	return &RepositoryRepo{db: db}
}

var _ IRepositoryDAO = (*RepositoryRepo)(nil)

// GetByID returns one repository scoped to (org, id), or ErrNotFound when no live row matches.
func (r *RepositoryRepo) GetByID(ctx context.Context, orgID, id string) (*model.Repository, error) {
	if orgID == "" || id == "" {
		return nil, errors.New("getting repository: orgID and id cannot be empty")
	}
	var out model.Repository
	if err := connFromContext(ctx, r.db).
		Where("id = ? AND organization_id = ?", id, orgID).
		First(&out).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("getting repository %s for org %s: %w", id, orgID, err)
	}
	return &out, nil
}

// ListByIDs returns the repositories for the given ids, scoped to org — the batch form of GetByID.
// Unresolvable ids are absent from the result; empty input returns nil, nil.
func (r *RepositoryRepo) ListByIDs(ctx context.Context, orgID string, ids []string) ([]*model.Repository, error) {
	if orgID == "" {
		return nil, errors.New("listing repositories by ids: orgID cannot be empty")
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var out []*model.Repository
	if err := connFromContext(ctx, r.db).
		Where("organization_id = ? AND id IN ?", orgID, ids).
		Find(&out).Error; err != nil {
		return nil, fmt.Errorf("listing repositories by ids for org %s: %w", orgID, err)
	}
	return out, nil
}

// ListAllActiveForOrg returns the org's non-archived, non-deleted repositories.
func (r *RepositoryRepo) ListAllActiveForOrg(ctx context.Context, orgID string) ([]*model.Repository, error) {
	if orgID == "" {
		return nil, errors.New("listing active repositories: orgID cannot be empty")
	}
	var out []*model.Repository
	if err := connFromContext(ctx, r.db).
		Where("organization_id = ? AND archived = FALSE AND deleted_at IS NULL", orgID).
		Find(&out).Error; err != nil {
		return nil, fmt.Errorf("listing active repositories for org %s: %w", orgID, err)
	}
	return out, nil
}
