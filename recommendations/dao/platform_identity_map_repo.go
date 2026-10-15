// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// PlatformIdentityMapRepo implements IPlatformIdentityMapDAO over the platform_identity_map table — the
// single-tenant replacement for a cross-service identity lookup a host application would otherwise make
// over gRPC.
type PlatformIdentityMapRepo struct {
	db *gorm.DB
}

// NewPlatformIdentityMapRepo builds the repo over db.
func NewPlatformIdentityMapRepo(db *gorm.DB) *PlatformIdentityMapRepo {
	return &PlatformIdentityMapRepo{db: db}
}

var _ IPlatformIdentityMapDAO = (*PlatformIdentityMapRepo)(nil)

// BatchGetIdentityIDsForPersons returns person_id → platform_identity_id for the given persons on one
// platform within the org. A person with no mapping is omitted. Empty personIDs returns an empty map.
func (r *PlatformIdentityMapRepo) BatchGetIdentityIDsForPersons(ctx context.Context, orgID string, personIDs []string, platform string) (map[string]string, error) {
	if orgID == "" {
		return nil, errors.New("batch-getting platform identities: orgID cannot be empty")
	}
	if platform == "" {
		return nil, errors.New("batch-getting platform identities: platform cannot be empty")
	}
	result := make(map[string]string, len(personIDs))
	if len(personIDs) == 0 {
		return result, nil
	}
	var rows []struct {
		PersonID           string `gorm:"column:person_id"`
		PlatformIdentityID string `gorm:"column:platform_identity_id"`
	}
	if err := connFromContext(ctx, r.db).
		Table("platform_identity_map").
		Where("organization_id = ? AND platform = ? AND person_id IN ?", orgID, platform, personIDs).
		Select("person_id, platform_identity_id").
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("batch-getting platform identities for org %s platform %s: %w", orgID, platform, err)
	}
	for _, row := range rows {
		result[row.PersonID] = row.PlatformIdentityID
	}
	return result, nil
}
