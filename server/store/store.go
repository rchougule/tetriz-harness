// Package store is the Postgres persistence for the control plane: plain SQL
// over pgx, one transaction per multi-row write.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
	"github.com/tetriz-ai/tetriz-harness/pkg/audience"
)

var (
	// ErrNotFound is returned when the addressed row does not exist (or is retracted).
	ErrNotFound = errors.New("not found")
	// ErrSlugExists is returned when a write would give two live definitions one (ide_type, type, slug).
	ErrSlugExists = errors.New("an artifact with this slug already exists for this ide_type and type")
	// ErrRootDocExists is returned when a second root-doc definition would share a target.
	ErrRootDocExists = errors.New("a CLAUDE.md or AGENTS.md is already enforced for this scope, repository and target")
	// ErrTooManyArtifacts is returned when a create would exceed the live-definition cap.
	ErrTooManyArtifacts = errors.New("the enforced-artifact limit is reached")
)

// Store is the Postgres-backed store.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to dsn and applies pending migrations.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("opening pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrating: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Pool exposes the pool for tests.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Artifact is one definition.
type Artifact struct {
	ID           string
	IdeType      v1.IdeType
	Type         v1.ArtifactType
	Slug         string
	Name         string
	PrimaryHash  string
	HookConfig   *v1.HookConfig
	EnforceUntil time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    *time.Time
}

// ArtifactFile is one stored file of a definition; the primary has RelativePath "".
type ArtifactFile struct {
	RelativePath string
	ContentHash  string
	IsPrimary    bool
}

// Blob is the metadata of one content-addressed body.
type Blob struct {
	ContentHash string
	Size        int64
}

func pgCode(err error) (code, constraint string) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code, pgErr.ConstraintName
	}
	return "", ""
}

// mapWriteErr turns the constraint violations callers can cause into sentinels.
func mapWriteErr(err error) error {
	code, constraint := pgCode(err)
	switch {
	case code == "23505" && constraint == "ux_org_artifacts_live":
		return ErrSlugExists
	case code == "23505" && constraint == "ux_enforced_root_doc_live":
		return ErrRootDocExists
	}
	return err
}

func marshalHookConfig(hc *v1.HookConfig) ([]byte, error) {
	if hc == nil {
		return nil, nil
	}
	b, err := protojson.Marshal(hc)
	if err != nil {
		return nil, fmt.Errorf("marshalling hook_config: %w", err)
	}
	return b, nil
}

const artifactColumns = `id::text, ide_type, type, slug, name, primary_hash, hook_config, enforce_until, created_at, updated_at, deleted_at`

func scanArtifact(row pgx.Row) (*Artifact, error) {
	var (
		a        Artifact
		ide, typ string
		hook     []byte
	)
	if err := row.Scan(&a.ID, &ide, &typ, &a.Slug, &a.Name, &a.PrimaryHash, &hook, &a.EnforceUntil, &a.CreatedAt, &a.UpdatedAt, &a.DeletedAt); err != nil {
		return nil, err
	}
	a.IdeType = v1.IdeType(v1.IdeType_value[ide])
	a.Type = v1.ArtifactType(v1.ArtifactType_value[typ])
	if hook != nil {
		a.HookConfig = &v1.HookConfig{}
		if err := protojson.Unmarshal(hook, a.HookConfig); err != nil {
			return nil, fmt.Errorf("decoding hook_config: %w", err)
		}
	}
	return &a, nil
}

func insertBlobs(ctx context.Context, tx pgx.Tx, blobs []Blob) error {
	for _, b := range blobs {
		if _, err := tx.Exec(ctx, `INSERT INTO blobs (content_hash, size_bytes) VALUES ($1, $2) ON CONFLICT (content_hash) DO NOTHING`, b.ContentHash, b.Size); err != nil {
			return fmt.Errorf("recording blob: %w", err)
		}
	}
	return nil
}

func replaceFiles(ctx context.Context, tx pgx.Tx, artifactID string, files []ArtifactFile) error {
	if _, err := tx.Exec(ctx, `DELETE FROM artifact_files WHERE artifact_id = $1`, artifactID); err != nil {
		return fmt.Errorf("clearing files: %w", err)
	}
	for _, f := range files {
		if _, err := tx.Exec(ctx, `INSERT INTO artifact_files (artifact_id, relative_path, content_hash, is_primary) VALUES ($1, $2, $3, $4)`,
			artifactID, f.RelativePath, f.ContentHash, f.IsPrimary); err != nil {
			return fmt.Errorf("writing file %q: %w", f.RelativePath, err)
		}
	}
	return nil
}

// CreateArtifact inserts a definition, or, when a live one already holds its
// (ide_type, type, slug), replaces that one's name, body and window and keeps
// its id. maxLive caps the number of live definitions a genuine create may
// reach.
func (s *Store) CreateArtifact(ctx context.Context, a Artifact, files []ArtifactFile, blobs []Blob, maxLive int) (*Artifact, error) {
	hook, err := marshalHookConfig(a.HookConfig)
	if err != nil {
		return nil, err
	}
	var out *Artifact
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM org_artifacts WHERE ide_type = $1 AND type = $2 AND slug = $3 AND deleted_at IS NULL)`,
			a.IdeType.String(), a.Type.String(), a.Slug).Scan(&exists); err != nil {
			return fmt.Errorf("checking natural key: %w", err)
		}
		if !exists {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM org_artifacts WHERE deleted_at IS NULL`).Scan(&n); err != nil {
				return fmt.Errorf("counting definitions: %w", err)
			}
			if n >= maxLive {
				return ErrTooManyArtifacts
			}
		}
		if err := insertBlobs(ctx, tx, blobs); err != nil {
			return err
		}
		// The conflict arbiter repeats the live index's predicate, so two racing
		// creates of one slug converge on a single live row.
		row := tx.QueryRow(ctx, `
			INSERT INTO org_artifacts (ide_type, type, slug, name, primary_hash, hook_config, enforce_until)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (ide_type, type, slug) WHERE deleted_at IS NULL DO UPDATE SET
				name = EXCLUDED.name, primary_hash = EXCLUDED.primary_hash, hook_config = EXCLUDED.hook_config,
				enforce_until = EXCLUDED.enforce_until, updated_at = now()
			RETURNING `+artifactColumns,
			a.IdeType.String(), a.Type.String(), a.Slug, a.Name, a.PrimaryHash, hook, a.EnforceUntil)
		created, err := scanArtifact(row)
		if err != nil {
			return fmt.Errorf("upserting definition: %w", mapWriteErr(err))
		}
		if err := replaceFiles(ctx, tx, created.ID, files); err != nil {
			return err
		}
		out = created
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("creating artifact: %w", mapWriteErr(err))
	}
	return out, nil
}

// UpdateArtifact rewrites a live definition's slug, name, hook config and
// window; files and blobs replace its body unless files is nil, which keeps
// the stored body.
func (s *Store) UpdateArtifact(ctx context.Context, a Artifact, files []ArtifactFile, blobs []Blob) (*Artifact, error) {
	hook, err := marshalHookConfig(a.HookConfig)
	if err != nil {
		return nil, err
	}
	var out *Artifact
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := insertBlobs(ctx, tx, blobs); err != nil {
			return err
		}
		primary := a.PrimaryHash
		if files == nil {
			primary = ""
		}
		row := tx.QueryRow(ctx, `
			UPDATE org_artifacts SET slug = $2, name = $3, hook_config = $4, enforce_until = $5,
				primary_hash = COALESCE(NULLIF($6, ''), primary_hash), updated_at = now()
			WHERE id = $1 AND deleted_at IS NULL
			RETURNING `+artifactColumns,
			a.ID, a.Slug, a.Name, hook, a.EnforceUntil, primary)
		updated, err := scanArtifact(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("updating definition: %w", mapWriteErr(err))
		}
		if files != nil {
			if err := replaceFiles(ctx, tx, updated.ID, files); err != nil {
				return err
			}
		}
		out = updated
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("updating artifact: %w", mapWriteErr(err))
	}
	return out, nil
}

// GetArtifact returns a definition by id; a retracted one only when includeRetracted.
func (s *Store) GetArtifact(ctx context.Context, id string, includeRetracted bool) (*Artifact, error) {
	q := `SELECT ` + artifactColumns + ` FROM org_artifacts WHERE id = $1`
	if !includeRetracted {
		q += ` AND deleted_at IS NULL`
	}
	a, err := scanArtifact(s.pool.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("loading artifact: %w", err)
	}
	return a, nil
}

// ListLiveArtifacts returns every live definition, oldest first.
func (s *Store) ListLiveArtifacts(ctx context.Context) ([]*Artifact, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+artifactColumns+` FROM org_artifacts WHERE deleted_at IS NULL ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("listing artifacts: %w", err)
	}
	defer rows.Close()
	var out []*Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning artifact: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing artifacts: %w", err)
	}
	return out, nil
}

// ListRetractedArtifacts returns every retracted definition, newest retraction first.
func (s *Store) ListRetractedArtifacts(ctx context.Context) ([]*Artifact, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+artifactColumns+` FROM org_artifacts WHERE deleted_at IS NOT NULL ORDER BY deleted_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("listing retracted artifacts: %w", err)
	}
	defer rows.Close()
	var out []*Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning artifact: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing retracted artifacts: %w", err)
	}
	return out, nil
}

// ListArtifactFiles returns each definition's files, primary first then by path.
func (s *Store) ListArtifactFiles(ctx context.Context, artifactIDs []string) (map[string][]ArtifactFile, error) {
	out := make(map[string][]ArtifactFile, len(artifactIDs))
	if len(artifactIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT artifact_id::text, relative_path, content_hash, is_primary FROM artifact_files
		WHERE artifact_id = ANY($1::text[]::uuid[]) ORDER BY artifact_id, is_primary DESC, relative_path`, artifactIDs)
	if err != nil {
		return nil, fmt.Errorf("listing artifact files: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var f ArtifactFile
		if err := rows.Scan(&id, &f.RelativePath, &f.ContentHash, &f.IsPrimary); err != nil {
			return nil, fmt.Errorf("scanning artifact file: %w", err)
		}
		out[id] = append(out[id], f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing artifact files: %w", err)
	}
	return out, nil
}

func scanTargets(rows pgx.Rows) (map[string][]audience.Row, error) {
	defer rows.Close()
	out := map[string][]audience.Row{}
	for rows.Next() {
		var (
			artifactID, scope        string
			repository, person, team *string
			r                        audience.Row
		)
		if err := rows.Scan(&r.ID, &artifactID, &scope, &repository, &person, &team); err != nil {
			return nil, fmt.Errorf("scanning target: %w", err)
		}
		r.Scope = v1.ArtifactScope(v1.ArtifactScope_value[scope])
		if repository != nil {
			r.Repository = *repository
		}
		if person != nil {
			r.PersonID = *person
		}
		if team != nil {
			r.TeamID = *team
		}
		out[artifactID] = append(out[artifactID], r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading targets: %w", err)
	}
	return out, nil
}

const targetColumns = `id::text, artifact_id::text, scope, repository, person_id, team_id`

// ListLiveTargets returns the live routing rows of each definition, oldest first.
func (s *Store) ListLiveTargets(ctx context.Context, artifactIDs []string) (map[string][]audience.Row, error) {
	if len(artifactIDs) == 0 {
		return map[string][]audience.Row{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+targetColumns+` FROM enforced_artifacts
		WHERE artifact_id = ANY($1::text[]::uuid[]) AND deleted_at IS NULL ORDER BY created_at, id`, artifactIDs)
	if err != nil {
		return nil, fmt.Errorf("listing targets: %w", err)
	}
	return scanTargets(rows)
}

// ListRetractedTargets returns, per retracted definition, the routing rows its
// retraction removed: those stamped with the definition's own deleted_at.
func (s *Store) ListRetractedTargets(ctx context.Context, artifactIDs []string) (map[string][]audience.Row, error) {
	if len(artifactIDs) == 0 {
		return map[string][]audience.Row{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT e.id::text, e.artifact_id::text, e.scope, e.repository, e.person_id, e.team_id
		FROM enforced_artifacts e JOIN org_artifacts a ON a.id = e.artifact_id
		WHERE e.artifact_id = ANY($1::text[]::uuid[]) AND a.deleted_at IS NOT NULL AND e.deleted_at = a.deleted_at
		ORDER BY e.created_at, e.id`, artifactIDs)
	if err != nil {
		return nil, fmt.Errorf("listing retracted targets: %w", err)
	}
	return scanTargets(rows)
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// SetTargets replaces the live rows of one (scope, repository) placement with
// desired and restarts the definition's window, keeping a later close already
// stored; it returns the resulting close. A root-doc definition may not take a
// slot another definition of its type holds.
func (s *Store) SetTargets(ctx context.Context, artifactID string, scope v1.ArtifactScope, repository string, desired []audience.Row, enforceUntil time.Time) (time.Time, error) {
	var until time.Time
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// The row lock serialises edits of one definition.
		var typ string
		err := tx.QueryRow(ctx, `SELECT type FROM org_artifacts WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, artifactID).Scan(&typ)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("locking definition: %w", err)
		}
		liveRows, err := tx.Query(ctx, `SELECT `+targetColumns+` FROM enforced_artifacts WHERE artifact_id = $1 AND deleted_at IS NULL`, artifactID)
		if err != nil {
			return fmt.Errorf("listing targets: %w", err)
		}
		liveByArtifact, err := scanTargets(liveRows)
		if err != nil {
			return err
		}
		live := liveByArtifact[artifactID]

		t := v1.ArtifactType(v1.ArtifactType_value[typ])
		if t == v1.ArtifactType_ARTIFACT_TYPE_CLAUDE_MD || t == v1.ArtifactType_ARTIFACT_TYPE_AGENTS_MD {
			otherRows, err := tx.Query(ctx, `SELECT `+targetColumns+` FROM enforced_artifacts
				WHERE type = $1 AND artifact_id <> $2 AND deleted_at IS NULL`, typ, artifactID)
			if err != nil {
				return fmt.Errorf("listing root-doc targets: %w", err)
			}
			others, err := scanTargets(otherRows)
			if err != nil {
				return err
			}
			for _, rows := range others {
				if audience.Collides(rows, desired) {
					return ErrRootDocExists
				}
			}
		}

		for _, r := range desired {
			if _, err := tx.Exec(ctx, `
				INSERT INTO enforced_artifacts (artifact_id, type, scope, repository, person_id, team_id)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (artifact_id, scope, repository, person_id, team_id) WHERE deleted_at IS NULL
				DO UPDATE SET updated_at = now()`,
				artifactID, typ, r.Scope.String(), nullable(r.Repository), nullable(r.PersonID), nullable(r.TeamID)); err != nil {
				return fmt.Errorf("writing target: %w", mapWriteErr(err))
			}
		}
		for _, r := range audience.Prune(live, desired, scope, repository) {
			if _, err := tx.Exec(ctx, `UPDATE enforced_artifacts SET deleted_at = now() WHERE id = $1`, r.ID); err != nil {
				return fmt.Errorf("removing target: %w", err)
			}
		}
		if err := tx.QueryRow(ctx, `UPDATE org_artifacts SET enforce_until = GREATEST(enforce_until, $2), updated_at = now()
			WHERE id = $1 RETURNING enforce_until`, artifactID, enforceUntil).Scan(&until); err != nil {
			return fmt.Errorf("restarting window: %w", err)
		}
		return nil
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("setting targets: %w", mapWriteErr(err))
	}
	return until, nil
}

// RetractArtifact retracts a live definition and every live row routed off it
// at one instant, so the retracted audience stays addressable.
func (s *Store) RetractArtifact(ctx context.Context, id string) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var at time.Time
		err := tx.QueryRow(ctx, `UPDATE org_artifacts SET deleted_at = now(), updated_at = now() WHERE id = $1 AND deleted_at IS NULL RETURNING deleted_at`, id).Scan(&at)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("retracting definition: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE enforced_artifacts SET deleted_at = $2 WHERE artifact_id = $1 AND deleted_at IS NULL`, id, at); err != nil {
			return fmt.Errorf("retracting targets: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("retracting artifact: %w", err)
	}
	return nil
}

// Paused reports the kill switch.
func (s *Store) Paused(ctx context.Context) (bool, error) {
	var p bool
	if err := s.pool.QueryRow(ctx, `SELECT paused FROM settings WHERE id`).Scan(&p); err != nil {
		return false, fmt.Errorf("reading settings: %w", err)
	}
	return p, nil
}

// SetPaused sets the kill switch.
func (s *Store) SetPaused(ctx context.Context, paused bool) error {
	if _, err := s.pool.Exec(ctx, `UPDATE settings SET paused = $1, updated_at = now() WHERE id`, paused); err != nil {
		return fmt.Errorf("writing settings: %w", err)
	}
	return nil
}
