package service

import (
	"context"
	"fmt"
	"sort"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
	"github.com/tetriz-ai/tetriz-harness/pkg/audience"
	"github.com/tetriz-ai/tetriz-harness/pkg/spec"
	"github.com/tetriz-ai/tetriz-harness/pkg/status"
	"github.com/tetriz-ai/tetriz-harness/server/store"
)

// snapshot is the directory read once per grading pass, so every audience in
// the pass expands against the same persons and teams.
type snapshot struct {
	persons []string
	known   map[string]bool
	members map[string][]string
}

func (s snapshot) ActivePersons(context.Context) ([]string, error) { return s.persons, nil }

func (s snapshot) TeamMembers(_ context.Context, teamID string) ([]string, error) {
	m, ok := s.members[teamID]
	if !ok {
		return nil, audience.ErrTeamNotFound
	}
	return m, nil
}

// readSnapshot loads the persons and the members of each targeted team. It
// reads nothing when there are no targets.
func (a *Admin) readSnapshot(ctx context.Context, defs []*store.Artifact, targets map[string][]audience.Row) (snapshot, error) {
	snap := snapshot{known: map[string]bool{}, members: map[string][]string{}}
	anyRows := false
	teamSet := map[string]bool{}
	for _, d := range defs {
		for _, r := range targets[d.ID] {
			anyRows = true
			if r.TeamID != "" {
				teamSet[r.TeamID] = true
			}
		}
	}
	if !anyRows {
		return snap, nil
	}
	persons, err := a.dir.Persons(ctx)
	if err != nil {
		return snapshot{}, fmt.Errorf("listing persons: %w", err)
	}
	for _, p := range persons {
		if !snap.known[p.ID] {
			snap.known[p.ID] = true
			snap.persons = append(snap.persons, p.ID)
		}
	}
	teamIDs := make([]string, 0, len(teamSet))
	for id := range teamSet {
		teamIDs = append(teamIDs, id)
	}
	sort.Strings(teamIDs)
	for _, id := range teamIDs {
		m, err := a.dir.TeamMembers(ctx, id)
		if err != nil {
			return snapshot{}, fmt.Errorf("listing members of team %s: %w", id, err)
		}
		snap.members[id] = m
	}
	return snap, nil
}

type gradeJob struct {
	def     *store.Artifact
	p       audience.Placement
	persons []string
	path    string
	pathErr error
}

// grade computes each definition's status per targeted person per placement
// from a fixed number of store reads, however many definitions it is given,
// and one directory read per targeted team. Only persons the directory holds
// are graded. A
// retracted definition (DeletedAt set) is graded against the rows in targets,
// which for it are the ones its retraction removed.
func (a *Admin) grade(ctx context.Context, defs []*store.Artifact, targets map[string][]audience.Row) (map[string][]*v1.PersonEnforcementStatus, error) {
	snap, err := a.readSnapshot(ctx, defs, targets)
	if err != nil {
		return nil, err
	}

	var (
		jobs     []gradeJob
		live     []string
		liveSeen = map[string]bool{}
		obsKeys  []store.ObservationKey
		failKeys []store.FailureKey
	)
	for _, d := range defs {
		retracted := d.DeletedAt != nil
		for _, p := range audience.GroupByPlacement(targets[d.ID]) {
			expanded, err := audience.Expand(ctx, p.Rows, snap)
			if err != nil {
				return nil, fmt.Errorf("expanding audience: %w", err)
			}
			persons := expanded[:0]
			for _, pid := range expanded {
				if snap.known[pid] {
					persons = append(persons, pid)
				}
			}
			if len(persons) == 0 {
				continue
			}
			path, pathErr := spec.InstallPath(d.IdeType, d.Type, d.Slug, p.Scope)
			jobs = append(jobs, gradeJob{def: d, p: p, persons: persons, path: path, pathErr: pathErr})
			for _, pid := range persons {
				if pathErr == nil {
					obsKeys = append(obsKeys, store.ObservationKey{PersonID: pid, IdeType: d.IdeType, Repository: p.Repository, SourcePath: path, PreferHash: d.PrimaryHash})
					failKeys = append(failKeys, store.FailureKey{PersonID: pid, ArtifactID: d.ID, Repository: p.Repository})
				}
				if retracted {
					continue
				}
				if !liveSeen[pid] {
					liveSeen[pid] = true
					live = append(live, pid)
				}
			}
		}
	}
	summaries, err := a.store.ReportSummaries(ctx, live)
	if err != nil {
		return nil, fmt.Errorf("loading reports: %w", err)
	}
	observed, err := a.store.Observations(ctx, obsKeys)
	if err != nil {
		return nil, fmt.Errorf("loading observations: %w", err)
	}
	failures, err := a.store.Failures(ctx, failKeys)
	if err != nil {
		return nil, fmt.Errorf("loading failures: %w", err)
	}

	now := a.cfg.now()
	out := make(map[string][]*v1.PersonEnforcementStatus, len(defs))
	for _, j := range jobs {
		d, p := j.def, j.p
		for _, pid := range j.persons {
			var obs *status.Observation
			if o := observed[store.ObservationKey{PersonID: pid, IdeType: d.IdeType, Repository: p.Repository, SourcePath: j.path, PreferHash: d.PrimaryHash}]; j.pathErr == nil && o != nil {
				obs = &status.Observation{ContentHash: o.ContentHash, ObservedAt: o.ReportedAt}
			}
			var fail *status.Failure
			if f := failures[store.FailureKey{PersonID: pid, ArtifactID: d.ID, Repository: p.Repository}]; j.pathErr == nil && f != nil {
				fail = &status.Failure{ContentHash: f.ContentHash, Reason: f.Reason, ReportedAt: f.ReportedAt}
			}
			var dec status.Decision
			if d.DeletedAt != nil {
				dec = status.DecideRemoval(obs, fail)
			} else {
				in := status.Input{
					Now:          now,
					EnforceUntil: &d.EnforceUntil,
					PathErr:      j.pathErr,
					PrimaryHash:  d.PrimaryHash,
					RepoLocal:    p.Scope == v1.ArtifactScope_ARTIFACT_SCOPE_REPO_LOCAL,
					Observed:     obs,
				}
				if sum := summaries[pid]; sum != nil {
					in.HasReported = true
					in.RepoKnown = sum.KnownRepositories[p.Repository]
				}
				in.Failure = fail
				dec = status.Decide(in)
			}
			st := &v1.PersonEnforcementStatus{
				PersonId:   pid,
				Scope:      p.Scope,
				Repository: p.Repository,
				Status:     dec.Status,
				Detail:     dec.Reason,
			}
			if !dec.At.IsZero() {
				st.ObservedAt = timestamppb.New(dec.At)
			}
			out[d.ID] = append(out[d.ID], st)
		}
	}
	return out, nil
}

// countStatuses tallies graded rows into the summary counters.
func countStatuses(rows []*v1.PersonEnforcementStatus) *v1.StatusCounts {
	c := &v1.StatusCounts{}
	for _, r := range rows {
		switch r.GetStatus() {
		case v1.EnforcementStatus_ENFORCEMENT_STATUS_INSTALLED:
			c.Installed++
		case v1.EnforcementStatus_ENFORCEMENT_STATUS_PENDING:
			c.Pending++
		case v1.EnforcementStatus_ENFORCEMENT_STATUS_FAILED:
			c.Failed++
		case v1.EnforcementStatus_ENFORCEMENT_STATUS_EXCLUDED:
			c.Excluded++
		case v1.EnforcementStatus_ENFORCEMENT_STATUS_REMOVAL_PENDING:
			c.RemovalPending++
		case v1.EnforcementStatus_ENFORCEMENT_STATUS_REMOVED:
			c.Removed++
		case v1.EnforcementStatus_ENFORCEMENT_STATUS_EXPIRED:
			c.Expired++
		}
	}
	return c
}
