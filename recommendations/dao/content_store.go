// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
)

// ContentStore is the storage-agnostic artifact content store behind the engine's
// artifactStore seam. Bodies and evidence live in prompt_library_content keyed by (organization_id,
// artifact_id); the writer upserts BEFORE the prompt_library row exists (no FK), and the serve path reads
// back by the same key. It satisfies the engine's artifactStore interface structurally. Run input/output
// are not persisted (audit-only, dropped) — those methods are no-ops returning an empty ref.
type ContentStore struct {
	db *gorm.DB
}

// NewContentStore builds the store over db.
func NewContentStore(db *gorm.DB) *ContentStore {
	return &ContentStore{db: db}
}

// PutRunInput is a no-op: run input is audit-only and not persisted. Returns an empty ref.
func (s *ContentStore) PutRunInput(_ context.Context, _, _ string, _ *promptlibraryv1.AggregatedContext) (string, error) {
	return "", nil
}

// PutRunOutput is a no-op: run output is audit-only and not persisted. Returns an empty ref.
func (s *ContentStore) PutRunOutput(_ context.Context, _, _ string, _ []byte) (string, error) {
	return "", nil
}

// PutArtifactContent upserts the artifact body (protojson) into prompt_library_content keyed by (org,
// artifactID) and returns the artifact id as the opaque content ref (the serve path reads back by that
// key). Called before the prompt_library row is inserted.
func (s *ContentStore) PutArtifactContent(ctx context.Context, orgID, artifactID string, content *promptlibraryv1.ArtifactContent) (string, error) {
	blob, err := protojson.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("marshaling artifact content for %s: %w", artifactID, err)
	}
	if err := s.upsert(ctx, orgID, artifactID, map[string]any{"content": blob}); err != nil {
		return "", fmt.Errorf("storing artifact content for %s: %w", artifactID, err)
	}
	return artifactID, nil
}

// PutArtifactEvidence upserts the artifact evidence blob (protojson) into prompt_library_content keyed by
// (org, artifactID) and returns the artifact id as the opaque ref.
func (s *ContentStore) PutArtifactEvidence(ctx context.Context, orgID, artifactID string, evidence *promptlibraryv1.ArtifactEvidence) (string, error) {
	blob, err := protojson.Marshal(evidence)
	if err != nil {
		return "", fmt.Errorf("marshaling artifact evidence for %s: %w", artifactID, err)
	}
	if err := s.upsert(ctx, orgID, artifactID, map[string]any{"evidence": blob}); err != nil {
		return "", fmt.Errorf("storing artifact evidence for %s: %w", artifactID, err)
	}
	return artifactID, nil
}

// upsert writes the given columns into the (org, artifactID) row, inserting it if absent and updating the
// named columns (plus updated_at) if present — so content and evidence can be written independently.
func (s *ContentStore) upsert(ctx context.Context, orgID, artifactID string, columns map[string]any) error {
	if orgID == "" || artifactID == "" {
		return errors.New("content store: orgID and artifactID cannot be empty")
	}
	row := map[string]any{"organization_id": orgID, "artifact_id": artifactID}
	assignCols := make([]string, 0, len(columns)+1)
	for name, value := range columns {
		row[name] = value
		assignCols = append(assignCols, name)
	}
	assignCols = append(assignCols, "updated_at")
	row["updated_at"] = gorm.Expr("now()")
	return connFromContext(ctx, s.db).
		Table("prompt_library_content").
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "organization_id"}, {Name: "artifact_id"}},
			DoUpdates: clause.AssignmentColumns(assignCols),
		}).
		Create(row).Error
}

// GetArtifactContent reads the stored body for (org, artifactID), or nil when none is stored (the serve
// path treats a nil body as "no content").
func (s *ContentStore) GetArtifactContent(ctx context.Context, orgID, artifactID string) ([]byte, error) {
	return s.readColumn(ctx, orgID, artifactID, "content")
}

// GetArtifactEvidence reads the stored evidence blob for (org, artifactID), or nil when none is stored.
func (s *ContentStore) GetArtifactEvidence(ctx context.Context, orgID, artifactID string) ([]byte, error) {
	return s.readColumn(ctx, orgID, artifactID, "evidence")
}

func (s *ContentStore) readColumn(ctx context.Context, orgID, artifactID, column string) ([]byte, error) {
	if orgID == "" || artifactID == "" {
		return nil, errors.New("content store: orgID and artifactID cannot be empty")
	}
	var blob []byte
	err := connFromContext(ctx, s.db).
		Table("prompt_library_content").
		Where("organization_id = ? AND artifact_id = ?", orgID, artifactID).
		Pluck(column, &blob).Error
	if err != nil {
		return nil, fmt.Errorf("reading %s for %s: %w", column, artifactID, err)
	}
	return blob, nil
}
