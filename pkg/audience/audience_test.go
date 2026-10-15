package audience

import (
	"context"
	"errors"
	"reflect"
	"testing"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
)

const (
	userGlobal = v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL
	repoLocal  = v1.ArtifactScope_ARTIFACT_SCOPE_REPO_LOCAL
)

func TestResolve(t *testing.T) {
	tests := []struct {
		name          string
		orgWide       bool
		persons       []string
		teams         []string
		wantErr       error
		wantSpecified bool
	}{
		{"none is unspecified", false, nil, nil, nil, false},
		{"org-wide", true, nil, nil, nil, true},
		{"persons", false, []string{"p1", "p2"}, nil, nil, true},
		{"teams", false, nil, []string{"t1"}, nil, true},
		{"org-wide and persons", true, []string{"p1"}, nil, ErrInvalid, false},
		{"org-wide and teams", true, nil, []string{"t1"}, ErrInvalid, false},
		{"persons and teams", false, []string{"p1"}, []string{"t1"}, ErrInvalid, false},
		{"all three", true, []string{"p1"}, []string{"t1"}, ErrInvalid, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := Resolve(tt.orgWide, tt.persons, tt.teams)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if a.Specified() != tt.wantSpecified {
				t.Fatalf("Specified = %v, want %v", a.Specified(), tt.wantSpecified)
			}
		})
	}
}

func TestRows(t *testing.T) {
	tests := []struct {
		name  string
		aud   Audience
		scope v1.ArtifactScope
		repo  string
		want  []Row
	}{
		{"org-wide is one selector-less row", Audience{OrgWide: true}, userGlobal, "", []Row{{Scope: userGlobal}}},
		{"one row per person", Audience{PersonIDs: []string{"p1", "p2"}}, repoLocal, "o/r",
			[]Row{{Scope: repoLocal, Repository: "o/r", PersonID: "p1"}, {Scope: repoLocal, Repository: "o/r", PersonID: "p2"}}},
		{"one row per team", Audience{TeamIDs: []string{"t1"}}, userGlobal, "", []Row{{Scope: userGlobal, TeamID: "t1"}}},
		{"unspecified yields none", Audience{}, userGlobal, "", []Row{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.aud.Rows(tt.scope, tt.repo); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Rows = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestTargetKeyEqual(t *testing.T) {
	a := Row{ID: "x", Scope: repoLocal, Repository: "o/r", PersonID: "p1"}
	tests := []struct {
		name string
		b    Row
		want bool
	}{
		{"same slot, different id", Row{ID: "y", Scope: repoLocal, Repository: "o/r", PersonID: "p1"}, true},
		{"other scope", Row{Scope: userGlobal, PersonID: "p1"}, false},
		{"other repo", Row{Scope: repoLocal, Repository: "o/s", PersonID: "p1"}, false},
		{"other person", Row{Scope: repoLocal, Repository: "o/r", PersonID: "p2"}, false},
		{"team instead of person", Row{Scope: repoLocal, Repository: "o/r", TeamID: "p1"}, false},
		{"org-wide", Row{Scope: repoLocal, Repository: "o/r"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TargetKeyEqual(a, tt.b); got != tt.want {
				t.Fatalf("TargetKeyEqual = %v, want %v", got, tt.want)
			}
		})
	}
	if !TargetKeyEqual(Row{Scope: userGlobal}, Row{Scope: userGlobal}) {
		t.Fatal("two org-wide user-global rows must be the same slot")
	}
}

func TestGroupByPlacement(t *testing.T) {
	rows := []Row{
		{ID: "1", Scope: repoLocal, Repository: "o/b", PersonID: "p1"},
		{ID: "2", Scope: userGlobal, PersonID: "p2"},
		{ID: "3", Scope: repoLocal, Repository: "o/a", TeamID: "t1"},
		{ID: "4", Scope: repoLocal, Repository: "o/b", PersonID: "p3"},
	}
	got := GroupByPlacement(rows)
	want := []Placement{
		{Scope: userGlobal, Rows: []Row{rows[1]}},
		{Scope: repoLocal, Repository: "o/a", Rows: []Row{rows[2]}},
		{Scope: repoLocal, Repository: "o/b", Rows: []Row{rows[0], rows[3]}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GroupByPlacement = %+v, want %+v", got, want)
	}

	audiences := []Audience{
		{PersonIDs: []string{"p2"}},
		{TeamIDs: []string{"t1"}},
		{PersonIDs: []string{"p1", "p3"}},
	}
	for i, p := range got {
		if a := p.Audience(); !reflect.DeepEqual(a, audiences[i]) {
			t.Errorf("placement %d Audience = %+v, want %+v", i, a, audiences[i])
		}
	}
	if a := (Placement{Rows: []Row{{Scope: userGlobal}}}).Audience(); !a.OrgWide {
		t.Error("an org-wide row folds to OrgWide")
	}
}

// An edit replaces the audience of the one placement it names; a placement it
// did not show must survive, or "also enforce on repo B" would un-enforce repo A.
func TestPrune(t *testing.T) {
	live := []Row{
		{ID: "a1", Scope: repoLocal, Repository: "o/a", PersonID: "p1"},
		{ID: "a2", Scope: repoLocal, Repository: "o/a", PersonID: "p2"},
		{ID: "b1", Scope: repoLocal, Repository: "o/b", PersonID: "p1"},
		{ID: "g1", Scope: userGlobal, PersonID: "p2"},
	}
	tests := []struct {
		name    string
		desired []Row
		scope   v1.ArtifactScope
		repo    string
		want    []string
	}{
		{"drops the unticked person at the edited placement", Audience{PersonIDs: []string{"p1"}}.Rows(repoLocal, "o/a"), repoLocal, "o/a", []string{"a2"}},
		{"switching to org-wide drops every person row there", Audience{OrgWide: true}.Rows(repoLocal, "o/a"), repoLocal, "o/a", []string{"a1", "a2"}},
		{"empty audience clears the placement", nil, repoLocal, "o/a", []string{"a1", "a2"}},
		{"unchanged audience prunes nothing", Audience{PersonIDs: []string{"p1", "p2"}}.Rows(repoLocal, "o/a"), repoLocal, "o/a", nil},
		{"user-global edit never touches repo placements", Audience{PersonIDs: []string{"p9"}}.Rows(userGlobal, ""), userGlobal, "", []string{"g1"}},
		{"a new placement prunes nothing", Audience{PersonIDs: []string{"p1"}}.Rows(repoLocal, "o/c"), repoLocal, "o/c", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, r := range Prune(live, tt.desired, tt.scope, tt.repo) {
				got = append(got, r.ID)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Prune = %v, want %v", got, tt.want)
			}
		})
	}
}

// Two root docs of one type at the same target write the same file; the same
// type in a different repository is a different file.
func TestCollides(t *testing.T) {
	existing := []Row{
		{Scope: repoLocal, Repository: "o/a"},
		{Scope: userGlobal, PersonID: "p1"},
	}
	tests := []struct {
		name    string
		desired []Row
		want    bool
	}{
		{"same repo org-wide", Audience{OrgWide: true}.Rows(repoLocal, "o/a"), true},
		{"different repo", Audience{OrgWide: true}.Rows(repoLocal, "o/b"), false},
		{"same person user-global", Audience{PersonIDs: []string{"p1"}}.Rows(userGlobal, ""), true},
		{"other person user-global", Audience{PersonIDs: []string{"p2"}}.Rows(userGlobal, ""), false},
		{"nothing desired", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Collides(existing, tt.desired); got != tt.want {
				t.Fatalf("Collides = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchPerson(t *testing.T) {
	orgUser := Row{ID: "org-ug", Scope: userGlobal}
	personUser := Row{ID: "p-ug", Scope: userGlobal, PersonID: "me"}
	otherUser := Row{ID: "other-ug", Scope: userGlobal, PersonID: "someone"}
	teamRepo := Row{ID: "team-a", Scope: repoLocal, Repository: "o/a", TeamID: "t1"}
	foreignTeamRepo := Row{ID: "team-b", Scope: repoLocal, Repository: "o/b", TeamID: "t2"}
	personRepo := Row{ID: "p-a", Scope: repoLocal, Repository: "o/a", PersonID: "me"}

	myTeams := map[string]bool{"t1": true}
	tests := []struct {
		name      string
		rows      []Row
		teamsErr  error
		wantIDs   []string
		wantCalls int
		wantErr   bool
	}{
		{"org-wide reaches everyone", []Row{orgUser}, nil, []string{"org-ug"}, 0, false},
		{"direct person", []Row{personUser, otherUser}, nil, []string{"p-ug"}, 0, false},
		{"another person's row does not", []Row{otherUser}, nil, nil, 0, false},
		{"org-wide and person at one placement install once", []Row{orgUser, personUser}, nil, []string{"org-ug"}, 0, false},
		{"member team", []Row{teamRepo}, nil, []string{"team-a"}, 1, false},
		{"non-member team", []Row{foreignTeamRepo}, nil, nil, 1, false},
		{"team and person at one placement install once", []Row{teamRepo, personRepo}, nil, []string{"team-a"}, 1, false},
		{"one row per placement", []Row{orgUser, teamRepo, foreignTeamRepo}, nil, []string{"org-ug", "team-a"}, 1, false},
		{"teams looked up once", []Row{teamRepo, foreignTeamRepo}, nil, []string{"team-a"}, 1, false},
		{"team lookup error fails", []Row{teamRepo}, errors.New("directory down"), nil, 1, true},
		{"no team rows, lookup error never surfaces", []Row{orgUser}, errors.New("directory down"), []string{"org-ug"}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			got, err := MatchPerson(tt.rows, "me", func() (map[string]bool, error) {
				calls++
				return myTeams, tt.teamsErr
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			var ids []string
			for _, r := range got {
				ids = append(ids, r.ID)
			}
			if !reflect.DeepEqual(ids, tt.wantIDs) {
				t.Fatalf("matched %v, want %v", ids, tt.wantIDs)
			}
			if calls != tt.wantCalls {
				t.Fatalf("teamsOf called %d times, want %d", calls, tt.wantCalls)
			}
		})
	}
}

type fakeDirectory struct {
	persons    []string
	personsErr error
	teams      map[string][]string
	teamErr    error
	orgCalls   int
}

func (f *fakeDirectory) ActivePersons(context.Context) ([]string, error) {
	f.orgCalls++
	return f.persons, f.personsErr
}

func (f *fakeDirectory) TeamMembers(_ context.Context, teamID string) ([]string, error) {
	if f.teamErr != nil {
		return nil, f.teamErr
	}
	m, ok := f.teams[teamID]
	if !ok {
		return nil, ErrTeamNotFound
	}
	return m, nil
}

func TestExpand(t *testing.T) {
	dir := func() *fakeDirectory {
		return &fakeDirectory{
			persons: []string{"a", "b", "c"},
			teams:   map[string][]string{"t1": {"b", "d"}, "t2": {"d", "e"}},
		}
	}
	tests := []struct {
		name         string
		rows         []Row
		mutate       func(*fakeDirectory)
		want         []string
		wantErr      bool
		wantOrgCalls int
	}{
		{"person rows", []Row{{PersonID: "x"}, {PersonID: "y"}}, nil, []string{"x", "y"}, false, 0},
		{"org-wide expands to active persons", []Row{{}}, nil, []string{"a", "b", "c"}, false, 1},
		{"team expands to members", []Row{{TeamID: "t1"}}, nil, []string{"b", "d"}, false, 0},
		{"overlapping teams dedupe", []Row{{TeamID: "t1"}, {TeamID: "t2"}}, nil, []string{"b", "d", "e"}, false, 0},
		{"person also in a team appears once", []Row{{PersonID: "d"}, {TeamID: "t1"}}, nil, []string{"d", "b"}, false, 0},
		{"disbanded team has no members", []Row{{TeamID: "gone"}, {PersonID: "x"}}, nil, []string{"x"}, false, 0},
		{"two org-wide rows list persons once", []Row{{Scope: userGlobal}, {Scope: repoLocal, Repository: "o/a"}}, nil, []string{"a", "b", "c"}, false, 1},
		{"empty person id ignored", []Row{{PersonID: ""}, {PersonID: "x"}}, func(f *fakeDirectory) { f.persons = []string{""} }, []string{"x"}, false, 1},
		{"team lookup error fails", []Row{{TeamID: "t1"}}, func(f *fakeDirectory) { f.teamErr = errors.New("down") }, nil, true, 0},
		{"roster error fails", []Row{{}}, func(f *fakeDirectory) { f.personsErr = errors.New("down") }, nil, true, 1},
		{"no rows", nil, nil, nil, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := dir()
			if tt.mutate != nil {
				tt.mutate(d)
			}
			got, err := Expand(context.Background(), tt.rows, d)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Expand = %v, want %v", got, tt.want)
			}
			if d.orgCalls != tt.wantOrgCalls {
				t.Fatalf("ActivePersons called %d times, want %d", d.orgCalls, tt.wantOrgCalls)
			}
		})
	}
}
