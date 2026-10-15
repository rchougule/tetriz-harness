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

// VCSRepoProfileRepo implements IVCSRepoProfileDAO over the vcs_repo_profile policy map. Single-tenant.
type VCSRepoProfileRepo struct {
	db *gorm.DB
}

// NewVCSRepoProfileRepo builds the repo over db.
func NewVCSRepoProfileRepo(db *gorm.DB) *VCSRepoProfileRepo {
	return &VCSRepoProfileRepo{db: db}
}

var _ IVCSRepoProfileDAO = (*VCSRepoProfileRepo)(nil)

// GetByRepoID returns the repo's profile, or ErrNotFound when none exists (non-fatal to the caller).
func (r *VCSRepoProfileRepo) GetByRepoID(ctx context.Context, orgID, repoID string) (*model.VCSRepoProfile, error) {
	if orgID == "" || repoID == "" {
		return nil, errors.New("getting repo profile: orgID and repoID cannot be empty")
	}
	var row model.VCSRepoProfile
	if err := connFromContext(ctx, r.db).
		Where("repo_id = ? AND organization_id = ?", repoID, orgID).
		First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("getting repo profile for org %s repo %s: %w", orgID, repoID, err)
	}
	return &row, nil
}
