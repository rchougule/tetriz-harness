// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// reviewBodyExpr extracts a review / PR-comment body from the vendor-faithful payload, across the known
// per-vendor shapes (GitHub/GitLab `body`, Bitbucket `content.raw` / `comment.content.raw`). NULLIF maps
// an empty body to NULL so the IS NOT NULL predicate excludes it.
const reviewBodyExpr = `COALESCE(NULLIF(payload->>'body', ''), NULLIF(payload->'content'->>'raw', ''), NULLIF(payload->'comment'->'content'->>'raw', ''))`

// The connector operations whose raw records carry review / PR-comment bodies.
const (
	opFetchReviews    = "fetch_reviews"
	opFetchPRComments = "fetch_pr_comments"
)

// RawRecordRepo implements IRawRecordDAO over the raw_records landing table — reading review/PR-comment
// body text from the payload JSONB. Single-tenant.
type RawRecordRepo struct {
	db *gorm.DB
}

// NewRawRecordRepo builds the repo over db.
func NewRawRecordRepo(db *gorm.DB) *RawRecordRepo {
	return &RawRecordRepo{db: db}
}

var _ IRawRecordDAO = (*RawRecordRepo)(nil)

// dayAfter returns the midnight at the start of the calendar day AFTER to — the exclusive upper bound
// for a day-grained window over the timestamp data_date column, so the whole final day is included.
func dayAfter(to time.Time) time.Time {
	return time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, to.Location()).AddDate(0, 0, 1)
}

// ListReviewBodies returns up to maxBodies in-window review/PR-comment body texts for the given repos,
// most-recent first (data_date DESC, id DESC) so a trip of the cap keeps the freshest deterministically.
// A non-positive maxBodies is uncapped. Empty repoFullNames returns nil, nil. The window is day-grained:
// [from, start-of-day-after-to).
func (r *RawRecordRepo) ListReviewBodies(ctx context.Context, orgID string, repoFullNames, connectorIDs []string, from, to time.Time, maxBodies int) ([]string, error) {
	if orgID == "" {
		return nil, errors.New("listing review bodies: orgID cannot be empty")
	}
	if len(repoFullNames) == 0 {
		return nil, nil
	}
	query := connFromContext(ctx, r.db).
		Table("raw_records").
		Where("organization_id = ?", orgID).
		Where("operation_id IN ?", []string{opFetchReviews, opFetchPRComments}).
		Where("data_date >= ? AND data_date < ?", from, dayAfter(to)).
		Where("deleted_at IS NULL").
		Where("metadata->>'repo' IN ?", repoFullNames).
		Where(reviewBodyExpr + " IS NOT NULL")
	if len(connectorIDs) > 0 {
		query = query.Where("connector_id IN ?", connectorIDs)
	}
	query = query.Order("data_date DESC, id DESC")
	if maxBodies > 0 {
		query = query.Limit(maxBodies)
	}
	var rows []struct {
		Body string `gorm:"column:body"`
	}
	if err := query.Select(reviewBodyExpr + " AS body").Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("listing review bodies for org %s: %w", orgID, err)
	}
	bodies := make([]string, len(rows))
	for i, row := range rows {
		bodies[i] = row.Body
	}
	return bodies, nil
}

// CountReviewBodies counts the review/PR comments for one repo in the window with a non-empty body — the
// same corpus ListReviewBodies would return, counted without materialising the strings. Same predicates,
// so the count matches the classified corpus exactly.
func (r *RawRecordRepo) CountReviewBodies(ctx context.Context, orgID, repoFullName, connectorID string, from, to time.Time) (int, error) {
	if orgID == "" || repoFullName == "" {
		return 0, errors.New("counting review bodies: orgID and repoFullName cannot be empty")
	}
	query := connFromContext(ctx, r.db).
		Table("raw_records").
		Where("organization_id = ?", orgID).
		Where("operation_id IN ?", []string{opFetchReviews, opFetchPRComments}).
		Where("data_date >= ? AND data_date < ?", from, dayAfter(to)).
		Where("deleted_at IS NULL").
		Where("metadata->>'repo' = ?", repoFullName).
		Where(reviewBodyExpr + " IS NOT NULL")
	if connectorID != "" {
		query = query.Where("connector_id = ?", connectorID)
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		return 0, fmt.Errorf("counting review bodies for org %s repo %s: %w", orgID, repoFullName, err)
	}
	return int(count), nil
}
