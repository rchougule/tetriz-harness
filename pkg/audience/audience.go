// Package audience resolves who an enforced artifact reaches. Targets are
// stored as selectors (org-wide, a person, a team) and expanded at read time,
// so a person who joins a team or the org later is reached without a rewrite.
package audience

import (
	"context"
	"errors"
	"fmt"
	"sort"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
)

var (
	// ErrInvalid is returned when more than one selector is set.
	ErrInvalid = errors.New("audience must be exactly one of org-wide, persons, or teams")
	// ErrTeamNotFound is what a Directory returns for a team that no longer exists;
	// Expand treats it as a team with no members.
	ErrTeamNotFound = errors.New("team not found")
)

// Audience is a validated selector: exactly one of OrgWide, PersonIDs or
// TeamIDs, or none at all (unspecified).
type Audience struct {
	OrgWide   bool
	PersonIDs []string
	TeamIDs   []string
}

// Specified reports whether any selector is set.
func (a Audience) Specified() bool {
	return a.OrgWide || len(a.PersonIDs) > 0 || len(a.TeamIDs) > 0
}

// Resolve validates the mutually exclusive selectors. An unspecified audience
// is valid here; whether the caller accepts one is the caller's rule.
func Resolve(orgWide bool, personIDs, teamIDs []string) (Audience, error) {
	set := 0
	if orgWide {
		set++
	}
	if len(personIDs) > 0 {
		set++
	}
	if len(teamIDs) > 0 {
		set++
	}
	if set > 1 {
		return Audience{}, ErrInvalid
	}
	return Audience{OrgWide: orgWide, PersonIDs: personIDs, TeamIDs: teamIDs}, nil
}

// Row is one routing row: a placement plus one selector. Empty PersonID and
// TeamID together mean org-wide; empty Repository means user-global. ID is the
// row's storage identity and plays no part in any comparison here.
type Row struct {
	ID         string
	Scope      v1.ArtifactScope
	Repository string
	PersonID   string
	TeamID     string
}

// OrgWide reports whether the row targets the whole org.
func (r Row) OrgWide() bool {
	return r.PersonID == "" && r.TeamID == ""
}

// Rows materialises an audience at one placement: one org-wide row, or one
// row per person, or one per team. An unspecified audience yields none.
func (a Audience) Rows(scope v1.ArtifactScope, repository string) []Row {
	switch {
	case a.OrgWide:
		return []Row{{Scope: scope, Repository: repository}}
	case len(a.PersonIDs) > 0:
		rows := make([]Row, 0, len(a.PersonIDs))
		for _, p := range a.PersonIDs {
			rows = append(rows, Row{Scope: scope, Repository: repository, PersonID: p})
		}
		return rows
	default:
		rows := make([]Row, 0, len(a.TeamIDs))
		for _, tm := range a.TeamIDs {
			rows = append(rows, Row{Scope: scope, Repository: repository, TeamID: tm})
		}
		return rows
	}
}

// TargetKeyEqual reports whether two rows occupy the same placement and
// selector slot, with unset equal to unset.
func TargetKeyEqual(a, b Row) bool {
	return a.Scope == b.Scope && a.Repository == b.Repository && a.PersonID == b.PersonID && a.TeamID == b.TeamID
}

// PlacementKey identifies a row's (scope, repository); it sorts USER_GLOBAL
// before REPO_LOCAL and repositories lexically.
func PlacementKey(scope v1.ArtifactScope, repository string) string {
	return fmt.Sprintf("%d|%s", scope, repository)
}

// Placement is the rows routed to one (scope, repository).
type Placement struct {
	Scope      v1.ArtifactScope
	Repository string
	Rows       []Row
}

// Audience folds a placement's rows back into its selector.
func (p Placement) Audience() Audience {
	var a Audience
	for _, r := range p.Rows {
		switch {
		case r.PersonID != "":
			a.PersonIDs = append(a.PersonIDs, r.PersonID)
		case r.TeamID != "":
			a.TeamIDs = append(a.TeamIDs, r.TeamID)
		default:
			a.OrgWide = true
		}
	}
	return a
}

// GroupByPlacement partitions rows by placement in PlacementKey order, keeping
// row order within each, so a response built from it is stable.
func GroupByPlacement(rows []Row) []Placement {
	byKey := make(map[string]*Placement, len(rows))
	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		k := PlacementKey(r.Scope, r.Repository)
		p, ok := byKey[k]
		if !ok {
			p = &Placement{Scope: r.Scope, Repository: r.Repository}
			byKey[k] = p
			keys = append(keys, k)
		}
		p.Rows = append(p.Rows, r)
	}
	sort.Strings(keys)
	out := make([]Placement, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byKey[k])
	}
	return out
}

// Prune returns the live rows a targets edit removes: rows at the edited
// placement that the desired set no longer names. Rows at other placements
// were not part of the edit and are never returned.
func Prune(live, desired []Row, scope v1.ArtifactScope, repository string) []Row {
	editKey := PlacementKey(scope, repository)
	var out []Row
	for _, r := range live {
		if PlacementKey(r.Scope, r.Repository) != editKey {
			continue
		}
		kept := false
		for _, d := range desired {
			if TargetKeyEqual(r, d) {
				kept = true
				break
			}
		}
		if !kept {
			out = append(out, r)
		}
	}
	return out
}

// Collides reports whether any desired row lands on a slot an existing row
// already holds. Callers use it to keep two root-doc definitions of one type
// off the same target, since both would write the same file.
func Collides(existing, desired []Row) bool {
	for _, e := range existing {
		for _, d := range desired {
			if TargetKeyEqual(e, d) {
				return true
			}
		}
	}
	return false
}

// MatchPerson returns the rows of one definition that reach personID, one per
// placement (the first matching row wins). teamsOf is called at most once,
// and only when a team row has to be tested.
func MatchPerson(rows []Row, personID string, teamsOf func() (map[string]bool, error)) ([]Row, error) {
	var teams map[string]bool
	loaded := false
	seen := make(map[string]bool, len(rows))
	var out []Row
	for _, r := range rows {
		match := false
		switch {
		case r.OrgWide():
			match = true
		case r.PersonID != "":
			match = r.PersonID == personID
		default:
			if !loaded {
				t, err := teamsOf()
				if err != nil {
					return nil, fmt.Errorf("resolving the person's teams: %w", err)
				}
				teams, loaded = t, true
			}
			match = teams[r.TeamID]
		}
		if !match {
			continue
		}
		k := PlacementKey(r.Scope, r.Repository)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
	}
	return out, nil
}

// Directory answers the membership questions Expand needs.
type Directory interface {
	// ActivePersons returns every person an org-wide row reaches.
	ActivePersons(ctx context.Context) ([]string, error)
	// TeamMembers returns a team's members, or ErrTeamNotFound.
	TeamMembers(ctx context.Context, teamID string) ([]string, error)
}

// Expand resolves rows to the distinct persons they reach, in first-seen
// order. A person named directly and via a team, or via two teams, appears once.
func Expand(ctx context.Context, rows []Row, dir Directory) ([]string, error) {
	seen := make(map[string]bool)
	var out []string
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	orgLoaded := false
	for _, r := range rows {
		switch {
		case r.PersonID != "":
			add(r.PersonID)
		case r.TeamID != "":
			members, err := dir.TeamMembers(ctx, r.TeamID)
			if errors.Is(err, ErrTeamNotFound) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("resolving team %s: %w", r.TeamID, err)
			}
			for _, m := range members {
				add(m)
			}
		default:
			if orgLoaded {
				continue
			}
			ids, err := dir.ActivePersons(ctx)
			if err != nil {
				return nil, fmt.Errorf("listing org persons: %w", err)
			}
			orgLoaded = true
			for _, id := range ids {
				add(id)
			}
		}
	}
	return out, nil
}
