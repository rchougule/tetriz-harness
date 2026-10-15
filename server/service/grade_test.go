package service

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
	"github.com/tetriz-ai/tetriz-harness/pkg/audience"
	"github.com/tetriz-ai/tetriz-harness/server/store"
)

// countingStore records how often each grading read runs.
type countingStore struct {
	fakeStore
	calls map[string]int
}

func (c *countingStore) ListLiveTargets(ctx context.Context, ids []string) (map[string][]audience.Row, error) {
	c.calls["ListLiveTargets"]++
	return c.fakeStore.ListLiveTargets(ctx, ids)
}

func (c *countingStore) ListRetractedTargets(context.Context, []string) (map[string][]audience.Row, error) {
	c.calls["ListRetractedTargets"]++
	return map[string][]audience.Row{}, nil
}

func (c *countingStore) ReportSummaries(context.Context, []string) (map[string]*store.ReportSummary, error) {
	c.calls["ReportSummaries"]++
	return map[string]*store.ReportSummary{"p1": {KnownRepositories: map[string]bool{}}}, nil
}

func (c *countingStore) Observations(context.Context, []store.ObservationKey) (map[store.ObservationKey]*store.Observation, error) {
	c.calls["Observations"]++
	return map[store.ObservationKey]*store.Observation{}, nil
}

func (c *countingStore) Failures(context.Context, []store.FailureKey) (map[store.FailureKey]*store.Failure, error) {
	c.calls["Failures"]++
	return map[store.FailureKey]*store.Failure{}, nil
}

func TestListArtifactsGradesInOnePass(t *testing.T) {
	user := v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL
	now := fixedNow().AddDate(0, 0, 10)
	var defs []*store.Artifact
	targets := map[string][]audience.Row{}
	for i, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		defs = append(defs, &store.Artifact{ID: id, IdeType: claude, Type: rule, Slug: "r" + id, EnforceUntil: now})
		switch i {
		case 0:
			targets[id] = []audience.Row{{Scope: user}}
		case 1:
			targets[id] = []audience.Row{{Scope: user, TeamID: "t1"}}
		case 2:
			targets[id] = []audience.Row{{Scope: user, PersonID: "p1"}}
		case 4:
			// A person the host no longer lists drops out of status.
			targets[id] = []audience.Row{{Scope: user, PersonID: "gone"}, {Scope: user, TeamID: "t1"}}
		}
	}
	cs := &countingStore{fakeStore: fakeStore{artifacts: defs, targets: targets}, calls: map[string]int{}}
	dir := &fakeDir{persons: []Person{{ID: "p1"}, {ID: "p2"}}, members: map[string][]string{"t1": {"p2", "gone"}}}
	a := NewAdmin(cs, dir, nil, Config{Now: fixedNow})
	resp, err := a.ListArtifacts(context.Background(), connect.NewRequest(&v1.ListArtifactsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ListLiveTargets", "ListRetractedTargets", "ReportSummaries", "Observations", "Failures"} {
		if cs.calls[name] != 1 {
			t.Errorf("%s ran %d times for %d artifacts, want 1", name, cs.calls[name], len(defs))
		}
	}
	// Two definitions target t1, which is still read once.
	for _, name := range []string{"Persons", "TeamMembers"} {
		if dir.calls[name] != 1 {
			t.Errorf("directory %s ran %d times, want 1", name, dir.calls[name])
		}
	}
	want := map[string][2]int32{"a1": {1, 1}, "a2": {0, 1}, "a3": {1, 0}, "a4": {0, 0}, "a5": {0, 1}}
	for _, s := range resp.Msg.GetArtifacts() {
		sc := s.GetStatusCounts()
		if got := [2]int32{sc.GetPending(), sc.GetExcluded()}; got != want[s.GetArtifactId()] {
			t.Errorf("%s pending/excluded = %v, want %v", s.GetArtifactId(), got, want[s.GetArtifactId()])
		}
	}
}
