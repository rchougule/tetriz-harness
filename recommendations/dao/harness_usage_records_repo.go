// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
)

// HarnessUsageRecordsRepo implements IHarnessUsageRecordsDAO over the per-invocation usage records that
// feed the usage-informed detectors (personal-never-used, MCP). Single-tenant and org-wide: every
// matching record in the whole-org harness inventory is read, like the other reads.
type HarnessUsageRecordsRepo struct {
	db *gorm.DB
}

// NewHarnessUsageRecordsRepo builds the repo over db.
func NewHarnessUsageRecordsRepo(db *gorm.DB) *HarnessUsageRecordsRepo {
	return &HarnessUsageRecordsRepo{db: db}
}

var _ IHarnessUsageRecordsDAO = (*HarnessUsageRecordsRepo)(nil)

// ListPrimaryRepoPersons returns the persons whose primary repo is repoID over the window, so each
// person's personal harness is evaluated in exactly one repo run. The candidate (person, repo) set is the
// UNION of in-window resolved usage (weighted by distinct conversations) and the person's synced repos
// (weight 0), and DISTINCT ON picks one row per person in a deterministic total order (sessions DESC,
// then repository_id ASC) so the argmax is stable across the repo runs that race for the same person.
func (r *HarnessUsageRecordsRepo) ListPrimaryRepoPersons(ctx context.Context, orgID, repoID string, windowStart, windowEnd time.Time) ([]string, error) {
	if orgID == "" {
		return nil, errors.New("listing primary-repo persons: orgID cannot be empty")
	}
	if repoID == "" {
		return nil, errors.New("listing primary-repo persons: repoID cannot be empty")
	}
	var personIDs []string
	err := connFromContext(ctx, r.db).Raw(`
		WITH candidates AS (
			SELECT person_id, repository_id, COUNT(DISTINCT conversation_id) AS sessions
			FROM harness_usage_records
			WHERE organization_id = ? AND repository_id IS NOT NULL
			  AND usage_date >= ? AND usage_date <= ?
			GROUP BY person_id, repository_id
			UNION ALL
			SELECT person_id, repository_id, 0 AS sessions
			FROM harness_sync_scopes
			WHERE organization_id = ? AND repository_id IS NOT NULL
		), ranked AS (
			SELECT DISTINCT ON (person_id) person_id, repository_id
			FROM (
				SELECT person_id, repository_id, SUM(sessions) AS sessions
				FROM candidates
				GROUP BY person_id, repository_id
			) c
			ORDER BY person_id, sessions DESC, repository_id ASC
		)
		SELECT person_id FROM ranked WHERE repository_id = ? ORDER BY person_id
	`, orgID, windowStart, windowEnd, orgID, repoID).Scan(&personIDs).Error
	if err != nil {
		return nil, fmt.Errorf("listing primary-repo persons for org %s repo %s: %w", orgID, repoID, err)
	}
	return personIDs, nil
}

// ListInvocationsForPersons returns each person's in-window invocation totals per usage identity. A
// PENDING (unresolved) row groups under the inline identity (ide_type / source_path / user_global) with
// an empty artifact_id. Empty personIDs returns nil, nil. ide_type is read as its stored value NAME and
// converted to the proto enum; an unknown name maps to UNSPECIFIED.
func (r *HarnessUsageRecordsRepo) ListInvocationsForPersons(ctx context.Context, orgID string, personIDs []string, windowStart, windowEnd time.Time) ([]PersonalInvocationSummary, error) {
	if orgID == "" {
		return nil, errors.New("listing invocations for persons: orgID cannot be empty")
	}
	if len(personIDs) == 0 {
		return nil, nil
	}
	var rows []struct {
		PersonID    string `gorm:"column:person_id"`
		ArtifactID  string `gorm:"column:artifact_id"`
		IdeType     string `gorm:"column:ide_type"`
		SourcePath  string `gorm:"column:source_path"`
		UserGlobal  bool   `gorm:"column:user_global"`
		Invocations int64  `gorm:"column:invocations"`
	}
	// GROUP BY the raw artifact_id column (NULL for a PENDING row); the SELECT COALESCEs it to '' so the
	// zero-value groups under the inline identity. The builder form expands person_id IN ? to a stable
	// placeholder list.
	if err := connFromContext(ctx, r.db).
		Table("harness_usage_records").
		Select("person_id, COALESCE(artifact_id::text, '') AS artifact_id, ide_type, source_path, user_global, SUM(invocation_count) AS invocations").
		Where("organization_id = ? AND person_id IN ?", orgID, personIDs).
		Where("usage_date >= ? AND usage_date <= ?", windowStart, windowEnd).
		Group("person_id, artifact_id, ide_type, source_path, user_global").
		Order("person_id, artifact_id, source_path").
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("listing invocations for persons in org %s: %w", orgID, err)
	}
	out := make([]PersonalInvocationSummary, 0, len(rows))
	for _, row := range rows {
		ideType := commonv1.IdeType_IDE_TYPE_UNSPECIFIED
		if value, ok := commonv1.IdeType_value[row.IdeType]; ok {
			ideType = commonv1.IdeType(value)
		}
		out = append(out, PersonalInvocationSummary{
			PersonID:    row.PersonID,
			ArtifactID:  row.ArtifactID,
			IdeType:     ideType,
			SourcePath:  row.SourcePath,
			UserGlobal:  row.UserGlobal,
			Invocations: row.Invocations,
		})
	}
	return out, nil
}
