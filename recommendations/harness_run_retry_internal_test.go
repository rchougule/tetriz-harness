// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
	daomocks "github.com/tetriz-ai/tetriz-harness/recommendations/dao/mocks"
)

// TestFailSkipRun_CanceledCtxSkipsWrite: on a canceled handler ctx (pod shutdown), failRun/skipRun
// must NOT write the terminal — a write would race the redelivery that re-owns the still-RUNNING
// row (the reaper's StaleAfter sweep backstops a genuinely abandoned row). This mirrors the shipped
// derive engine's identical fail/skip contract. The MockITaskExecutionDAO has NO expectation, so the
// gomock controller (finished via NewController(t)'s cleanup) fails if either wrote.
func TestFailSkipRun_CanceledCtxSkipsWrite(t *testing.T) {
	ctrl := gomock.NewController(t)
	execDAO := daomocks.NewMockITaskExecutionDAO(ctrl)
	e := &HarnessImproveEngine{executionDAO: execDAO}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	e.failRun(ctx, "org", "exec", errors.New("pipeline boom"))
	e.skipRun(ctx, "org", "exec", "flag off")
}

// TestFailSkipRun_LiveCtxWritesTerminal: with a live ctx, failRun/skipRun WRITE the terminal (the
// MarkTerminalIfRunning expectations assert the writes happened).
func TestFailSkipRun_LiveCtxWritesTerminal(t *testing.T) {
	ctrl := gomock.NewController(t)
	execDAO := daomocks.NewMockITaskExecutionDAO(ctrl)
	execDAO.EXPECT().MarkTerminalIfRunning(gomock.Any(), "org", "exec",
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_FAILED, gomock.Any(), gomock.Any(), gomock.Any()).
		Return(true, nil)
	execDAO.EXPECT().MarkTerminalIfRunning(gomock.Any(), "org", "exec",
		taskexecutionsv1.TaskExecutionStatus_TASK_EXECUTION_STATUS_SKIPPED, gomock.Any(), gomock.Any(), gomock.Any()).
		Return(true, nil)
	// The terminal write runs inside Transactor.InTx; a pass-through mock runs the fn in-line so the
	// MarkTerminalIfRunning expectations above observe the write.
	tx := daomocks.NewMockTransactor(ctrl)
	tx.EXPECT().InTx(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }).AnyTimes()
	e := &HarnessImproveEngine{executionDAO: execDAO, tx: tx}

	e.failRun(context.Background(), "org", "exec", errors.New("boom"))
	e.skipRun(context.Background(), "org", "exec", "flag-disabled")
}

// TestRotateBodyWindow_PagesTheTail proves the capped body-fetch window PAGES the whole set across
// runs — so a repo with more than bodyCap distinct bodies is eventually fully scored instead of the
// tail being stranded — and is idempotent for a fixed seed (a run's retries fetch the same window).
func TestRotateBodyWindow_PagesTheTail(t *testing.T) {
	keys := []string{"a", "b", "c", "d", "e"} // 5 keys, cap 2 → 3 chunks: [a b] [c d] [e]
	const bodyCap = 2

	// Consecutive seeds page consecutive chunks; the union over ceil(total/cap) runs covers ALL keys.
	seen := map[string]bool{}
	for seed := int64(0); seed < 3; seed++ {
		for _, k := range rotateBodyWindow(keys, bodyCap, seed) {
			seen[k] = true
		}
	}
	require.Len(t, seen, len(keys), "over ceil(total/cap) runs every key is fetched — no permanent tail")

	require.Equal(t, []string{"a", "b"}, rotateBodyWindow(keys, bodyCap, 0))
	require.Equal(t, []string{"c", "d"}, rotateBodyWindow(keys, bodyCap, 1))
	require.Equal(t, []string{"e"}, rotateBodyWindow(keys, bodyCap, 2), "short last chunk")
	require.Equal(t, []string{"a", "b"}, rotateBodyWindow(keys, bodyCap, 3), "wraps after the chunk count")
	window7 := rotateBodyWindow(keys, bodyCap, 7)
	require.Equal(t, window7, rotateBodyWindow(keys, bodyCap, 7), "same seed → same window (idempotent for a run's retries)")
	require.Equal(t, keys, rotateBodyWindow(keys, 10, 5), "a set that fits is returned whole")
}

// TestMonthlyRotationSeed_AdvancesMonthly: the seed advances by one each month (the window pages) and
// is identical within a month (a run's retries pick the same window).
func TestMonthlyRotationSeed_AdvancesMonthly(t *testing.T) {
	jan := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	feb := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	janLate := time.Date(2026, 1, 28, 9, 0, 0, 0, time.UTC)
	require.Equal(t, monthlyRotationSeed(jan)+1, monthlyRotationSeed(feb), "consecutive months advance one chunk")
	require.Equal(t, monthlyRotationSeed(jan), monthlyRotationSeed(janLate), "same month → same window (idempotent)")
}
