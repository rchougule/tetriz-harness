// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"fmt"
	"time"

	taskexecutionsv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/taskexecutions/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/dao"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// GateReader owns the reads behind the per-repo admission gate: which
// repos are in scope, and how much in-window review signal each one carries. It
// needs only the repo + raw-record DAOs, so a caller that wants the gate's answer
// without the derivation machinery (the DERIVATION_GATE_EXPLAIN operator read)
// constructs one of these instead of a full Aggregator.
//
// Aggregator delegates its own ListInScopeProjects / CountReviewBodiesForRepo here
// so there is exactly ONE implementation of each. Two copies would let an operator
// read print a count the live gate never computed — the failure this type exists to
// prevent.
type GateReader struct {
	repoDAO      dao.IRepositoryDAO
	rawRecordDAO dao.IRawRecordDAO
}

// NewGateReader wires the gate's two read dependencies.
func NewGateReader(repoDAO dao.IRepositoryDAO, rawRecordDAO dao.IRawRecordDAO) *GateReader {
	return &GateReader{repoDAO: repoDAO, rawRecordDAO: rawRecordDAO}
}

// ListInScopeProjects returns the org's active repos that have a fetched tree.
// A repo with no tree is grounded against nothing, so it cannot derive.
func (g *GateReader) ListInScopeProjects(ctx context.Context, orgID string) ([]*model.Repository, error) {
	repos, err := g.repoDAO.ListAllActiveForOrg(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("listing in-scope projects for org %s: %w", orgID, err)
	}
	inScope := make([]*model.Repository, 0, len(repos))
	for _, repo := range repos {
		if repo.TreeKey == nil || *repo.TreeKey == "" {
			continue // no fetched tree → nothing to ground against; skip
		}
		inScope = append(inScope, repo)
	}
	return inScope, nil
}

// CountReviewBodiesForRepo returns the count of in-window review/PR comments for
// one repo — the per-repo gate's review-signal floor input.
func (g *GateReader) CountReviewBodiesForRepo(ctx context.Context, orgID, repoFullName, connectorID string, from, to time.Time) (int, error) {
	count, err := g.rawRecordDAO.CountReviewBodies(ctx, orgID, repoFullName, connectorID, from, to)
	if err != nil {
		return 0, fmt.Errorf("counting review bodies for org %s repo %s: %w", orgID, repoFullName, err)
	}
	return count, nil
}

// RepoGateResult is one repo's admission outcome: the review-signal count that
// drove it, and the SKIP sub-status (UNSPECIFIED means admitted).
type RepoGateResult struct {
	// ReviewBodies is the in-window review/PR-comment count compared against the floor.
	ReviewBodies int
	// SkipReason is UNSPECIFIED when the repo is admitted.
	SkipReason taskexecutionsv1.TaskExecutionSubStatus
}

// Admitted reports whether the gate lets this repo derive.
func (r RepoGateResult) Admitted() bool {
	return r.SkipReason == taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_UNSPECIFIED
}

// EvaluateRepoGate applies the review-comment floor to one repo and returns the
// count alongside the verdict. It is the SINGLE definition of that decision, so
// every caller agrees by construction rather than by review.
//
// floor is supplied by the caller rather than read here, because a caller sources
// it from its own config accessor while the comparison itself must stay identical.
// A floor <= 0 admits every repo (no floor configured), which is the documented
// open-gate behaviour, not a refusal.
//
// The boundary is `count < floor`, so a repo sitting exactly on the floor is
// ADMITTED. Reads run in whatever tenant scope the caller already holds.
func EvaluateRepoGate(
	ctx context.Context,
	reader interface {
		CountReviewBodiesForRepo(ctx context.Context, orgID, repoFullName, connectorID string, from, to time.Time) (int, error)
	},
	orgID, repoFullName, connectorID string,
	reviewFrom, to time.Time,
	floor int,
) (RepoGateResult, error) {
	count, err := reader.CountReviewBodiesForRepo(ctx, orgID, repoFullName, connectorID, reviewFrom, to)
	if err != nil {
		return RepoGateResult{}, fmt.Errorf("per-repo gate read for org %s repo %s: %w", orgID, repoFullName, err)
	}
	res := RepoGateResult{
		ReviewBodies: count,
		SkipReason:   taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_UNSPECIFIED,
	}
	if count < floor {
		res.SkipReason = taskexecutionsv1.TaskExecutionSubStatus_TASK_EXECUTION_SUB_STATUS_REPO_FEW_REVIEWS
	}
	return res, nil
}
