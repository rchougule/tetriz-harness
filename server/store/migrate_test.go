package store_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
	"github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1/harnessctlv1connect"
	"github.com/tetriz-ai/tetriz-harness/server"
	"github.com/tetriz-ai/tetriz-harness/server/blob"
	"github.com/tetriz-ai/tetriz-harness/server/store"
)

func testDatabase(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL is not set; skipping the Postgres migration test")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to HARNESS_TEST_DATABASE_URL: %v", err)
	}
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "hcp_mig_" + hex.EncodeToString(suffix)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating test database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(context.Background())
	})
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing dsn: %v", err)
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
}

// uuidDir is a host directory whose ids are the uuids the old built-in
// directory handed out, as a host importing that data would keep them.
type uuidDir struct {
	persons []server.Person
	teams   []server.Team
	members map[string][]string
}

func (d uuidDir) Persons(context.Context) ([]server.Person, error) { return d.persons, nil }
func (d uuidDir) Teams(context.Context) ([]server.Team, error)     { return d.teams, nil }
func (d uuidDir) TeamMembers(_ context.Context, id string) ([]string, error) {
	return d.members[id], nil
}
func (d uuidDir) PersonTeams(_ context.Context, pid string) ([]string, error) {
	var out []string
	for tid, ms := range d.members {
		for _, m := range ms {
			if m == pid {
				out = append(out, tid)
			}
		}
	}
	return out, nil
}

func TestMigrateHostIDsFrom0003(t *testing.T) {
	dsn := testDatabase(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.MigrateTo(ctx, pool, 3); err != nil {
		t.Fatalf("migrating to 0003: %v", err)
	}

	ruleHash, agentHash := blob.Hash([]byte("rule")), blob.Hash([]byte("agent"))
	var alice, bob, team, rule, agent string
	seed := []struct {
		dst *string
		sql string
	}{
		{&alice, `INSERT INTO persons (display_name) VALUES ('alice') RETURNING id::text`},
		{&bob, `INSERT INTO persons (display_name) VALUES ('bob') RETURNING id::text`},
		{&team, `INSERT INTO teams (name) VALUES ('platform') RETURNING id::text`},
	}
	for _, s := range seed {
		if err := pool.QueryRow(ctx, s.sql).Scan(s.dst); err != nil {
			t.Fatalf("%s: %v", s.sql, err)
		}
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`INSERT INTO team_members (team_id, person_id) VALUES ($1, $2)`, team, alice)
	exec(`INSERT INTO device_tokens (person_id, label, token_hash) VALUES ($1, 'laptop', '\x00')`, alice)
	exec(`INSERT INTO blobs (content_hash, size_bytes) VALUES ($1, 4), ($2, 5)`, ruleHash, agentHash)
	artifact := func(typ, slug, hash string) string {
		var id string
		if err := pool.QueryRow(ctx, `INSERT INTO org_artifacts (ide_type, type, slug, name, primary_hash, enforce_until)
			VALUES ('IDE_TYPE_CLAUDE_CODE', $1, $2, $2, $3, now() + interval '14 days') RETURNING id::text`, typ, slug, hash).Scan(&id); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO artifact_files (artifact_id, relative_path, content_hash, is_primary) VALUES ($1, '', $2, true)`, id, hash)
		return id
	}
	rule = artifact("ARTIFACT_TYPE_RULE", "style", ruleHash)
	agent = artifact("ARTIFACT_TYPE_AGENT", "reviewer", agentHash)
	exec(`INSERT INTO enforced_artifacts (artifact_id, type, scope, team_id) VALUES ($1, 'ARTIFACT_TYPE_RULE', 'ARTIFACT_SCOPE_USER_GLOBAL', $2)`, rule, team)
	exec(`INSERT INTO enforced_artifacts (artifact_id, type, scope, person_id) VALUES ($1, 'ARTIFACT_TYPE_AGENT', 'ARTIFACT_SCOPE_USER_GLOBAL', $2)`, agent, bob)
	for _, p := range []string{alice, bob} {
		exec(`INSERT INTO agent_reports (person_id, device_id, agent_version, known_repositories, reported_at) VALUES ($1, 'd1', '1', '{}', now())`, p)
	}
	exec(`INSERT INTO report_installed (person_id, ide_type, repository, source_path, content_hash) VALUES ($1, 'IDE_TYPE_CLAUDE_CODE', '', '.claude/rules/style.md', $2)`, alice, ruleHash)
	exec(`INSERT INTO report_apply_failures (person_id, artifact_id, repository, content_hash, reason) VALUES ($1, $2, '', $3, 'denied')`, bob, agent, agentHash)

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("applying 0004: %v", err)
	}

	for _, c := range []struct{ table, column string }{
		{"enforced_artifacts", "person_id"}, {"enforced_artifacts", "team_id"}, {"agent_reports", "person_id"},
		{"report_installed", "person_id"}, {"report_apply_failures", "person_id"},
	} {
		var typ string
		if err := pool.QueryRow(ctx, `SELECT data_type FROM information_schema.columns WHERE table_name = $1 AND column_name = $2`, c.table, c.column).Scan(&typ); err != nil {
			t.Fatal(err)
		}
		if typ != "text" {
			t.Errorf("%s.%s is %s, want text", c.table, c.column, typ)
		}
	}
	for _, tbl := range []string{"persons", "teams", "team_members", "device_tokens"} {
		var reg *string
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, tbl).Scan(&reg); err != nil {
			t.Fatal(err)
		}
		if reg != nil {
			t.Errorf("table %s survived the migration", tbl)
		}
	}
	var gotTeam, gotPerson string
	if err := pool.QueryRow(ctx, `SELECT team_id FROM enforced_artifacts WHERE artifact_id = $1`, rule).Scan(&gotTeam); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT person_id FROM enforced_artifacts WHERE artifact_id = $1`, agent).Scan(&gotPerson); err != nil {
		t.Fatal(err)
	}
	if gotTeam != team || gotPerson != bob {
		t.Fatalf("target ids = %q %q, want %q %q", gotTeam, gotPerson, team, bob)
	}

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := uuidDir{
		persons: []server.Person{{ID: alice, DisplayName: "alice"}, {ID: bob, DisplayName: "bob"}},
		teams:   []server.Team{{ID: team, Name: "platform"}},
		members: map[string][]string{team: {alice}},
	}
	h, err := server.New(server.Options{Store: st, Directory: dir, Blobs: blobs})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	admin := harnessctlv1connect.NewAdminServiceClient(srv.Client(), srv.URL)
	for _, c := range []struct {
		artifact, person string
		want             v1.EnforcementStatus
	}{
		{rule, alice, v1.EnforcementStatus_ENFORCEMENT_STATUS_INSTALLED},
		{agent, bob, v1.EnforcementStatus_ENFORCEMENT_STATUS_FAILED},
	} {
		resp, err := admin.GetEnforcementStatus(ctx, connect.NewRequest(&v1.GetEnforcementStatusRequest{ArtifactId: c.artifact}))
		if err != nil {
			t.Fatal(err)
		}
		sts := resp.Msg.GetStatuses()
		if len(sts) != 1 || sts[0].GetPersonId() != c.person || sts[0].GetStatus() != c.want {
			t.Fatalf("status of %s = %v, want one %v row for %s", c.artifact, sts, c.want, c.person)
		}
	}
}
