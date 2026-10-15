package service

import (
	"context"
	"errors"
	"fmt"
)

// MaxIDLen bounds a person or team id a target may name.
const MaxIDLen = 200

var (
	errPersonNotFound = errors.New("person not found")
	errTeamNotFound   = errors.New("team not found")
	errInvalidHostID  = fmt.Errorf("person and team ids must be 1-%d bytes", MaxIDLen)
)

// Directory is the host's record of people and teams, which the server reads
// and never writes. Ids are opaque non-empty strings of at most MaxIDLen
// bytes; the server stores them in targets and reports as given, so an id
// must keep naming the same person or team for as long as those rows exist.
//
// The server reads the directory on every grading pass and every agent call,
// so an implementation should answer from memory or a cache. Any error it
// returns fails the RPC as Internal.
type Directory interface {
	// Persons returns every person, in the order listings show them. A person
	// absent from it drops out of every status, whatever targets name them.
	Persons(ctx context.Context) ([]Person, error)
	// Teams returns every team, in the order listings show them.
	Teams(ctx context.Context) ([]Team, error)
	// TeamMembers returns the ids of a team's members; an unknown team has none.
	TeamMembers(ctx context.Context, teamID string) ([]string, error)
	// PersonTeams returns the ids of the teams a person belongs to.
	PersonTeams(ctx context.Context, personID string) ([]string, error)
}

// Person is one entry of a Directory.
type Person struct {
	ID          string
	DisplayName string
}

// Team is one entry of a Directory; its members come from TeamMembers.
type Team struct {
	ID   string
	Name string
}

func validateHostID(id string) error {
	if id == "" || len(id) > MaxIDLen {
		return invalid(fmt.Errorf("%q: %w", id, errInvalidHostID))
	}
	return nil
}

// requireKnown refuses ids the directory does not hold, so a target never
// names someone the host has not told the server about.
func requireKnown(ctx context.Context, dir Directory, personIDs, teamIDs []string) error {
	if len(personIDs) > 0 {
		persons, err := dir.Persons(ctx)
		if err != nil {
			return fmt.Errorf("listing persons: %w", err)
		}
		known := make(map[string]bool, len(persons))
		for _, p := range persons {
			known[p.ID] = true
		}
		for _, id := range personIDs {
			if !known[id] {
				return notFound(fmt.Errorf("%s: %w", id, errPersonNotFound))
			}
		}
	}
	if len(teamIDs) > 0 {
		teams, err := dir.Teams(ctx)
		if err != nil {
			return fmt.Errorf("listing teams: %w", err)
		}
		known := make(map[string]bool, len(teams))
		for _, t := range teams {
			known[t.ID] = true
		}
		for _, id := range teamIDs {
			if !known[id] {
				return notFound(fmt.Errorf("%s: %w", id, errTeamNotFound))
			}
		}
	}
	return nil
}
