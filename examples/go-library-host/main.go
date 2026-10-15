// Command go-library-host is the smallest host for the control plane library:
// a fixed directory of one person in one team, a Postgres store, a blob store in
// ./blobs, and a middleware that authenticates each call with a bearer token.
// docs/go-library.md walks through it. Run it from the repository root:
//
//	DATABASE_URL=postgres://... ADMIN_TOKEN=example-admin-token go run ./examples/go-library-host
package main

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

// directory is the host's people and teams; a real host reads its own records.
type directory struct{}

func (directory) Persons(context.Context) ([]server.Person, error) {
	return []server.Person{{ID: "dev-1", DisplayName: "Dev One"}}, nil
}
func (directory) Teams(context.Context) ([]server.Team, error) {
	return []server.Team{{ID: "platform", Name: "Platform"}}, nil
}
func (directory) TeamMembers(_ context.Context, team string) ([]string, error) {
	return map[string][]string{"platform": {"dev-1"}}[team], nil
}
func (directory) PersonTeams(_ context.Context, person string) ([]string, error) {
	return map[string][]string{"dev-1": {"platform"}}[person], nil
}

func main() {
	ctx := context.Background()
	st, err := store.Open(ctx, os.Getenv("DATABASE_URL")) // migrates on open
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	blobs, err := blob.NewFS("./blobs")
	if err != nil {
		log.Fatal(err)
	}
	h, err := server.New(server.Options{Store: st, Directory: directory{}, Blobs: blobs})
	if err != nil {
		log.Fatal(err)
	}

	// The handler authenticates nothing: this middleware is the only guard.
	admin := "Bearer " + os.Getenv("ADMIN_TOKEN")
	devices := map[string]string{"Bearer dev-1-device-token": "dev-1"}
	host := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch {
		case strings.HasPrefix(r.URL.Path, server.AdminPathPrefix):
			if auth != admin || admin == "Bearer " {
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
	log.Fatal(http.ListenAndServe("127.0.0.1:8090", host))
}
