package service

import (
	"context"
	"slices"
	"time"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
	"github.com/tetriz-ai/tetriz-harness/pkg/audience"
	"github.com/tetriz-ai/tetriz-harness/server/store"
)

// fakeStore implements only what a test needs; any other call panics on the
// nil embedded interface, which flags a handler reaching storage it should not.
type fakeStore struct {
	Store
	paused    bool
	artifacts []*store.Artifact
	targets   map[string][]audience.Row
	files     map[string][]store.ArtifactFile
	setCalls  int
}

func (f *fakeStore) Paused(context.Context) (bool, error) { return f.paused, nil }

func (f *fakeStore) ListLiveArtifacts(context.Context) ([]*store.Artifact, error) {
	return f.artifacts, nil
}

func (f *fakeStore) ListLiveTargets(context.Context, []string) (map[string][]audience.Row, error) {
	return f.targets, nil
}

func (f *fakeStore) ListArtifactFiles(context.Context, []string) (map[string][]store.ArtifactFile, error) {
	return f.files, nil
}

func (f *fakeStore) SetTargets(_ context.Context, _ string, _ v1.ArtifactScope, _ string, _ []audience.Row, until time.Time) (time.Time, error) {
	f.setCalls++
	return until, nil
}

func (f *fakeStore) GetArtifact(_ context.Context, id string, _ bool) (*store.Artifact, error) {
	for _, a := range f.artifacts {
		if a.ID == id {
			return a, nil
		}
	}
	return nil, store.ErrNotFound
}

func fixedNow() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// fakeDir is an in-memory host directory that counts its reads.
type fakeDir struct {
	persons []Person
	teams   []Team
	members map[string][]string
	calls   map[string]int
}

func (d *fakeDir) count(name string) {
	if d.calls == nil {
		d.calls = map[string]int{}
	}
	d.calls[name]++
}

func (d *fakeDir) Persons(context.Context) ([]Person, error) {
	d.count("Persons")
	return d.persons, nil
}

func (d *fakeDir) Teams(context.Context) ([]Team, error) {
	d.count("Teams")
	return d.teams, nil
}

func (d *fakeDir) TeamMembers(_ context.Context, teamID string) ([]string, error) {
	d.count("TeamMembers")
	return d.members[teamID], nil
}

func (d *fakeDir) PersonTeams(_ context.Context, personID string) ([]string, error) {
	d.count("PersonTeams")
	var out []string
	for team, ms := range d.members {
		if slices.Contains(ms, personID) {
			out = append(out, team)
		}
	}
	return out, nil
}
