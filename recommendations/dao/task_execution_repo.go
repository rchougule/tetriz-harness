// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package dao

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// ErrNotFound is returned by a by-id read or a masked update that matches no live row.
var ErrNotFound = errors.New("record not found")

// taskExecutionColumnMap maps each TaskExecutionFieldMask enum value to its SQL column name. The
// field-mask enum is hand-maintained here — keep it in sync with the enum as it grows.
var taskExecutionColumnMap = map[taskexecutionsv1.TaskExecutionFieldMask]string{
	taskexecutionsv1.TaskExecutionFieldMask_TASK_EXECUTION_FIELD_MASK_STATUS:         "status",
	taskexecutionsv1.TaskExecutionFieldMask_TASK_EXECUTION_FIELD_MASK_COMPLETED_AT:   "completed_at",
	taskexecutionsv1.TaskExecutionFieldMask_TASK_EXECUTION_FIELD_MASK_INPUT_PAYLOAD:  "input_payload",
	taskexecutionsv1.TaskExecutionFieldMask_TASK_EXECUTION_FIELD_MASK_OUTPUT_PAYLOAD: "output_payload",
	taskexecutionsv1.TaskExecutionFieldMask_TASK_EXECUTION_FIELD_MASK_ERROR_MESSAGE:  "error_message",
	taskexecutionsv1.TaskExecutionFieldMask_TASK_EXECUTION_FIELD_MASK_DEDUP_KEY:      "dedup_key",
	taskexecutionsv1.TaskExecutionFieldMask_TASK_EXECUTION_FIELD_MASK_SUBSTATUS:      "substatus",
}

// SelectColumnsForTaskExecution resolves a mask slice to SQL column names. UNSPECIFIED / unknown
// entries map to "" and are dropped; duplicates are deduped.
func SelectColumnsForTaskExecution(masks []taskexecutionsv1.TaskExecutionFieldMask) []string {
	seen := make(map[string]struct{}, len(masks))
	columns := make([]string, 0, len(masks))
	for _, mask := range masks {
		column := taskExecutionColumnMap[mask]
		if column == "" {
			continue
		}
		if _, ok := seen[column]; ok {
			continue
		}
		seen[column] = struct{}{}
		columns = append(columns, column)
	}
	return columns
}

// TaskExecutionRepo implements ITaskExecutionDAO over PostgreSQL via GORM. Single-tenant: the org is
// bound in every query, and every matching record is read. Enum columns are compared by their full proto
// value NAME string (`.String()`) — the generated enums carry no driver.Valuer, so a raw enum bind would
// use the ordinal and match the text column nothing.
type TaskExecutionRepo struct {
	db *gorm.DB
}

// NewTaskExecutionRepo builds the repo over db.
func NewTaskExecutionRepo(db *gorm.DB) *TaskExecutionRepo {
	return &TaskExecutionRepo{db: db}
}

var _ ITaskExecutionDAO = (*TaskExecutionRepo)(nil)

// CreateIfNoneRunning inserts a fresh RUNNING run for exec unless a RUNNING run already exists for the
// same (organization_id, task_type, dedup_key) — the DB-enforced single-in-flight guarantee. It uses
// INSERT … ON CONFLICT DO NOTHING against the partial-unique idx_te_single_in_flight, so a concurrent
// second fire does NOT raise a unique violation that would abort the caller's transaction; the conflict
// is detected via RowsAffected and reported as created=false (the caller skips). On created=true exec.ID
// is populated from RETURNING.
//
// The caller sets OrganizationID, TaskType, TriggerType, TriggeredBy, DedupKey; this stamps
// Status=RUNNING, StartedAt (if unset), and a non-nil empty InputPayload so the NOT NULL jsonb column is
// satisfied before the engine fills the real content ref via Update.
func (r *TaskExecutionRepo) CreateIfNoneRunning(ctx context.Context, exec *model.TaskExecution) (bool, error) {
	if exec == nil {
		return false, errors.New("create-if-none-running: exec cannot be nil")
	}
	if exec.OrganizationID == "" || exec.DedupKey == "" {
		return false, errors.New("create-if-none-running: organization_id and dedup_key are required")
	}
	exec.Status = taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_RUNNING
	if exec.StartedAt.IsZero() {
		exec.StartedAt = time.Now().UTC()
	}
	if exec.InputPayload == nil {
		exec.InputPayload = &taskexecutionsv1.ContentRef{}
	}
	// TargetWhere must mirror idx_te_single_in_flight's predicate exactly so Postgres uses that partial
	// index as the conflict arbiter. The status literal is inlined (NOT a `?` parameter): a bound
	// parameter is not folded to a constant under a generic plan, so a parameterized predicate fails to
	// match the index's literal predicate (error 42P10). The value is a controlled enum name, so
	// inlining it is safe.
	runningStatus := taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_RUNNING.String()
	res := connFromContext(ctx, r.db).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "organization_id"}, {Name: "task_type"}, {Name: "dedup_key"}},
		TargetWhere: clause.Where{Exprs: []clause.Expression{
			clause.Expr{SQL: fmt.Sprintf("status = '%s' AND deleted_at IS NULL", runningStatus)},
		}},
		DoNothing: true,
	}).Create(exec)
	if res.Error != nil {
		return false, fmt.Errorf("create-if-none-running for org %s dedup_key %s: %w", exec.OrganizationID, exec.DedupKey, res.Error)
	}
	return res.RowsAffected > 0, nil
}

// GetByID returns the task execution for (org, id), or ErrNotFound when no non-deleted row matches. The
// engine resumes a run by the id minted at create time; the lookup is org-scoped for defense-in-depth.
func (r *TaskExecutionRepo) GetByID(ctx context.Context, orgID, id string) (*model.TaskExecution, error) {
	if orgID == "" || id == "" {
		return nil, errors.New("getting task execution: orgID and id cannot be empty")
	}
	var row model.TaskExecution
	if err := connFromContext(ctx, r.db).Where("id = ? AND organization_id = ?", id, orgID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("getting task execution %s: %w", id, err)
	}
	return &row, nil
}

// Update writes the masked columns of exec onto the row identified by (org, id), plus updated_at. masks
// must resolve to ≥1 known column. Select forces a masked zero/nil column to be written rather than
// skipped (so the engine can stamp an input/output content ref). A zero-row update is ErrNotFound: for a
// run state-machine a lost status write would strand the run RUNNING, so it fails loud.
func (r *TaskExecutionRepo) Update(ctx context.Context, exec *model.TaskExecution, masks []taskexecutionsv1.TaskExecutionFieldMask) error {
	if exec == nil {
		return errors.New("updating task execution: exec cannot be nil")
	}
	if exec.ID == "" {
		return errors.New("updating task execution: id cannot be empty")
	}
	columns := SelectColumnsForTaskExecution(masks)
	if len(columns) == 0 {
		return errors.New("updating task execution: masks resolved to no known columns")
	}
	scoped := connFromContext(ctx, r.db).Model(&model.TaskExecution{}).Where("id = ?", exec.ID)
	if exec.OrganizationID != "" {
		scoped = scoped.Where("organization_id = ?", exec.OrganizationID)
	}
	res := scoped.Select(columns).Updates(exec)
	if res.Error != nil {
		return fmt.Errorf("updating task execution %s: %w", exec.ID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("updating task execution %s: %w", exec.ID, ErrNotFound)
	}
	return nil
}

// MarkTerminalIfRunning compare-and-sets the terminal status: it stamps status + substatus +
// completed_at ONLY while the row is still RUNNING and reports whether it applied. The status=RUNNING
// predicate takes the row lock, so it races safely against ReapStaleRunning — exactly one wins, and a
// stale worker that loses gets marked=false and must roll back rather than overwrite the reaped row.
// error_message is set only when supplied, so a FAILED transition stays attributable while a COMPLETED
// one leaves the column NULL.
func (r *TaskExecutionRepo) MarkTerminalIfRunning(ctx context.Context, orgID, id string, status taskexecutionsv1.TaskExecutionStatus, substatus taskexecutionsv1.TaskExecutionSubStatus, completedAt time.Time, errorMessage string) (bool, error) {
	if orgID == "" || id == "" {
		return false, errors.New("marking task execution terminal: orgID and id cannot be empty")
	}
	updates := map[string]interface{}{
		// .String() stores the enum-name string (the status column's text form). A map-based update
		// bypasses the struct serializer, so bind the name explicitly.
		"status":       status.String(),
		"substatus":    substatus.String(),
		"completed_at": completedAt,
		"updated_at":   completedAt,
	}
	if errorMessage != "" {
		updates["error_message"] = errorMessage
	}
	running := taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_RUNNING.String()
	res := connFromContext(ctx, r.db).Model(&model.TaskExecution{}).
		Where("id = ? AND organization_id = ? AND status = ?", id, orgID, running).
		Updates(updates)
	if res.Error != nil {
		return false, fmt.Errorf("marking task execution %s terminal for org %s: %w", id, orgID, res.Error)
	}
	// RowsAffected == 0 means the row was no longer RUNNING (reaped / superseded) — not an error: the
	// caller rolls back its tx and acks the run as a no-op.
	return res.RowsAffected > 0, nil
}

// ReapStaleRunning fails any RUNNING run of the given task_type for the org whose started_at is older
// than staleAfter — freeing the single-in-flight slot a crashed/lost run would otherwise hold forever.
// Sets error_message so the failure is attributable. Returns the count reaped. A non-positive staleAfter
// is refused rather than mass-reaping every live slot.
func (r *TaskExecutionRepo) ReapStaleRunning(ctx context.Context, orgID string, taskType taskexecutionsv1.TaskType, staleAfter time.Duration) (int64, error) {
	if orgID == "" {
		return 0, errors.New("reap stale running: orgID cannot be empty")
	}
	if staleAfter <= 0 {
		return 0, fmt.Errorf("reap stale running: staleAfter must be positive, got %s", staleAfter)
	}
	reapedAt := time.Now().UTC()
	cutoff := reapedAt.Add(-staleAfter)
	running := taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_RUNNING.String()
	res := connFromContext(ctx, r.db).Model(&model.TaskExecution{}).
		Where("organization_id = ? AND task_type = ? AND status = ? AND started_at < ?",
			orgID, taskType.String(), running, cutoff).
		Updates(map[string]interface{}{
			"status":        taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_FAILED.String(),
			"error_message": "reaped: stale running run exceeded staleness window",
			"completed_at":  reapedAt,
			"updated_at":    reapedAt,
		})
	if res.Error != nil {
		return 0, fmt.Errorf("reaping stale running runs for org %s: %w", orgID, res.Error)
	}
	return res.RowsAffected, nil
}
