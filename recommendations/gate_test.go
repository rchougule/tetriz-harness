// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
)

// NOTE: this is single-tenant, so the EvaluateRepoGate admission-floor coverage below is the
// meaningful part of the gate suite.

// stubReviewCounter is a hand-rolled stand-in for the one method EvaluateRepoGate
// reads through. Hand-rolled rather than gomock because the assertion here is about
// the floor comparison, and a fake keeps the boundary cases readable as data.
type stubReviewCounter struct {
	count int
	err   error
	// gotFrom/gotTo capture the window forwarded to the DAO, so the test can prove
	// the evaluator passes the caller's review window through unaltered.
	gotFrom, gotTo time.Time
}

func (s *stubReviewCounter) CountReviewBodiesForRepo(_ context.Context, _, _, _ string, from, to time.Time) (int, error) {
	s.gotFrom, s.gotTo = from, to
	if s.err != nil {
		return 0, s.err
	}
	return s.count, nil
}

// TestEvaluateRepoGate pins the admission boundary. The floor is a MINIMUM, so a
// repo sitting exactly on it derives — an off-by-one here silently withholds
// derivation from a qualifying repo (or admits one that should have been skipped),
// and the only visible symptom is an org that never produces artifacts.
func TestEvaluateRepoGate(t *testing.T) {
	const (
		admitted = taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_UNSPECIFIED
		fewRevs  = taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_REPO_FEW_REVIEWS
	)

	tests := []struct {
		name       string
		count      int
		floor      int
		wantReason taskexecutionsv1.TaskExecutionSubStatus
	}{
		{name: "below floor is skipped", count: 43, floor: 100, wantReason: fewRevs},
		{name: "one short of floor is skipped", count: 99, floor: 100, wantReason: fewRevs},
		{name: "exactly on floor is admitted", count: 100, floor: 100, wantReason: admitted},
		{name: "above floor is admitted", count: 101, floor: 100, wantReason: admitted},
		{name: "zero signal is skipped", count: 0, floor: 100, wantReason: fewRevs},
		// A zero/negative floor is an unconfigured gate, which must stay OPEN rather
		// than withhold every repo — a config-read returning 0 must not dark-ship a
		// fleet-wide derivation outage.
		{name: "zero floor admits a repo with no signal", count: 0, floor: 0, wantReason: admitted},
		{name: "negative floor admits", count: 0, floor: -1, wantReason: admitted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubReviewCounter{count: tt.count}
			res, err := EvaluateRepoGate(context.Background(), stub, "org-1", "ninjafactory/sbm-master", "bitbucket",
				time.Now().Add(-30*24*time.Hour), time.Now(), tt.floor)

			require.NoError(t, err)
			assert.Equal(t, tt.wantReason, res.SkipReason)
			assert.Equal(t, tt.count, res.ReviewBodies, "the count that drove the verdict must be reported for the operator read")
			assert.Equal(t, tt.wantReason == admitted, res.Admitted())
		})
	}
}

// TestEvaluateRepoGateReadError proves a failed count is surfaced, never silently
// reported as a SKIP. Mislabelling an unreadable repo as REPO_FEW_REVIEWS would
// record a terminal, auditable-looking verdict from missing data.
func TestEvaluateRepoGateReadError(t *testing.T) {
	stub := &stubReviewCounter{err: errors.New("boom")}

	res, err := EvaluateRepoGate(context.Background(), stub, "org-1", "repo", "bitbucket",
		time.Now().Add(-30*24*time.Hour), time.Now(), 100)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "per-repo gate read")
	assert.Equal(t, taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_UNSPECIFIED, res.SkipReason,
		"a read failure must not masquerade as a skip verdict")
}

// TestEvaluateRepoGateForwardsWindow pins that the caller's review window reaches
// the DAO unaltered — the evaluator must not re-derive or widen it, or the count
// would stop matching the corpus the derivation classifies.
func TestEvaluateRepoGateForwardsWindow(t *testing.T) {
	from := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	stub := &stubReviewCounter{count: 5}

	_, err := EvaluateRepoGate(context.Background(), stub, "org-1", "repo", "bitbucket", from, to, 100)

	require.NoError(t, err)
	assert.Equal(t, from, stub.gotFrom)
	assert.Equal(t, to, stub.gotTo)
}
