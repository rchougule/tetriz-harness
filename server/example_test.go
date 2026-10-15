package server_test

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/tetriz-ai/tetriz-harness/server"
	"github.com/tetriz-ai/tetriz-harness/server/blob"
	"github.com/tetriz-ai/tetriz-harness/server/store"
)

// staticDir is a host directory held in memory.
type staticDir struct{}

func (staticDir) Persons(context.Context) ([]server.Person, error) {
	return []server.Person{{ID: "dev-1", DisplayName: "Dev One"}}, nil
}

func (staticDir) Teams(context.Context) ([]server.Team, error) {
	return []server.Team{{ID: "platform", Name: "Platform"}}, nil
}

func (staticDir) TeamMembers(_ context.Context, teamID string) ([]string, error) {
	if teamID == "platform" {
		return []string{"dev-1"}, nil
	}
	return nil, nil
}

func (staticDir) PersonTeams(_ context.Context, personID string) ([]string, error) {
	if personID == "dev-1" {
		return []string{"platform"}, nil
	}
	return nil, nil
}

func ExampleNew() {
	ctx := context.Background()
	st, err := store.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	blobs, err := blob.NewFS("/var/lib/harness/blobs")
	if err != nil {
		log.Fatal(err)
	}
	h, err := server.New(server.Options{Store: st, Directory: staticDir{}, Blobs: blobs})
	if err != nil {
		log.Fatal(err)
	}

	// The handler authenticates nothing, so the host checks every request:
	// here admins by one token, agents by a device token mapped to a person.
	devices := map[string]string{"Bearer dev-1-device-token": "dev-1"}
	authn := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch {
		case strings.HasPrefix(r.URL.Path, server.AdminPathPrefix):
			if auth != "Bearer admin-token" {
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
		case strings.HasPrefix(r.URL.Path, server.AgentPathPrefix):
			person, ok := devices[auth]
			if !ok {
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			r = r.WithContext(server.WithPerson(r.Context(), person))
		}
		h.ServeHTTP(w, r)
	})

	mux := http.NewServeMux()
	mux.Handle("/", authn)
	log.Fatal(http.ListenAndServe(":8080", mux))
}
