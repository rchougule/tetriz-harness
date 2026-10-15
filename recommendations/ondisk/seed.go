// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package ondisk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Deterministic UUIDv5 namespaces for the seeded ids. Re-scanning the same harness must mint the SAME
// harness_artifacts and harness_files ids: a DETECTED finding's slug is hashed from the artifact id
// (see conflictSlug / securitySlug), so a changing id would give the same underlying issue a different
// slug every scan — breaking cross-scan suppression and loosening the re-scan reconcile. The names are
// fixed random UUIDs used only as the SHA-1 namespace seed.
var (
	artifactIDNamespace = uuid.MustParse("0054da7a-0000-4000-8000-0000000000a1")
	fileIDNamespace     = uuid.MustParse("0054da7a-0000-4000-8000-0000000000f1")
)

// mintArtifactID derives a stable harness_artifacts id from the (org, repo, source-path, type) key.
// The source path is unique per artifact within a repo (the settings.json children carry distinct
// "#permissions"/"#hook:" suffixes), so the key identifies the artifact across scans.
func mintArtifactID(orgID, repoID, sourcePath, artifactType string) string {
	key := strings.Join([]string{orgID, repoID, sourcePath, artifactType}, "\x00")
	return uuid.NewSHA1(artifactIDNamespace, []byte(key)).String()
}

// mintFileID derives a stable harness_files id from the (org, content-hash) key — the same key the
// content-addressed lookup-or-create dedups on — so a body first stored on a fresh DB gets the same
// id every scan.
func mintFileID(orgID, contentHash string) string {
	key := strings.Join([]string{orgID, contentHash}, "\x00")
	return uuid.NewSHA1(fileIDNamespace, []byte(key)).String()
}

// treeBlobEntry mirrors the paths-only repo-tree entry shape the engine parses from
// vcs_repositories.tree_content : a blob path + its type.
type treeBlobEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

// treeBlob is the paths-only repo tree the reader fills. truncated is always false for a local walk —
// nothing is clipped — so the non-existence-asserting path lenses (dead-glob, broken-path) always run.
type treeBlob struct {
	Truncated bool            `json:"truncated"`
	Entries   []treeBlobEntry `json:"entries"`
}

// Seed writes the inventory into the harness-inventory tables under (orgID, repoID): one vcs_repositories
// row carrying the paths-only tree, one harness_files row per DISTINCT body (content-addressed by
// sha256, so settings.json children share one row), and one harness_artifacts row per classified
// artifact (artifact_source=VCS, person-less, pointing at its content file). After Seed the config-only
// DETECTED detectors run against this repo via the normal engine read path.
//
// Seed is idempotent on the (orgID, repoID) partition: it clears any prior seed of that partition (the
// repo row and its artifacts) before re-seeding, so re-scanning the same directory reconciles in place
// rather than conflicting on the vcs_repositories primary key. Content-addressed harness_files are
// reused when a body is already stored (shared across repos within the org), so a re-scan adds no
// duplicate file rows. It runs every statement on db (pass a transaction to make the seed atomic).
func (inv *Inventory) Seed(ctx context.Context, db *gorm.DB, orgID, repoID string) error {
	conn := db.WithContext(ctx)

	// Clear the (org, repo) partition so a re-scan reconciles instead of PK-conflicting on the repo row.
	// Artifacts are deleted outright (re-inserted below); harness_files are content-addressed and shared,
	// so they are reused rather than deleted (see the lookup-or-create below).
	if err := conn.Exec(
		`DELETE FROM harness_artifacts WHERE organization_id = ? AND repository_id = ?`,
		orgID, repoID,
	).Error; err != nil {
		return fmt.Errorf("clearing harness_artifacts for repo %s: %w", repoID, err)
	}
	if err := conn.Exec(
		`DELETE FROM vcs_repositories WHERE id = ? AND organization_id = ?`,
		repoID, orgID,
	).Error; err != nil {
		return fmt.Errorf("clearing vcs_repositories for repo %s: %w", repoID, err)
	}

	tree := treeBlob{Entries: make([]treeBlobEntry, 0, len(inv.RepoPaths))}
	for _, p := range inv.RepoPaths {
		tree.Entries = append(tree.Entries, treeBlobEntry{Path: p, Type: "blob"})
	}
	treeJSON, err := json.Marshal(tree)
	if err != nil {
		return fmt.Errorf("marshalling repo tree for repo %s: %w", repoID, err)
	}
	if err := conn.Exec(
		`INSERT INTO vcs_repositories (id, organization_id, full_name, name, archived, tree_content)
		 VALUES (?, ?, ?, ?, false, ?)`,
		repoID, orgID, "ondisk/"+repoID, "ondisk", string(treeJSON),
	).Error; err != nil {
		return fmt.Errorf("seeding vcs_repositories for repo %s: %w", repoID, err)
	}

	// Content-address each distinct body to one harness_files row so artifacts sharing a body (the
	// settings.json children) share one file, mirroring the VCS ingest's content dedup. A body already
	// stored under this org is reused (its id looked up), so a re-scan grows no duplicate file rows.
	fileIDByHash := map[string]string{}
	for _, artifact := range inv.Artifacts {
		hash := bodyHash(artifact.Body)
		if _, done := fileIDByHash[hash]; done {
			continue
		}
		fileID, err := ensureHarnessFile(ctx, db, orgID, hash, artifact.Body)
		if err != nil {
			return fmt.Errorf("seeding harness_files (hash %s): %w", hash, err)
		}
		fileIDByHash[hash] = fileID
	}

	for _, artifact := range inv.Artifacts {
		fileID := fileIDByHash[bodyHash(artifact.Body)]
		if err := conn.Exec(
			`INSERT INTO harness_artifacts
			 (id, organization_id, artifact_source, ide_type, scope, type, name, repository_id, source_path, current_file_id, operational_state)
			 VALUES (?, ?, 'HARNESS_ARTIFACT_SOURCE_VCS', ?, ?, ?, ?, ?, ?, ?, 'HARNESS_OPERATIONAL_STATE_ENABLED')`,
			mintArtifactID(orgID, repoID, artifact.SourcePath, artifact.Type.String()),
			orgID, artifact.IdeType.String(), artifact.Scope.String(), artifact.Type.String(),
			artifact.Name, repoID, artifact.SourcePath, fileID,
		).Error; err != nil {
			return fmt.Errorf("seeding harness_artifacts (%s): %w", artifact.SourcePath, err)
		}
	}
	return nil
}

// ensureHarnessFile returns the id of the harness_files row for (orgID, hash), reusing an existing row
// when the content-addressed body is already stored and inserting one otherwise. This keeps a re-scan
// from growing duplicate file rows for bodies that have not changed.
func ensureHarnessFile(ctx context.Context, db *gorm.DB, orgID, hash string, body []byte) (string, error) {
	var existingID string
	err := db.WithContext(ctx).Raw(
		`SELECT id FROM harness_files WHERE organization_id = ? AND content_hash = ? LIMIT 1`,
		orgID, hash,
	).Scan(&existingID).Error
	if err != nil {
		return "", err
	}
	if existingID != "" {
		return existingID, nil
	}
	fileID := mintFileID(orgID, hash)
	if err := db.WithContext(ctx).Exec(
		`INSERT INTO harness_files (id, organization_id, content_hash, byte_size, status, content)
		 VALUES (?, ?, ?, ?, 'HARNESS_FILE_STATUS_CONFIRMED', ?)`,
		fileID, orgID, hash, int64(len(body)), body,
	).Error; err != nil {
		return "", err
	}
	return fileID, nil
}

// bodyHash is the content-address of a harness body: its sha256 hex.
func bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
