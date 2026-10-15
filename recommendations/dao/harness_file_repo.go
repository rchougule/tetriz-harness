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

// HarnessFileRepo implements IHarnessFileDAO over the content-addressed harness_files store. Bodies are
// read in place from the content column. Single-tenant.
type HarnessFileRepo struct {
	db *gorm.DB
}

// NewHarnessFileRepo builds the repo over db.
func NewHarnessFileRepo(db *gorm.DB) *HarnessFileRepo {
	return &HarnessFileRepo{db: db}
}

var _ IHarnessFileDAO = (*HarnessFileRepo)(nil)

// ListByIDs returns the harness_files rows for the given ids within the org (including their inline
// content). Unresolvable ids are absent; empty input returns nil, nil.
func (r *HarnessFileRepo) ListByIDs(ctx context.Context, orgID string, ids []string) ([]*model.HarnessFile, error) {
	if orgID == "" {
		return nil, errors.New("listing harness files by ids: orgID cannot be empty")
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var out []*model.HarnessFile
	if err := connFromContext(ctx, r.db).
		Where("organization_id = ? AND id IN ?", orgID, ids).
		Find(&out).Error; err != nil {
		return nil, fmt.Errorf("listing harness files by ids for org %s: %w", orgID, err)
	}
	return out, nil
}
