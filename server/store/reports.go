package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
)

// Installed is one managed file an agent found on disk.
type Installed struct {
	IdeType     v1.IdeType
	Repository  string
	Checkout    string
	SourcePath  string
	ContentHash string
}

// ApplyFailure is one convergence failure an agent reported.
type ApplyFailure struct {
	ArtifactID  string
	Repository  string
	ContentHash string
	Reason      string
}

// Report is a person's complete reported state.
type Report struct {
	PersonID          string
	DeviceID          string
	AgentVersion      string
	KnownRepositories []string
	Installed         []Installed
	Failures          []ApplyFailure
	ReportedAt        time.Time
}

// ReplaceReport makes r the person's only report.
func (s *Store) ReplaceReport(ctx context.Context, r Report) error {
	known := r.KnownRepositories
	if known == nil {
		known = []string{}
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO agent_reports (person_id, device_id, agent_version, known_repositories, reported_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (person_id) DO UPDATE SET device_id = EXCLUDED.device_id, agent_version = EXCLUDED.agent_version,
				known_repositories = EXCLUDED.known_repositories, reported_at = EXCLUDED.reported_at`,
			r.PersonID, r.DeviceID, r.AgentVersion, known, r.ReportedAt); err != nil {
			return fmt.Errorf("writing report: %w", mapWriteErr(err))
		}
		if _, err := tx.Exec(ctx, `DELETE FROM report_installed WHERE person_id = $1`, r.PersonID); err != nil {
			return fmt.Errorf("clearing installed: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM report_apply_failures WHERE person_id = $1`, r.PersonID); err != nil {
			return fmt.Errorf("clearing failures: %w", err)
		}
		if len(r.Installed) > 0 {
			ides := make([]string, 0, len(r.Installed))
			repos := make([]string, 0, len(r.Installed))
			checkouts := make([]string, 0, len(r.Installed))
			paths := make([]string, 0, len(r.Installed))
			hashes := make([]string, 0, len(r.Installed))
			for _, in := range r.Installed {
				ides = append(ides, in.IdeType.String())
				repos = append(repos, in.Repository)
				checkouts = append(checkouts, in.Checkout)
				paths = append(paths, in.SourcePath)
				hashes = append(hashes, in.ContentHash)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO report_installed (person_id, ide_type, repository, checkout, source_path, content_hash)
				SELECT $1, * FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[])`,
				r.PersonID, ides, repos, checkouts, paths, hashes); err != nil {
				return fmt.Errorf("writing installed: %w", err)
			}
		}
		for _, f := range r.Failures {
			if _, err := tx.Exec(ctx, `
				INSERT INTO report_apply_failures (person_id, artifact_id, repository, content_hash, reason)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (person_id, artifact_id, repository) DO UPDATE SET content_hash = EXCLUDED.content_hash, reason = EXCLUDED.reason`,
				r.PersonID, f.ArtifactID, f.Repository, f.ContentHash, f.Reason); err != nil {
				return fmt.Errorf("writing failure: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("replacing report: %w", err)
	}
	return nil
}

// ReportSummary is the part of a person's report eligibility reads.
type ReportSummary struct {
	ReportedAt        time.Time
	KnownRepositories map[string]bool
}

// ReportSummaries returns the report summary of each person who has reported.
func (s *Store) ReportSummaries(ctx context.Context, personIDs []string) (map[string]*ReportSummary, error) {
	out := make(map[string]*ReportSummary, len(personIDs))
	if len(personIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT person_id, reported_at, known_repositories FROM agent_reports WHERE person_id = ANY($1::text[])`, personIDs)
	if err != nil {
		return nil, fmt.Errorf("loading reports: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id    string
			sum   ReportSummary
			known []string
		)
		if err := rows.Scan(&id, &sum.ReportedAt, &known); err != nil {
			return nil, fmt.Errorf("scanning report: %w", err)
		}
		sum.KnownRepositories = make(map[string]bool, len(known))
		for _, k := range known {
			sum.KnownRepositories[k] = true
		}
		out[id] = &sum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("loading reports: %w", err)
	}
	return out, nil
}

// Observation is the file a report carries at one identity.
type Observation struct {
	ContentHash string
	ReportedAt  time.Time
}

// ObservationKey names one file a person's report may carry. PreferHash is
// the body the caller is grading against: with several checkouts reporting
// the path, the row carrying it wins, so any matching checkout grounds
// INSTALLED.
type ObservationKey struct {
	PersonID   string
	IdeType    v1.IdeType
	Repository string
	SourcePath string
	PreferHash string
}

// Observations returns the reported file at each key in one query; keys with
// nothing reported there are absent.
func (s *Store) Observations(ctx context.Context, keys []ObservationKey) (map[ObservationKey]*Observation, error) {
	out := make(map[ObservationKey]*Observation, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	persons := make([]string, 0, len(keys))
	ides := make([]string, 0, len(keys))
	repos := make([]string, 0, len(keys))
	paths := make([]string, 0, len(keys))
	prefer := make([]string, 0, len(keys))
	seen := make(map[ObservationKey]bool, len(keys))
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		persons = append(persons, k.PersonID)
		ides = append(ides, k.IdeType.String())
		repos = append(repos, k.Repository)
		paths = append(paths, k.SourcePath)
		prefer = append(prefer, k.PreferHash)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (k.person_id, k.ide_type, k.repository, k.source_path, k.prefer_hash)
			k.person_id, k.ide_type, k.repository, k.source_path, k.prefer_hash, i.content_hash, r.reported_at
		FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[]) AS k(person_id, ide_type, repository, source_path, prefer_hash)
		JOIN report_installed i ON i.person_id = k.person_id AND i.ide_type = k.ide_type
			AND i.repository = k.repository AND i.source_path = k.source_path
		JOIN agent_reports r ON r.person_id = i.person_id
		ORDER BY k.person_id, k.ide_type, k.repository, k.source_path, k.prefer_hash,
			(i.content_hash = k.prefer_hash) DESC, i.checkout`,
		persons, ides, repos, paths, prefer)
	if err != nil {
		return nil, fmt.Errorf("loading observations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			k   ObservationKey
			ide string
			o   Observation
		)
		if err := rows.Scan(&k.PersonID, &ide, &k.Repository, &k.SourcePath, &k.PreferHash, &o.ContentHash, &o.ReportedAt); err != nil {
			return nil, fmt.Errorf("scanning observation: %w", err)
		}
		k.IdeType = v1.IdeType(v1.IdeType_value[ide])
		out[k] = &o
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("loading observations: %w", err)
	}
	return out, nil
}

// Failure is a reported apply failure with the report that carried it.
type Failure struct {
	ContentHash string
	Reason      string
	ReportedAt  time.Time
}

// FailureKey names one placement of an artifact for one person.
type FailureKey struct {
	PersonID   string
	ArtifactID string
	Repository string
}

// Failures returns the apply failure reported at each key in one query; keys
// without one are absent.
func (s *Store) Failures(ctx context.Context, keys []FailureKey) (map[FailureKey]*Failure, error) {
	out := make(map[FailureKey]*Failure, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	persons := make([]string, 0, len(keys))
	artifacts := make([]string, 0, len(keys))
	repos := make([]string, 0, len(keys))
	seen := make(map[FailureKey]bool, len(keys))
	for _, k := range keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		persons = append(persons, k.PersonID)
		artifacts = append(artifacts, k.ArtifactID)
		repos = append(repos, k.Repository)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT k.person_id, k.artifact_id, k.repository, f.content_hash, f.reason, r.reported_at
		FROM unnest($1::text[], $2::text[], $3::text[]) AS k(person_id, artifact_id, repository)
		JOIN report_apply_failures f ON f.person_id = k.person_id AND f.artifact_id = k.artifact_id::uuid
			AND f.repository = k.repository
		JOIN agent_reports r ON r.person_id = f.person_id`,
		persons, artifacts, repos)
	if err != nil {
		return nil, fmt.Errorf("loading failures: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			k FailureKey
			f Failure
		)
		if err := rows.Scan(&k.PersonID, &k.ArtifactID, &k.Repository, &f.ContentHash, &f.Reason, &f.ReportedAt); err != nil {
			return nil, fmt.Errorf("scanning failure: %w", err)
		}
		out[k] = &f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("loading failures: %w", err)
	}
	return out, nil
}
