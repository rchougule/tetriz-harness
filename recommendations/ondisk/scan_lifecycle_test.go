// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package ondisk_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/testsupport"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
	"github.com/tetriz-ai/tetriz-harness/recommendations/ondisk"
)

func TestScan_RejectedRunPreservesInventory(t *testing.T) {
	tx := testsupport.OpenTestTx(t)
	testsupport.LoadMigrations(t, tx, allMigrations...)
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(path, []byte("original instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := ondisk.Scan(context.Background(), tx, ondisk.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	exec := &model.TaskExecution{OrganizationID: first.OrgID, DedupKey: first.RepoID, TaskType: taskexecutionsv1.TaskType_TASK_TYPE_HARNESS_IMPROVE, TriggerType: taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_MANUAL}
	if created, err := dao.NewTaskExecutionRepo(tx).CreateIfNoneRunning(context.Background(), exec); err != nil || !created {
		t.Fatalf("create running scan: %v, %v", created, err)
	}
	if err := os.WriteFile(path, []byte("replacement instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ondisk.Scan(context.Background(), tx, ondisk.Options{Dir: dir}); err == nil || !strings.Contains(err.Error(), "already in flight") {
		t.Fatalf("expected rejected scan, got %v", err)
	}
	var row struct{ Content []byte }
	if err := tx.Raw(`SELECT f.content FROM harness_artifacts a JOIN harness_files f ON f.id = a.current_file_id WHERE a.repository_id = ?`, first.RepoID).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if string(row.Content) != "original instructions" {
		t.Fatalf("rejected scan changed inventory to %q", row.Content)
	}
}

func TestScan_SeedFailureRollsBackAndAllowsRetry(t *testing.T) {
	tx := testsupport.OpenTestTx(t)
	testsupport.LoadMigrations(t, tx, allMigrations...)
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(path, []byte("original instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := ondisk.Scan(context.Background(), tx, ondisk.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Exec(`ALTER TABLE harness_files ADD CONSTRAINT reject_replacement CHECK (content <> convert_to('replacement instructions', 'UTF8')) NOT VALID`).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ondisk.Scan(context.Background(), tx, ondisk.Options{Dir: dir}); err == nil {
		t.Fatal("expected seed failure")
	}
	var count int64
	if err := tx.Raw(`SELECT count(*) FROM harness_artifacts WHERE repository_id = ?`, first.RepoID).Scan(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("failed seed lost prior inventory: %d artifacts", count)
	}
	if err := tx.Exec(`ALTER TABLE harness_files DROP CONSTRAINT reject_replacement`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := ondisk.Scan(context.Background(), tx, ondisk.Options{Dir: dir}); err != nil {
		t.Fatalf("retry after failed seed: %v", err)
	}
}

func TestScan_FailedEngineIsAnErrorAndAllowsRetry(t *testing.T) {
	tx := testsupport.OpenTestTx(t)
	testsupport.LoadMigrations(t, tx, allMigrations...)
	if err := tx.Exec(`ALTER TABLE task_executions ADD CONSTRAINT reject_success CHECK (status NOT IN ('TASK_EXECUTION_STATUS_COMPLETED', 'TASK_EXECUTION_STATUS_COMPLETED_NO_VISIBLE')) NOT VALID`).Error; err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result, err := ondisk.Scan(context.Background(), tx, ondisk.Options{Dir: dir}); err == nil || result != nil {
		t.Fatalf("failed engine reported success: result=%+v err=%v", result, err)
	}
	if err := tx.Exec(`ALTER TABLE task_executions DROP CONSTRAINT reject_success`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := ondisk.Scan(context.Background(), tx, ondisk.Options{Dir: dir}); err != nil {
		t.Fatalf("retry after failed engine: %v", err)
	}
}
