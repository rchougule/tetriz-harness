package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"connectrpc.com/connect"

	"github.com/tetriz-ai/tetriz-harness/server"
)

// directoryFile is the reference host's whole record of people, teams and
// credentials, read once at start. A token is either the secret itself or
// "sha256:" plus the lowercase hex digest of it; only the digest form keeps
// the secret out of the file.
type directoryFile struct {
	AdminToken string `json:"admin_token"`
	Persons    []struct {
		ID           string   `json:"id"`
		DisplayName  string   `json:"display_name"`
		DeviceTokens []string `json:"device_tokens"`
	} `json:"persons"`
	Teams []struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Members []string `json:"members"`
	} `json:"teams"`
}

// fileDirectory serves a directoryFile as the server's Directory and
// authenticates requests against its tokens.
type fileDirectory struct {
	persons     []server.Person
	teams       []server.Team
	members     map[string][]string
	teamsOf     map[string][]string
	adminDigest [sha256.Size]byte
	// deviceOwner maps a device token's digest to its person.
	deviceOwner map[[sha256.Size]byte]string
}

var _ server.Directory = (*fileDirectory)(nil)

const digestPrefix = "sha256:"

func tokenDigest(field, tok string) ([sha256.Size]byte, error) {
	var d [sha256.Size]byte
	if hexDigest, ok := strings.CutPrefix(tok, digestPrefix); ok {
		b, err := hex.DecodeString(hexDigest)
		if err != nil || len(b) != sha256.Size || hexDigest != strings.ToLower(hexDigest) {
			return d, fmt.Errorf("%s: %q is not sha256: and 64 lowercase hex digits", field, tok)
		}
		copy(d[:], b)
		return d, nil
	}
	if strings.TrimSpace(tok) == "" {
		return d, fmt.Errorf("%s is empty", field)
	}
	return sha256.Sum256([]byte(tok)), nil
}

func validID(id string) bool { return id != "" && len(id) <= server.MaxIDLen }

func loadDirectory(path string) (*fileDirectory, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading directory file: %w", err)
	}
	defer func() { _ = fh.Close() }()
	f, err := parseDirectory(fh)
	if err != nil {
		return nil, fmt.Errorf("parsing directory file %s: %w", path, err)
	}
	d, err := buildDirectory(f)
	if err != nil {
		return nil, fmt.Errorf("directory file %s: %w", path, err)
	}
	return d, nil
}

func parseDirectory(r io.Reader) (directoryFile, error) {
	var f directoryFile
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return directoryFile{}, fmt.Errorf("decoding: %w", err)
	}
	return f, nil
}

func buildDirectory(f directoryFile) (*fileDirectory, error) {
	d := &fileDirectory{
		members:     map[string][]string{},
		teamsOf:     map[string][]string{},
		deviceOwner: map[[sha256.Size]byte]string{},
	}
	var err error
	if d.adminDigest, err = tokenDigest("admin_token", f.AdminToken); err != nil {
		return nil, err
	}
	var errs []error
	persons := map[string]bool{}
	for i, p := range f.Persons {
		if !validID(p.ID) || persons[p.ID] {
			errs = append(errs, fmt.Errorf("persons[%d]: id %q is empty, longer than %d bytes or repeated", i, p.ID, server.MaxIDLen))
			continue
		}
		persons[p.ID] = true
		name := p.DisplayName
		if name == "" {
			name = p.ID
		}
		d.persons = append(d.persons, server.Person{ID: p.ID, DisplayName: name})
		for j, tok := range p.DeviceTokens {
			dg, err := tokenDigest(fmt.Sprintf("persons[%d].device_tokens[%d]", i, j), tok)
			switch {
			case err != nil:
				errs = append(errs, err)
			case dg == d.adminDigest:
				errs = append(errs, fmt.Errorf("persons[%d].device_tokens[%d] equals the admin token", i, j))
			case d.deviceOwner[dg] != "":
				errs = append(errs, fmt.Errorf("persons[%d].device_tokens[%d] is already another device's token", i, j))
			default:
				d.deviceOwner[dg] = p.ID
			}
		}
	}
	teams := map[string]bool{}
	for i, t := range f.Teams {
		if !validID(t.ID) || teams[t.ID] {
			errs = append(errs, fmt.Errorf("teams[%d]: id %q is empty, longer than %d bytes or repeated", i, t.ID, server.MaxIDLen))
			continue
		}
		teams[t.ID] = true
		name := t.Name
		if name == "" {
			name = t.ID
		}
		d.teams = append(d.teams, server.Team{ID: t.ID, Name: name})
		seen := map[string]bool{}
		for _, m := range t.Members {
			if !persons[m] {
				errs = append(errs, fmt.Errorf("teams[%d]: member %q is not a listed person", i, m))
				continue
			}
			if seen[m] {
				continue
			}
			seen[m] = true
			d.members[t.ID] = append(d.members[t.ID], m)
			d.teamsOf[m] = append(d.teamsOf[m], t.ID)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *fileDirectory) Persons(context.Context) ([]server.Person, error) { return d.persons, nil }

func (d *fileDirectory) Teams(context.Context) ([]server.Team, error) { return d.teams, nil }

func (d *fileDirectory) TeamMembers(_ context.Context, teamID string) ([]string, error) {
	return d.members[teamID], nil
}

func (d *fileDirectory) PersonTeams(_ context.Context, personID string) ([]string, error) {
	return d.teamsOf[personID], nil
}

func bearer(h http.Header) (string, bool) {
	v := h.Get("Authorization")
	const prefix = "Bearer "
	if len(v) <= len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(v[len(prefix):])
	return tok, tok != ""
}

var errUnauthenticated = errors.New("missing or invalid bearer token")

// authenticate admits an AdminService call only with the admin token, and an
// AgentService call only with a listed device token, which it maps to that
// token's person. The health check and the UI's static files pass through.
func (d *fileDirectory) authenticate(next http.Handler) http.Handler {
	errw := connect.NewErrorWriter()
	refuse := func(w http.ResponseWriter, r *http.Request) {
		_ = errw.Write(w, r, connect.NewError(connect.CodeUnauthenticated, errUnauthenticated))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, server.AdminPathPrefix):
			tok, ok := bearer(r.Header)
			dg := sha256.Sum256([]byte(tok))
			if !ok || subtle.ConstantTimeCompare(dg[:], d.adminDigest[:]) != 1 {
				refuse(w, r)
				return
			}
		case strings.HasPrefix(r.URL.Path, server.AgentPathPrefix):
			tok, ok := bearer(r.Header)
			person := d.deviceOwner[sha256.Sum256([]byte(tok))]
			if !ok || person == "" {
				refuse(w, r)
				return
			}
			r = r.WithContext(server.WithPerson(r.Context(), person))
		}
		next.ServeHTTP(w, r)
	})
}
