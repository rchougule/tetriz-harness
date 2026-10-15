// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/testsupport"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// Real-Postgres tests for the run-lifecycle DAO and the transaction seam: single-in-flight create, the
// terminal compare-and-set, the stale reaper, and that Transactor.InTx commits on success and rolls back
// on error (including the atomicity guarantee — a terminal stamp rolls back with a failing
// persist). Each runs in one rolled-back transaction loading 0007.

func openTaskExecutionTx(t *testing.T) *gorm.DB {
	t.Helper()
	tx := testsupport.OpenTestTx(t)
	loadSchemaFile(t, tx, "0007_task_executions.sql")
	return tx
}

const (
	teOrgA  = "11111111-1111-1111-1111-111111111111"
	teRepo1 = "33333333-3333-3333-3333-333333333331"
	teRepo2 = "33333333-3333-3333-3333-333333333332"
)

func newExec(dedupKey string, taskType taskexecutionsv1.TaskType) *model.TaskExecution {
	return &model.TaskExecution{
		OrganizationID: teOrgA,
		TaskType:       taskType,
		TriggerType:    taskexecutionsv1.TaskTriggerType_TASK_TRIGGER_TYPE_MANUAL,
		DedupKey:       dedupKey,
	}
}

func TestTaskExecutionRepo_CreateIfNoneRunning_SingleInFlight(t *testing.T) {
	tx := openTaskExecutionTx(t)
	repo := NewTaskExecutionRepo(tx)
	ctx := context.Background()
	improve := taskexecutionsv1.TaskType_TASK_TYPE_HARNESS_IMPROVE

	first := newExec(teRepo1, improve)
	created, err := repo.CreateIfNoneRunning(ctx, first)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if !created || first.ID == "" {
		t.Fatalf("first create = (%v, id=%q), want created with a minted id", created, first.ID)
	}

	// A second RUNNING for the SAME (org, task_type, dedup_key) must lose the single-in-flight slot.
	second := newExec(teRepo1, improve)
	created, err = repo.CreateIfNoneRunning(ctx, second)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created {
		t.Error("second create should be blocked by single-in-flight (created=false)")
	}

	// A different dedup_key is a different partition — it gets its own slot.
	other := newExec(teRepo2, improve)
	created, err = repo.CreateIfNoneRunning(ctx, other)
	if err != nil {
		t.Fatalf("other-partition create: %v", err)
	}
	if !created {
		t.Error("a different dedup_key must get its own in-flight slot")
	}
}

func TestTaskExecutionRepo_MarkTerminalIfRunning_OnlyWhenRunning(t *testing.T) {
	tx := openTaskExecutionTx(t)
	repo := NewTaskExecutionRepo(tx)
	ctx := context.Background()
	improve := taskexecutionsv1.TaskType_TASK_TYPE_HARNESS_IMPROVE

	exec := newExec(teRepo1, improve)
	if _, err := repo.CreateIfNoneRunning(ctx, exec); err != nil {
		t.Fatalf("seeding running row: %v", err)
	}

	marked, err := repo.MarkTerminalIfRunning(ctx, teOrgA, exec.ID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED,
		taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_UNSPECIFIED, time.Now().UTC(), "")
	if err != nil {
		t.Fatalf("first mark terminal: %v", err)
	}
	if !marked {
		t.Fatal("marking a RUNNING row terminal should return marked=true")
	}
	got, err := repo.GetByID(ctx, teOrgA, exec.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED {
		t.Errorf("status = %s, want COMPLETED", got.Status)
	}

	// A second mark on the now-COMPLETED (non-RUNNING) row must be a no-op → marked=false (the
	// superseded/reaped branch the engine's errRunSuperseded relies on).
	marked, err = repo.MarkTerminalIfRunning(ctx, teOrgA, exec.ID,
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_FAILED,
		taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_UNSPECIFIED, time.Now().UTC(), "late")
	if err != nil {
		t.Fatalf("second mark terminal: %v", err)
	}
	if marked {
		t.Error("marking a non-RUNNING row should return marked=false")
	}
}

func TestTaskExecutionRepo_ReapStaleRunning(t *testing.T) {
	tx := openTaskExecutionTx(t)
	repo := NewTaskExecutionRepo(tx)
	ctx := context.Background()
	improve := taskexecutionsv1.TaskType_TASK_TYPE_HARNESS_IMPROVE

	// A stale RUNNING row (started two hours ago). StartedAt is pre-set, so CreateIfNoneRunning keeps it.
	stale := newExec(teRepo1, improve)
	stale.StartedAt = time.Now().UTC().Add(-2 * time.Hour)
	if _, err := repo.CreateIfNoneRunning(ctx, stale); err != nil {
		t.Fatalf("seeding stale row: %v", err)
	}
	// A fresh RUNNING row (different partition) that must survive the reap.
	fresh := newExec(teRepo2, improve)
	if _, err := repo.CreateIfNoneRunning(ctx, fresh); err != nil {
		t.Fatalf("seeding fresh row: %v", err)
	}

	reaped, err := repo.ReapStaleRunning(ctx, teOrgA, improve, time.Hour)
	if err != nil {
		t.Fatalf("ReapStaleRunning: %v", err)
	}
	if reaped != 1 {
		t.Errorf("reaped = %d, want 1 (only the two-hour-old row)", reaped)
	}
	staleAfter, err := repo.GetByID(ctx, teOrgA, stale.ID)
	if err != nil {
		t.Fatalf("GetByID stale: %v", err)
	}
	if staleAfter.Status != taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_FAILED {
		t.Errorf("stale row status = %s, want FAILED", staleAfter.Status)
	}
	freshAfter, err := repo.GetByID(ctx, teOrgA, fresh.ID)
	if err != nil {
		t.Fatalf("GetByID fresh: %v", err)
	}
	if freshAfter.Status != taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_RUNNING {
		t.Errorf("fresh row status = %s, want RUNNING (not reaped)", freshAfter.Status)
	}

	// A non-positive window is refused rather than mass-reaping live slots.
	if _, err := repo.ReapStaleRunning(ctx, teOrgA, improve, 0); err == nil {
		t.Error("ReapStaleRunning with staleAfter=0 should be refused")
	}
}

func TestGormTransactor_InTx_CommitAndRollback(t *testing.T) {
	tx := openTaskExecutionTx(t)
	repo := NewTaskExecutionRepo(tx)
	txor := NewGormTransactor(tx)
	ctx := context.Background()
	improve := taskexecutionsv1.TaskType_TASK_TYPE_HARNESS_IMPROVE

	// Commit: a row created inside InTx is visible after a nil return.
	var committedID string
	if err := txor.InTx(ctx, func(c context.Context) error {
		exec := newExec(teRepo1, improve)
		created, err := repo.CreateIfNoneRunning(c, exec)
		if err != nil {
			return err
		}
		if !created {
			return errors.New("expected create inside tx")
		}
		committedID = exec.ID
		return nil
	}); err != nil {
		t.Fatalf("InTx commit: %v", err)
	}
	if _, err := repo.GetByID(ctx, teOrgA, committedID); err != nil {
		t.Errorf("committed row should be readable after InTx: %v", err)
	}

	// Rollback: a row created inside InTx is gone after a non-nil return, and the error propagates.
	sentinel := errors.New("boom")
	var rolledID string
	err := txor.InTx(ctx, func(c context.Context) error {
		exec := newExec(teRepo2, improve)
		if _, createErr := repo.CreateIfNoneRunning(c, exec); createErr != nil {
			return createErr
		}
		rolledID = exec.ID
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx should propagate fn's error, got %v", err)
	}
	if _, err := repo.GetByID(ctx, teOrgA, rolledID); !errors.Is(err, ErrNotFound) {
		t.Errorf("rolled-back row should be absent, got err %v", err)
	}
}

// The atomicity guarantee: a terminal stamp made inside InTx rolls back when a later step in the
// same transaction fails — so a persist failure never leaves a half-promoted terminal row.
func TestGormTransactor_InTx_TerminalStampRollsBackOnFailure(t *testing.T) {
	tx := openTaskExecutionTx(t)
	repo := NewTaskExecutionRepo(tx)
	txor := NewGormTransactor(tx)
	ctx := context.Background()
	improve := taskexecutionsv1.TaskType_TASK_TYPE_HARNESS_IMPROVE

	exec := newExec(teRepo1, improve)
	if _, err := repo.CreateIfNoneRunning(ctx, exec); err != nil {
		t.Fatalf("seeding running row: %v", err)
	}

	sentinel := errors.New("persist failed")
	err := txor.InTx(ctx, func(c context.Context) error {
		marked, markErr := repo.MarkTerminalIfRunning(c, teOrgA, exec.ID,
			taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_COMPLETED,
			taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_UNSPECIFIED, time.Now().UTC(), "")
		if markErr != nil {
			return markErr
		}
		if !marked {
			return errors.New("expected marked=true")
		}
		// Simulate a persist failure after the terminal stamp.
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx should propagate the persist error, got %v", err)
	}
	got, err := repo.GetByID(ctx, teOrgA, exec.ID)
	if err != nil {
		t.Fatalf("GetByID after rollback: %v", err)
	}
	if got.Status != taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_RUNNING {
		t.Errorf("status = %s, want RUNNING (the terminal stamp must roll back with the failed persist)", got.Status)
	}
}
