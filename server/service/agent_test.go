package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
	"github.com/tetriz-ai/tetriz-harness/pkg/audience"
	"github.com/tetriz-ai/tetriz-harness/server/blob"
	"github.com/tetriz-ai/tetriz-harness/server/store"
)

const (
	artID    = "00000000-0000-0000-0000-0000000000a1"
	hookID   = "00000000-0000-0000-0000-0000000000a2"
	personID = "00000000-0000-0000-0000-0000000000b1"
)

func agentFixture(t *testing.T) (*fakeStore, blob.Store, string) {
	t.Helper()
	bs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := blob.Hash([]byte("rule body"))
	if err := bs.Put(context.Background(), h, []byte("rule body")); err != nil {
		t.Fatal(err)
	}
	until := fixedNow().Add(-time.Hour)
	fs := &fakeStore{
		artifacts: []*store.Artifact{
			{ID: artID, IdeType: claude, Type: rule, Slug: "style", PrimaryHash: h, EnforceUntil: until},
			{ID: hookID, IdeType: claude, Type: hook, Slug: "guard", PrimaryHash: h, EnforceUntil: fixedNow().Add(time.Hour), HookConfig: &v1.HookConfig{Event: "Stop"}},
		},
		targets: map[string][]audience.Row{
			artID:  {{Scope: v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL}},
			hookID: {{Scope: v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL}},
		},
		files: map[string][]store.ArtifactFile{
			artID:  {{ContentHash: h, IsPrimary: true}},
			hookID: {{ContentHash: h, IsPrimary: true}},
		},
	}
	return fs, bs, h
}

func TestGetEnforcedSet(t *testing.T) {
	ctx := WithPerson(context.Background(), personID)
	tests := []struct {
		name       string
		paused     bool
		allowHooks bool
		wantIDs    []string
	}{
		{"hooks hidden when disabled", false, false, []string{artID}},
		{"hooks served when allowed", false, true, []string{artID, hookID}},
		{"paused serves nothing", true, true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs, bs, _ := agentFixture(t)
			fs.paused = tt.paused
			resp, err := NewAgent(fs, &fakeDir{}, bs, Config{AllowHooks: tt.allowHooks, Now: fixedNow}, nil).
				GetEnforcedSet(ctx, connect.NewRequest(&v1.GetEnforcedSetRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			if resp.Msg.GetPaused() != tt.paused {
				t.Fatalf("paused = %v", resp.Msg.GetPaused())
			}
			var got []string
			for _, a := range resp.Msg.GetArtifacts() {
				got = append(got, a.GetArtifactId())
			}
			if strings.Join(got, ",") != strings.Join(tt.wantIDs, ",") {
				t.Fatalf("artifacts = %v, want %v", got, tt.wantIDs)
			}
			for _, a := range resp.Msg.GetArtifacts() {
				switch a.GetArtifactId() {
				case artID:
					if !a.GetExpired() || a.GetInstallPath() != ".claude/rules/style.md" {
						t.Fatalf("rule = %+v", a)
					}
				case hookID:
					if a.GetExpired() || a.GetHookCommand() != `"$HOME"/.claude/hooks/guard.sh` || !a.GetFiles()[0].GetExecutable() {
						t.Fatalf("hook = %+v", a)
					}
				}
			}
		})
	}
}

func TestGetArtifactBodies(t *testing.T) {
	ctx := WithPerson(context.Background(), personID)
	other := blob.Hash([]byte("someone else's"))
	tests := []struct {
		name     string
		paused   bool
		hashes   func(h string) []string
		wantCode connect.Code
	}{
		{"own hash served", false, func(h string) []string { return []string{h, h} }, 0},
		{"foreign hash refused", false, func(string) []string { return []string{other} }, connect.CodeNotFound},
		{"malformed hash", false, func(string) []string { return []string{"XYZ"} }, connect.CodeInvalidArgument},
		{"paused", true, func(h string) []string { return []string{h} }, connect.CodeFailedPrecondition},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs, bs, h := agentFixture(t)
			fs.paused = tt.paused
			resp, err := NewAgent(fs, &fakeDir{}, bs, Config{Now: fixedNow}, nil).
				GetArtifactBodies(ctx, connect.NewRequest(&v1.GetArtifactBodiesRequest{ContentHashes: tt.hashes(h)}))
			if tt.wantCode != 0 {
				if connect.CodeOf(err) != tt.wantCode {
					t.Fatalf("err = %v, want %v", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(resp.Msg.GetBodies()) != 1 || string(resp.Msg.GetBodies()[0].GetContent()) != "rule body" {
				t.Fatalf("bodies = %+v", resp.Msg.GetBodies())
			}
		})
	}
}

func TestReportStateValidation(t *testing.T) {
	ctx := WithPerson(context.Background(), personID)
	h := blob.Hash([]byte("x"))
	tests := []struct {
		name    string
		req     *v1.ReportStateRequest
		wantMsg string
	}{
		{"bad known repo", &v1.ReportStateRequest{KnownRepositories: []string{"nope"}}, "owner/repo"},
		{"unspecified ide", &v1.ReportStateRequest{Installed: []*v1.InstalledArtifact{{SourcePath: "a", ContentHash: h}}}, "ide_type"},
		{"bad hash", &v1.ReportStateRequest{Installed: []*v1.InstalledArtifact{{IdeType: claude, SourcePath: "a", ContentHash: "abc"}}}, "64 lowercase hex"},
		{"duplicate identity", &v1.ReportStateRequest{Installed: []*v1.InstalledArtifact{
			{IdeType: claude, SourcePath: "a", ContentHash: h}, {IdeType: claude, SourcePath: "a", ContentHash: h}}}, "twice"},
		{"bad failure id", &v1.ReportStateRequest{ApplyFailures: []*v1.ApplyFailure{{ArtifactId: "x"}}}, "uuid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewAgent(&fakeStore{}, &fakeDir{}, nil, Config{Now: fixedNow}, nil).ReportState(ctx, connect.NewRequest(tt.req))
			if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("err = %v, want InvalidArgument containing %q", err, tt.wantMsg)
			}
		})
	}
}

// TestEnforcedSetByTeam resolves a team row through the host's PersonTeams.
func TestEnforcedSetByTeam(t *testing.T) {
	fs, bs, _ := agentFixture(t)
	fs.targets = map[string][]audience.Row{artID: {{Scope: v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL, TeamID: "platform"}}}
	dir := &fakeDir{members: map[string][]string{"platform": {"dev-1"}}}
	ag := NewAgent(fs, dir, bs, Config{Now: fixedNow}, nil)
	for _, tc := range []struct {
		person string
		want   int
	}{{"dev-1", 1}, {"dev-2", 0}} {
		resp, err := ag.GetEnforcedSet(WithPerson(context.Background(), tc.person), connect.NewRequest(&v1.GetEnforcedSetRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if got := len(resp.Msg.GetArtifacts()); got != tc.want {
			t.Fatalf("%s: %d artifacts, want %d", tc.person, got, tc.want)
		}
	}
}

func TestAgentRequiresPerson(t *testing.T) {
	_, err := NewAgent(&fakeStore{}, &fakeDir{}, nil, Config{}, nil).GetEnforcedSet(context.Background(), connect.NewRequest(&v1.GetEnforcedSetRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("err = %v", err)
	}
}
