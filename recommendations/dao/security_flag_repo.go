// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// SecurityFlagDAO reads the open security-flag counts off the flat security_flags table.
// Single-tenant: the org is bound in the query text, and every matching record is counted.
type SecurityFlagDAO struct {
	db *gorm.DB
}

// NewSecurityFlagDAO builds the reader over db.
func NewSecurityFlagDAO(db *gorm.DB) *SecurityFlagDAO {
	return &SecurityFlagDAO{db: db}
}

var _ ISecurityFlagDAO = (*SecurityFlagDAO)(nil)

// securityFlagTypeCount scans one GROUP BY row of the open-flag count.
type securityFlagTypeCount struct {
	FlagType string `gorm:"column:flag_type"`
	Count    int    `gorm:"column:count"`
}

// CountByFlagType returns the open-flag count per flag_type over the window, keyed by the kebab-case
// category slug (e.g. "api-key-leak"). Only status='open' is counted: a resolved flag is a handled
// incident and a false_positive was never a real one, so counting either would re-surface cleared
// incidents as detector evidence. Soft-deleted rows are excluded. The window bounds flag_date (a
// calendar DATE), inclusive on both ends.
func (r *SecurityFlagDAO) CountByFlagType(ctx context.Context, orgID string, from, to time.Time) (map[string]int, error) {
	if orgID == "" {
		return nil, fmt.Errorf("counting security flags by type: orgID cannot be empty")
	}
	var rows []securityFlagTypeCount
	err := r.db.WithContext(ctx).
		Table("security_flags").
		Where("organization_id = ? AND status = 'open' AND flag_date >= ? AND flag_date <= ? AND deleted_at IS NULL",
			orgID, from, to).
		Select("flag_type, COUNT(*) AS count").
		Group("flag_type").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("counting security flags by type for org %s: %w", orgID, err)
	}
	counts := make(map[string]int, len(rows))
	for _, row := range rows {
		counts[row.FlagType] = row.Count
	}
	return counts, nil
}
