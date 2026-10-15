package server_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
	"github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1/harnessctlv1connect"
	"github.com/tetriz-ai/tetriz-harness/server"
	"github.com/tetriz-ai/tetriz-harness/server/blob"
	"github.com/tetriz-ai/tetriz-harness/server/store"
)

var (
	userGlobal = v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL
	repoLocal  = v1.ArtifactScope_ARTIFACT_SCOPE_REPO_LOCAL
	claude     = v1.IdeType_IDE_TYPE_CLAUDE_CODE
)

// testDatabase creates a throwaway database next to the one
// HARNESS_TEST_DATABASE_URL names and drops it after the test.
func testDatabase(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL is not set; skipping the Postgres integration suite (point it at a server where the role may CREATE DATABASE)")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting to HARNESS_TEST_DATABASE_URL: %v", err)
	}
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "hcp_it_" + hex.EncodeToString(suffix)
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

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// hostDir is the test's host: an in-memory directory plus the token map its
// middleware authenticates agent calls against.
type hostDir struct {
	mu      sync.Mutex
	persons []server.Person
	teams   []server.Team
	members map[string][]string
	tokens  map[string]string
}

func (d *hostDir) Persons(context.Context) ([]server.Person, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.persons), nil
}

func (d *hostDir) Teams(context.Context) ([]server.Team, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.teams), nil
}

func (d *hostDir) TeamMembers(_ context.Context, teamID string) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.members[teamID]), nil
}

func (d *hostDir) PersonTeams(_ context.Context, personID string) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, t := range d.teams {
		if slices.Contains(d.members[t.ID], personID) {
			out = append(out, t.ID)
		}
	}
	return out, nil
}

// remove drops a person from the directory and every team, keeping their token
// so a test can show the server, not the host, stops counting them.
func (d *hostDir) remove(personID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.persons = slices.DeleteFunc(d.persons, func(p server.Person) bool { return p.ID == personID })
	for id, ms := range d.members {
		d.members[id] = slices.DeleteFunc(ms, func(m string) bool { return m == personID })
	}
}

// authenticate maps an agent's bearer token to its person; admin calls pass
// untouched, as authenticating them is the host's business.
func (d *hostDir) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, server.AgentPathPrefix) {
			d.mu.Lock()
			pid := d.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
			d.mu.Unlock()
			if pid != "" {
				r = r.WithContext(server.WithPerson(r.Context(), pid))
			}
		}
		next.ServeHTTP(w, r)
	})
}

type env struct {
	t     *testing.T
	st    *store.Store
	dir   *hostDir
	srv   *httptest.Server
	admin harnessctlv1connect.AdminServiceClient
	clock *clock
}

func bearer(token string) connect.Option {
	return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	}))
}

func newEnv(t *testing.T, allowHooks bool) *env {
	t.Helper()
	dsn := testDatabase(t)
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(st.Close)
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{now: time.Now().UTC()}
	dir := &hostDir{members: map[string][]string{}, tokens: map[string]string{}}
	h, err := server.New(server.Options{Store: st, Directory: dir, Blobs: blobs, AllowHooks: allowHooks, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(dir.authenticate(h))
	t.Cleanup(srv.Close)
	return &env{
		t: t, st: st, dir: dir, srv: srv, clock: c,
		admin: harnessctlv1connect.NewAdminServiceClient(srv.Client(), srv.URL),
	}
}

func (e *env) agent(token string) harnessctlv1connect.AgentServiceClient {
	return harnessctlv1connect.NewAgentServiceClient(e.srv.Client(), e.srv.URL, bearer(token))
}

type result[T any] struct {
	resp *connect.Response[T]
	err  error
}

func call[T any](resp *connect.Response[T], err error) result[T] { return result[T]{resp, err} }

func (r result[T]) must(t *testing.T) *T {
	t.Helper()
	if r.err != nil {
		t.Fatalf("rpc failed: %v", r.err)
	}
	return r.resp.Msg
}

// person adds a host person whose id is deliberately not a uuid.
func (e *env) person(name string) (id, token string) {
	id, token = "person:"+name, "token-"+name
	e.dir.mu.Lock()
	defer e.dir.mu.Unlock()
	e.dir.persons = append(e.dir.persons, server.Person{ID: id, DisplayName: name})
	e.dir.tokens[token] = id
	return id, token
}

func (e *env) team(name string, members ...string) string {
	id := "team:" + name
	e.dir.mu.Lock()
	defer e.dir.mu.Unlock()
	e.dir.teams = append(e.dir.teams, server.Team{ID: id, Name: name})
	e.dir.members[id] = members
	return id
}

func (e *env) upsert(req *v1.UpsertArtifactRequest) string {
	return call(e.admin.UpsertArtifact(context.Background(), connect.NewRequest(req))).must(e.t).GetArtifactId()
}

func (e *env) target(id string, scope v1.ArtifactScope, repo string, aud *v1.Audience) {
	call(e.admin.SetTargets(context.Background(), connect.NewRequest(&v1.SetTargetsRequest{ArtifactId: id, Scope: scope, Repository: repo, Audience: aud}))).must(e.t)
}

func (e *env) report(token string, req *v1.ReportStateRequest) {
	call(e.agent(token).ReportState(context.Background(), connect.NewRequest(req))).must(e.t)
}

// statusOf returns the one status row for person at the placement.
func (e *env) statusOf(artifactID, personID string, scope v1.ArtifactScope, repo string) *v1.PersonEnforcementStatus {
	e.t.Helper()
	resp := call(e.admin.GetEnforcementStatus(context.Background(), connect.NewRequest(&v1.GetEnforcementStatusRequest{ArtifactId: artifactID}))).must(e.t)
	for _, s := range resp.GetStatuses() {
		if s.GetPersonId() == personID && s.GetScope() == scope && s.GetRepository() == repo {
			return s
		}
	}
	e.t.Fatalf("no status for person %s at %v %q in %v", personID, scope, repo, resp.GetStatuses())
	return nil
}

func wantStatus(t *testing.T, got *v1.PersonEnforcementStatus, want v1.EnforcementStatus) {
	t.Helper()
	if got.GetStatus() != want {
		t.Fatalf("status = %v (%q), want %v", got.GetStatus(), got.GetDetail(), want)
	}
}

func installed(path, hash string) *v1.InstalledArtifact {
	return &v1.InstalledArtifact{IdeType: claude, SourcePath: path, ContentHash: hash}
}

// TestLifecycle walks one rule through targeting, delivery, convergence, a
// content change, a failed apply and expiry.
func TestLifecycle(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	alice, aliceTok := e.person("alice")

	team := e.team("platform", alice)

	id := e.upsert(&v1.UpsertArtifactRequest{IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_RULE, Slug: "style", Name: "Style", InlineContent: "be terse"})
	e.target(id, userGlobal, "", &v1.Audience{TeamIds: []string{team}})

	set := call(e.agent(aliceTok).GetEnforcedSet(ctx, connect.NewRequest(&v1.GetEnforcedSetRequest{}))).must(t)
	if len(set.GetArtifacts()) != 1 {
		t.Fatalf("enforced set = %v", set.GetArtifacts())
	}
	ea := set.GetArtifacts()[0]
	if ea.GetInstallPath() != ".claude/rules/style.md" || ea.GetPrimaryHash() != blob.Hash([]byte("be terse")) || ea.GetExpired() {
		t.Fatalf("enforced artifact = %v", ea)
	}
	bodies := call(e.agent(aliceTok).GetArtifactBodies(ctx, connect.NewRequest(&v1.GetArtifactBodiesRequest{ContentHashes: []string{ea.GetPrimaryHash()}}))).must(t)
	if len(bodies.GetBodies()) != 1 || string(bodies.GetBodies()[0].GetContent()) != "be terse" {
		t.Fatalf("bodies = %v", bodies.GetBodies())
	}

	e.report(aliceTok, &v1.ReportStateRequest{DeviceId: "d1", Installed: []*v1.InstalledArtifact{installed(ea.GetInstallPath(), ea.GetPrimaryHash())}})
	st := e.statusOf(id, alice, userGlobal, "")
	wantStatus(t, st, v1.EnforcementStatus_ENFORCEMENT_STATUS_INSTALLED)
	if st.GetObservedAt() == nil {
		t.Fatal("INSTALLED without observed_at")
	}

	e.upsert(&v1.UpsertArtifactRequest{ArtifactId: id, IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_RULE, Slug: "style", Name: "Style", InlineContent: "be very terse"})
	wantStatus(t, e.statusOf(id, alice, userGlobal, ""), v1.EnforcementStatus_ENFORCEMENT_STATUS_PENDING)

	newHash := blob.Hash([]byte("be very terse"))
	e.report(aliceTok, &v1.ReportStateRequest{
		DeviceId:      "d1",
		Installed:     []*v1.InstalledArtifact{installed(ea.GetInstallPath(), ea.GetPrimaryHash())},
		ApplyFailures: []*v1.ApplyFailure{{ArtifactId: id, ContentHash: newHash, Reason: "permission denied"}},
	})
	st = e.statusOf(id, alice, userGlobal, "")
	wantStatus(t, st, v1.EnforcementStatus_ENFORCEMENT_STATUS_FAILED)
	if st.GetDetail() != "permission denied" {
		t.Fatalf("detail = %q", st.GetDetail())
	}

	e.clock.Advance(16 * 24 * time.Hour)
	wantStatus(t, e.statusOf(id, alice, userGlobal, ""), v1.EnforcementStatus_ENFORCEMENT_STATUS_EXPIRED)
	set = call(e.agent(aliceTok).GetEnforcedSet(ctx, connect.NewRequest(&v1.GetEnforcedSetRequest{}))).must(t)
	if len(set.GetArtifacts()) != 1 || !set.GetArtifacts()[0].GetExpired() {
		t.Fatalf("expired artifact not flagged: %v", set.GetArtifacts())
	}

	// A save restarts the window.
	e.upsert(&v1.UpsertArtifactRequest{ArtifactId: id, IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_RULE, Slug: "style"})
	wantStatus(t, e.statusOf(id, alice, userGlobal, ""), v1.EnforcementStatus_ENFORCEMENT_STATUS_FAILED)
}

func TestRetraction(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	gone, goneTok := e.person("gone")
	kept, keptTok := e.person("kept")
	id := e.upsert(&v1.UpsertArtifactRequest{IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_AGENT, Slug: "reviewer", InlineContent: "review"})
	e.target(id, userGlobal, "", &v1.Audience{OrgWide: true})
	path, hash := ".claude/agents/reviewer.md", blob.Hash([]byte("review"))
	e.report(goneTok, &v1.ReportStateRequest{Installed: []*v1.InstalledArtifact{installed(path, hash)}})
	e.report(keptTok, &v1.ReportStateRequest{Installed: []*v1.InstalledArtifact{installed(path, hash)}})

	call(e.admin.RemoveArtifact(ctx, connect.NewRequest(&v1.RemoveArtifactRequest{ArtifactId: id}))).must(t)
	e.report(goneTok, &v1.ReportStateRequest{})

	wantStatus(t, e.statusOf(id, gone, userGlobal, ""), v1.EnforcementStatus_ENFORCEMENT_STATUS_REMOVED)
	wantStatus(t, e.statusOf(id, kept, userGlobal, ""), v1.EnforcementStatus_ENFORCEMENT_STATUS_REMOVAL_PENDING)

	// An agent that leaves a locally modified file in place says why.
	why := "left in place: modified since installed: " + path
	e.report(keptTok, &v1.ReportStateRequest{
		Installed:     []*v1.InstalledArtifact{installed(path, blob.Hash([]byte("review, edited")))},
		ApplyFailures: []*v1.ApplyFailure{{ArtifactId: id, Reason: why}},
	})
	st := e.statusOf(id, kept, userGlobal, "")
	wantStatus(t, st, v1.EnforcementStatus_ENFORCEMENT_STATUS_REMOVAL_PENDING)
	if st.GetDetail() != why {
		t.Fatalf("removal detail = %q, want %q", st.GetDetail(), why)
	}

	list := call(e.admin.ListArtifacts(ctx, connect.NewRequest(&v1.ListArtifactsRequest{}))).must(t)
	if len(list.GetArtifacts()) != 0 {
		t.Fatalf("retracted artifact still listed: %v", list.GetArtifacts())
	}
	set := call(e.agent(keptTok).GetEnforcedSet(ctx, connect.NewRequest(&v1.GetEnforcedSetRequest{}))).must(t)
	if len(set.GetArtifacts()) != 0 {
		t.Fatalf("retracted artifact still enforced: %v", set.GetArtifacts())
	}
	// The slug is free again, and a retracted id cannot be edited.
	e.upsert(&v1.UpsertArtifactRequest{IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_AGENT, Slug: "reviewer", InlineContent: "review v2"})
	_, err := e.admin.UpsertArtifact(ctx, connect.NewRequest(&v1.UpsertArtifactRequest{ArtifactId: id, IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_AGENT, Slug: "reviewer"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("editing a retracted artifact: %v", err)
	}
}

func TestExclusion(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	silent, _ := e.person("silent")
	elsewhere, elsewhereTok := e.person("elsewhere")
	checkout, checkoutTok := e.person("checkout")
	id := e.upsert(&v1.UpsertArtifactRequest{IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_CLAUDE_MD, Slug: "root", InlineContent: "# repo"})
	e.target(id, repoLocal, "acme/app", &v1.Audience{OrgWide: true})
	e.report(elsewhereTok, &v1.ReportStateRequest{KnownRepositories: []string{"acme/other"}})
	e.report(checkoutTok, &v1.ReportStateRequest{
		KnownRepositories: []string{"acme/app"},
		Installed:         []*v1.InstalledArtifact{{IdeType: claude, Repository: "acme/app", SourcePath: "CLAUDE.md", ContentHash: blob.Hash([]byte("# repo"))}},
	})

	st := e.statusOf(id, silent, repoLocal, "acme/app")
	wantStatus(t, st, v1.EnforcementStatus_ENFORCEMENT_STATUS_EXCLUDED)
	st = e.statusOf(id, elsewhere, repoLocal, "acme/app")
	wantStatus(t, st, v1.EnforcementStatus_ENFORCEMENT_STATUS_EXCLUDED)
	if st.GetDetail() == "" {
		t.Fatal("EXCLUDED without a reason")
	}
	wantStatus(t, e.statusOf(id, checkout, repoLocal, "acme/app"), v1.EnforcementStatus_ENFORCEMENT_STATUS_INSTALLED)

	elig := call(e.admin.ListEligibleEngineers(ctx, connect.NewRequest(&v1.ListEligibleEngineersRequest{Repository: "acme/app"}))).must(t)
	eligible := map[string]bool{}
	for _, en := range elig.GetEngineers() {
		eligible[en.GetPersonId()] = en.GetEligible()
	}
	if eligible[silent] || eligible[elsewhere] || !eligible[checkout] {
		t.Fatalf("eligibility = %v", eligible)
	}

	// A second CLAUDE.md on the same target collides on disk and is refused.
	other := e.upsert(&v1.UpsertArtifactRequest{IdeType: v1.IdeType_IDE_TYPE_CURSOR, Type: v1.ArtifactType_ARTIFACT_TYPE_CLAUDE_MD, Slug: "root", InlineContent: "# other"})
	_, err := e.admin.SetTargets(ctx, connect.NewRequest(&v1.SetTargetsRequest{ArtifactId: other, Scope: repoLocal, Repository: "acme/app", Audience: &v1.Audience{OrgWide: true}}))
	if connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("root-doc collision: %v", err)
	}
	files := call(e.admin.GetArtifactFiles(ctx, connect.NewRequest(&v1.GetArtifactFilesRequest{ArtifactId: id}))).must(t)
	if len(files.GetFiles()) != 1 || files.GetFiles()[0].GetRelativePath() != "CLAUDE.md" || files.GetRepositories()[0] != "acme/app" {
		t.Fatalf("files = %v", files)
	}
}

// TestRepositoryCase matches repository names case-insensitively: targets,
// reports and eligibility queries are all normalised to lowercase.
func TestRepositoryCase(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	dev, tok := e.person("dev")
	id := e.upsert(&v1.UpsertArtifactRequest{IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_CLAUDE_MD, Slug: "root", InlineContent: "# repo"})
	e.target(id, repoLocal, "Acme/App", &v1.Audience{OrgWide: true})

	set := call(e.agent(tok).GetEnforcedSet(ctx, connect.NewRequest(&v1.GetEnforcedSetRequest{}))).must(t)
	if len(set.GetArtifacts()) != 1 || set.GetArtifacts()[0].GetRepository() != "acme/app" {
		t.Fatalf("enforced set = %v", set.GetArtifacts())
	}
	e.report(tok, &v1.ReportStateRequest{
		KnownRepositories: []string{"ACME/app"},
		Installed:         []*v1.InstalledArtifact{{IdeType: claude, Repository: "acme/APP", SourcePath: "CLAUDE.md", ContentHash: blob.Hash([]byte("# repo"))}},
	})
	wantStatus(t, e.statusOf(id, dev, repoLocal, "acme/app"), v1.EnforcementStatus_ENFORCEMENT_STATUS_INSTALLED)

	elig := call(e.admin.ListEligibleEngineers(ctx, connect.NewRequest(&v1.ListEligibleEngineersRequest{Repository: "Acme/APP"}))).must(t)
	if len(elig.GetEngineers()) != 1 || !elig.GetEngineers()[0].GetEligible() {
		t.Fatalf("eligibility = %v", elig.GetEngineers())
	}

	// Re-targeting under another spelling replaces the same placement.
	e.target(id, repoLocal, "ACME/APP", &v1.Audience{})
	if set := call(e.agent(tok).GetEnforcedSet(ctx, connect.NewRequest(&v1.GetEnforcedSetRequest{}))).must(t); len(set.GetArtifacts()) != 0 {
		t.Fatalf("placement survived an empty audience under another spelling: %v", set.GetArtifacts())
	}
}

// TestCheckouts reports two checkouts of one repository side by side: any
// checkout carrying the enforced hash makes the placement INSTALLED.
func TestCheckouts(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	dev, tok := e.person("dev")
	id := e.upsert(&v1.UpsertArtifactRequest{IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_CLAUDE_MD, Slug: "root", InlineContent: "# repo"})
	e.target(id, repoLocal, "acme/app", &v1.Audience{OrgWide: true})
	current, stale := blob.Hash([]byte("# repo")), blob.Hash([]byte("# old"))
	row := func(checkout, hash string) *v1.InstalledArtifact {
		return &v1.InstalledArtifact{IdeType: claude, Repository: "acme/app", Checkout: checkout, SourcePath: "CLAUDE.md", ContentHash: hash}
	}

	e.report(tok, &v1.ReportStateRequest{KnownRepositories: []string{"acme/app"}, Installed: []*v1.InstalledArtifact{row("c1", stale), row("c2", current)}})
	wantStatus(t, e.statusOf(id, dev, repoLocal, "acme/app"), v1.EnforcementStatus_ENFORCEMENT_STATUS_INSTALLED)

	e.report(tok, &v1.ReportStateRequest{KnownRepositories: []string{"acme/app"}, Installed: []*v1.InstalledArtifact{row("c1", stale), row("c2", stale)}})
	wantStatus(t, e.statusOf(id, dev, repoLocal, "acme/app"), v1.EnforcementStatus_ENFORCEMENT_STATUS_PENDING)

	_, err := e.agent(tok).ReportState(ctx, connect.NewRequest(&v1.ReportStateRequest{Installed: []*v1.InstalledArtifact{row("c1", stale), row("c1", current)}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("one checkout reported twice: %v", err)
	}
	_, err = e.agent(tok).ReportState(ctx, connect.NewRequest(&v1.ReportStateRequest{Installed: []*v1.InstalledArtifact{{IdeType: claude, Checkout: "c1", SourcePath: ".claude/CLAUDE.md", ContentHash: current}}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("checkout without a repository: %v", err)
	}

	call(e.admin.RemoveArtifact(ctx, connect.NewRequest(&v1.RemoveArtifactRequest{ArtifactId: id}))).must(t)
	e.report(tok, &v1.ReportStateRequest{KnownRepositories: []string{"acme/app"}, Installed: []*v1.InstalledArtifact{row("c2", stale)}})
	wantStatus(t, e.statusOf(id, dev, repoLocal, "acme/app"), v1.EnforcementStatus_ENFORCEMENT_STATUS_REMOVAL_PENDING)
}

func TestPause(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	_, tok := e.person("p")
	id := e.upsert(&v1.UpsertArtifactRequest{IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_RULE, Slug: "r", InlineContent: "x"})
	e.target(id, userGlobal, "", &v1.Audience{OrgWide: true})

	call(e.admin.PauseEnforcement(ctx, connect.NewRequest(&v1.PauseEnforcementRequest{}))).must(t)
	set := call(e.agent(tok).GetEnforcedSet(ctx, connect.NewRequest(&v1.GetEnforcedSetRequest{}))).must(t)
	if !set.GetPaused() || len(set.GetArtifacts()) != 0 {
		t.Fatalf("paused set = %v", set)
	}
	if list := call(e.admin.ListArtifacts(ctx, connect.NewRequest(&v1.ListArtifactsRequest{}))).must(t); !list.GetEnforcementPaused() {
		t.Fatal("ListArtifacts does not report the pause")
	}
	call(e.admin.ResumeEnforcement(ctx, connect.NewRequest(&v1.ResumeEnforcementRequest{}))).must(t)
	set = call(e.agent(tok).GetEnforcedSet(ctx, connect.NewRequest(&v1.GetEnforcedSetRequest{}))).must(t)
	if set.GetPaused() || len(set.GetArtifacts()) != 1 {
		t.Fatalf("resumed set = %v", set)
	}
}

func TestHooks(t *testing.T) {
	ctx := context.Background()
	req := &v1.UpsertArtifactRequest{IdeType: v1.IdeType_IDE_TYPE_CODEX, Type: v1.ArtifactType_ARTIFACT_TYPE_HOOK, Slug: "guard",
		InlineContent: "#!/bin/sh\nexit 0\n", HookConfig: &v1.HookConfig{Event: "PreToolUse"}}

	off := newEnv(t, false)
	_, err := off.admin.UpsertArtifact(ctx, connect.NewRequest(req))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("hook with hooks disabled: %v", err)
	}

	on := newEnv(t, true)
	_, tok := on.person("p")
	id := on.upsert(req)
	on.target(id, userGlobal, "", &v1.Audience{OrgWide: true})
	set := call(on.agent(tok).GetEnforcedSet(ctx, connect.NewRequest(&v1.GetEnforcedSetRequest{}))).must(t)
	if len(set.GetArtifacts()) != 1 {
		t.Fatalf("set = %v", set)
	}
	ea := set.GetArtifacts()[0]
	if ea.GetHookCommand() != `"${CODEX_HOME:-$HOME/.codex}"/hooks/guard.sh` || !ea.GetFiles()[0].GetExecutable() || ea.GetHookConfig().GetEvent() != "PreToolUse" {
		t.Fatalf("hook = %v", ea)
	}
}

func TestConcurrentUpsertOneLiveRow(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	const n = 12
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := e.admin.UpsertArtifact(ctx, connect.NewRequest(&v1.UpsertArtifactRequest{
				IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_SKILL, Slug: "shared",
				Files: []*v1.ArtifactFile{{RelativePath: "SKILL.md", Content: fmt.Sprintf("v%d", i)}, {RelativePath: "scripts/run.sh", Content: "echo"}},
			}))
			errs[i] = err
			if err == nil {
				ids[i] = resp.Msg.GetArtifactId()
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
		if ids[i] != ids[0] {
			t.Fatalf("upserts returned different ids: %v", ids)
		}
	}
	var live int
	if err := e.st.Pool().QueryRow(ctx, `SELECT count(*) FROM org_artifacts WHERE slug = 'shared' AND deleted_at IS NULL`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Fatalf("live rows = %d, want 1", live)
	}
	var files int
	if err := e.st.Pool().QueryRow(ctx, `SELECT count(*) FROM artifact_files WHERE artifact_id = $1`, ids[0]).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if files != 2 {
		t.Fatalf("file rows = %d, want 2", files)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	dsn := testDatabase(t)
	for i := 0; i < 2; i++ {
		st, err := store.Open(context.Background(), dsn)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		st.Close()
	}
}
