// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations_test

import (
	"context"
	"strings"
	"testing"

	"gorm.io/gorm"

	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations"
	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/config"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/testsupport"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// TestHarnessImproveEngine_PersistsDetectedFindings_Standalone is the acceptance milestone: it proves the
// engine runs end-to-end with only a local Postgres and no external services. It seeds a committed
// harness with a duplicate rule ID across two RULE files, runs the DETECTED
// per-repo scan (config-only detectors; the no-op LLM degrades the behavioral/fix legs), and asserts the
// dup-id finding lands in prompt_library as recommendation_source=DETECTED with is_latest set.
//
// It runs in one rolled-back transaction loading every migration; SKIPs cleanly without POSTGRES_TEST_DSN.
func TestHarnessImproveEngine_PersistsDetectedFindings_Standalone(t *testing.T) {
	tx := testsupport.OpenTestTx(t)
	testsupport.LoadMigrations(t, tx,
		"0001_session_signals.sql", "0002_vcs_pr_signals.sql", "0003_pm_signals.sql",
		"0004_harness_inventory.sql", "0005_identity.sql", "0006_prompt_library.sql",
		"0007_task_executions.sql", "0008_prompt_library_content.sql",
	)

	const (
		org    = "11111111-1111-1111-1111-111111111111"
		repoID = "33333333-3333-3333-3333-333333333331"
		file1  = "ff000000-0000-0000-0000-000000000001"
		file2  = "ff000000-0000-0000-0000-000000000002"
		art1   = "aa000000-0000-0000-0000-000000000001"
		art2   = "aa000000-0000-0000-0000-000000000002"
	)
	// Two committed RULE files that both define rule ID GO-001 — the duplicate the dup-id detector flags.
	body := "# Project rules\n\n- GO-001: Always wrap errors with context.\n"
	seedFile(t, tx, org, file1, "hash-1", body)
	seedFile(t, tx, org, file2, "hash-2", body)
	seedRepo(t, tx, org, repoID)
	seedRuleArtifact(t, tx, org, art1, repoID, file1, "CLAUDE/rules/a.md")
	seedRuleArtifact(t, tx, org, art2, repoID, file2, "CLAUDE/rules/b.md")

	// Wire the engine entirely from local collaborators — real DAOs over the tx, the content store, the
	// real transactor, and the NO-OP LLM (no tetriz-py).
	ctx := context.Background()
	execDAO := dao.NewTaskExecutionRepo(tx)
	detectorCfg := config.DefaultDetectorConfig()
	noop := recommendations.NewNoopLLMClient()
	engine := recommendations.NewHarnessImproveEngine(
		dao.NewGormTransactor(tx),
		execDAO,
		dao.NewHarnessArtifactRepo(tx),
		dao.NewRepositoryRepo(tx),
		dao.NewSecurityFlagDAO(tx),
		dao.NewPromptLibraryRepo(tx),
		dao.NewHarnessFileRepo(tx),
		dao.NewContentStore(tx),
		dao.NewSessionInsightDAO(tx),
		dao.NewHarnessUsageRecordsRepo(tx),
		noop, // harnessIntentClassifier (degrades)
		noop, // harnessFixGenerator (degrades)
		&detectorCfg,
		func(string) bool { return true },
	)

	// Mint the RUNNING task_executions row the engine resumes (the cmd loop's row-creation step).
	exec := &model.TaskExecution{
		OrganizationID: org,
		TaskType:       taskexecutionsv1.TaskType_TASK_TYPE_HARNESS_IMPROVE,
		TriggerType:    taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_MANUAL,
		DedupKey:       repoID,
	}
	created, err := execDAO.CreateIfNoneRunning(ctx, exec)
	if err != nil || !created {
		t.Fatalf("creating run row: created=%v err=%v", created, err)
	}

	if err := engine.Run(ctx, org, taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_MANUAL, exec.ID); err != nil {
		t.Fatalf("HarnessImproveEngine.Run: %v", err)
	}

	// THE MILESTONE ASSERTION: a DETECTED finding was persisted, is_latest, for the repo partition.
	var rows []struct {
		Slug                 string `gorm:"column:slug"`
		RecommendationSource string `gorm:"column:recommendation_source"`
		Dimension            string `gorm:"column:dimension"`
		Scope                string `gorm:"column:scope"`
		ScopeValue           string `gorm:"column:scope_value"`
		IsLatest             bool   `gorm:"column:is_latest"`
		SurfaceState         string `gorm:"column:surface_state"`
	}
	if err := tx.Raw(
		`SELECT slug, recommendation_source, dimension, scope, scope_value, is_latest, surface_state
		   FROM prompt_library WHERE organization_id = ? AND deleted_at IS NULL`, org,
	).Scan(&rows).Error; err != nil {
		t.Fatalf("reading prompt_library: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no prompt_library rows persisted — the engine did not persist any DETECTED finding standalone")
	}
	var dupID *int
	for i := range rows {
		r := rows[i]
		if r.RecommendationSource != "ARTIFACT_RECOMMENDATION_SOURCE_DETECTED" {
			t.Errorf("row %q source = %q, want DETECTED", r.Slug, r.RecommendationSource)
		}
		if r.Dimension == "HARNESS_FINDING_DIMENSION_CONFLICT" && strings.Contains(r.Slug, "dup") {
			dupID = &i
		}
	}
	if dupID == nil {
		t.Fatalf("no dup-id CONFLICT finding found; persisted rows = %+v", rows)
	}
	got := rows[*dupID]
	if !got.IsLatest {
		t.Errorf("dup-id finding is_latest = false, want true (the reconcile promotes the latest visible run)")
	}
	if got.Scope != "ARTIFACT_SCOPE_PROJECT" || got.ScopeValue != repoID {
		t.Errorf("dup-id finding scope = %s/%s, want PROJECT/%s", got.Scope, got.ScopeValue, repoID)
	}
	if got.SurfaceState != "ARTIFACT_SURFACE_STATE_VISIBLE" {
		t.Errorf("dup-id finding surface_state = %s, want VISIBLE", got.SurfaceState)
	}

	// The run reached a terminal status (not left RUNNING).
	final, err := execDAO.GetByID(ctx, org, exec.ID)
	if err != nil {
		t.Fatalf("reloading run: %v", err)
	}
	if final.Status == taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_RUNNING {
		t.Errorf("run left RUNNING, want a terminal status; got %s", final.Status)
	}
}

func seedFile(t *testing.T, tx *gorm.DB, org, id, hash, content string) {
	t.Helper()
	if err := tx.Exec(
		`INSERT INTO harness_files (id, organization_id, content_hash, status, content)
		 VALUES (?, ?, ?, 'HARNESS_FILE_STATUS_CONFIRMED', ?)`, id, org, hash, []byte(content)).Error; err != nil {
		t.Fatalf("seeding harness_files: %v", err)
	}
}

func seedRepo(t *testing.T, tx *gorm.DB, org, repoID string) {
	t.Helper()
	// A non-empty paths-only tree so the path lenses are "complete" — without it the conflict partition
	// reports path-lens-incomplete and the reconcile leaves the finding is_latest=false.
	tree := `{"truncated":false,"entries":[{"path":"src/main.go","type":"blob"},{"path":"README.md","type":"blob"}]}`
	if err := tx.Exec(
		`INSERT INTO vcs_repositories (id, organization_id, full_name, name, archived, tree_content)
		 VALUES (?, ?, 'acme/api', 'api', false, ?)`, repoID, org, tree).Error; err != nil {
		t.Fatalf("seeding vcs_repositories: %v", err)
	}
}

func seedRuleArtifact(t *testing.T, tx *gorm.DB, org, id, repoID, fileID, sourcePath string) {
	t.Helper()
	if err := tx.Exec(
		`INSERT INTO harness_artifacts
		 (id, organization_id, artifact_source, ide_type, scope, type, name, repository_id, source_path, current_file_id, operational_state)
		 VALUES (?, ?, 'HARNESS_ARTIFACT_SOURCE_VCS', 'IDE_TYPE_CLAUDE_CODE', 'HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED', 'HARNESS_ARTIFACT_TYPE_RULE', ?, ?, ?, ?, 'HARNESS_OPERATIONAL_STATE_ENABLED')`,
		id, org, sourcePath, repoID, sourcePath, fileID).Error; err != nil {
		t.Fatalf("seeding harness_artifacts: %v", err)
	}
}
