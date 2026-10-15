package server_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
	"github.com/tetriz-ai/tetriz-harness/server/blob"
)

// TestHostDirectory drives the server purely from the host's directory: a
// team the host defines is targeted, a member converges, a person the host
// removes drops out, and an agent call the host bound no person to fails.
func TestHostDirectory(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	alice, aliceTok := e.person("alice")
	bob, bobTok := e.person("bob")
	team := e.team("platform", alice, bob)

	// The admin client sends no credentials: authenticating admins is the host's job.
	id := e.upsert(&v1.UpsertArtifactRequest{IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_RULE, Slug: "style", InlineContent: "be terse"})
	e.target(id, userGlobal, "", &v1.Audience{TeamIds: []string{team}})

	teams := call(e.admin.ListTeams(ctx, connect.NewRequest(&v1.ListTeamsRequest{}))).must(t)
	if len(teams.GetTeams()) != 1 || teams.GetTeams()[0].GetTeamId() != team {
		t.Fatalf("teams = %v", teams.GetTeams())
	}

	set := call(e.agent(aliceTok).GetEnforcedSet(ctx, connect.NewRequest(&v1.GetEnforcedSetRequest{}))).must(t)
	if len(set.GetArtifacts()) != 1 || set.GetArtifacts()[0].GetArtifactId() != id {
		t.Fatalf("enforced set = %v", set.GetArtifacts())
	}
	path, hash := set.GetArtifacts()[0].GetInstallPath(), blob.Hash([]byte("be terse"))
	e.report(aliceTok, &v1.ReportStateRequest{Installed: []*v1.InstalledArtifact{installed(path, hash)}})
	e.report(bobTok, &v1.ReportStateRequest{Installed: []*v1.InstalledArtifact{installed(path, hash)}})
	wantStatus(t, e.statusOf(id, alice, userGlobal, ""), v1.EnforcementStatus_ENFORCEMENT_STATUS_INSTALLED)
	wantStatus(t, e.statusOf(id, bob, userGlobal, ""), v1.EnforcementStatus_ENFORCEMENT_STATUS_INSTALLED)

	e.dir.remove(bob)
	resp := call(e.admin.GetEnforcementStatus(ctx, connect.NewRequest(&v1.GetEnforcementStatusRequest{ArtifactId: id}))).must(t)
	for _, s := range resp.GetStatuses() {
		if s.GetPersonId() == bob {
			t.Fatalf("removed person still in status: %v", s)
		}
	}
	if len(resp.GetStatuses()) != 1 {
		t.Fatalf("statuses = %v", resp.GetStatuses())
	}

	_, err := e.agent("not-a-host-token").GetEnforcedSet(ctx, connect.NewRequest(&v1.GetEnforcedSetRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("agent call without a person: %v", err)
	}

	_, err = e.admin.SetTargets(ctx, connect.NewRequest(&v1.SetTargetsRequest{ArtifactId: id, Scope: userGlobal, Audience: &v1.Audience{TeamIds: []string{"team:unknown"}}}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("targeting a team the host does not list: %v", err)
	}
}
