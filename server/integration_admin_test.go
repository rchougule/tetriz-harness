package server_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
	"github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1/harnessctlv1connect"
	"github.com/tetriz-ai/tetriz-harness/server/blob"
)

func TestUpsertEnforceUntil(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	rule := v1.ArtifactType_ARTIFACT_TYPE_RULE
	want := e.clock.Now().Add(40 * 24 * time.Hour).Truncate(time.Second)
	resp := call(e.admin.UpsertArtifact(ctx, connect.NewRequest(&v1.UpsertArtifactRequest{
		IdeType: claude, Type: rule, Slug: "long", InlineContent: "x", EnforceUntil: timestamppb.New(want),
	}))).must(t)
	if !resp.GetEnforceUntil().AsTime().Equal(want) {
		t.Fatalf("enforce_until = %v, want %v", resp.GetEnforceUntil().AsTime(), want)
	}
	list := call(e.admin.ListArtifacts(ctx, connect.NewRequest(&v1.ListArtifactsRequest{}))).must(t)
	if got := list.GetArtifacts()[0].GetEnforceUntil().AsTime(); !got.Equal(want) {
		t.Fatalf("stored enforce_until = %v, want %v", got, want)
	}

	// Targeting restarts the window but never shortens a chosen one.
	tr := call(e.admin.SetTargets(ctx, connect.NewRequest(&v1.SetTargetsRequest{ArtifactId: resp.GetArtifactId(), Scope: userGlobal, Audience: &v1.Audience{OrgWide: true}}))).must(t)
	if !tr.GetEnforceUntil().AsTime().Equal(want) {
		t.Fatalf("SetTargets enforce_until = %v, want %v", tr.GetEnforceUntil().AsTime(), want)
	}
	e.clock.Advance(30 * 24 * time.Hour)
	tr = call(e.admin.SetTargets(ctx, connect.NewRequest(&v1.SetTargetsRequest{ArtifactId: resp.GetArtifactId(), Scope: userGlobal, Audience: &v1.Audience{OrgWide: true}}))).must(t)
	if got, def := tr.GetEnforceUntil().AsTime(), e.clock.Now().Add(15*24*time.Hour); got.Sub(def).Abs() > time.Second {
		t.Fatalf("SetTargets past the chosen close = %v, want about %v", got, def)
	}

	// A save without it falls back to the default window.
	resp = call(e.admin.UpsertArtifact(ctx, connect.NewRequest(&v1.UpsertArtifactRequest{
		ArtifactId: resp.GetArtifactId(), IdeType: claude, Type: rule, Slug: "long",
	}))).must(t)
	if got, def := resp.GetEnforceUntil().AsTime(), e.clock.Now().Add(15*24*time.Hour); got.Sub(def).Abs() > time.Second {
		t.Fatalf("default enforce_until = %v, want about %v", got, def)
	}

	_, err := e.admin.UpsertArtifact(ctx, connect.NewRequest(&v1.UpsertArtifactRequest{
		IdeType: claude, Type: rule, Slug: "past", InlineContent: "x", EnforceUntil: timestamppb.New(e.clock.Now().Add(-time.Hour)),
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("past enforce_until: %v", err)
	}
}

func TestGetArtifact(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	skill := v1.ArtifactType_ARTIFACT_TYPE_SKILL
	files := []*v1.ArtifactFile{{RelativePath: "scripts/run.sh", Content: "echo"}, {RelativePath: "SKILL.md", Content: "# skill"}}
	id := e.upsert(&v1.UpsertArtifactRequest{IdeType: claude, Type: skill, Slug: "helper", Name: "Helper", Files: files})

	// No placement at all: the body is still readable for editing.
	got := call(e.admin.GetArtifact(ctx, connect.NewRequest(&v1.GetArtifactRequest{ArtifactId: id}))).must(t)
	if got.GetArtifact().GetName() != "Helper" || got.GetInlineContent() != "# skill" || len(got.GetArtifact().GetPlacements()) != 0 {
		t.Fatalf("artifact = %v", got)
	}
	if len(got.GetFiles()) != 2 || got.GetFiles()[0].GetRelativePath() != "SKILL.md" || got.GetFiles()[1].GetRelativePath() != "scripts/run.sh" || got.GetFiles()[1].GetContent() != "echo" {
		t.Fatalf("files = %v", got.GetFiles())
	}
	// The returned set round-trips through UpsertArtifact unchanged.
	e.upsert(&v1.UpsertArtifactRequest{ArtifactId: id, IdeType: claude, Type: skill, Slug: "helper", Name: "Helper", Files: got.GetFiles()})
	again := call(e.admin.GetArtifact(ctx, connect.NewRequest(&v1.GetArtifactRequest{ArtifactId: id}))).must(t)
	if again.GetArtifact().GetPrimaryHash() != got.GetArtifact().GetPrimaryHash() || len(again.GetFiles()) != 2 {
		t.Fatalf("round trip changed the artifact: %v", again)
	}

	rule := e.upsert(&v1.UpsertArtifactRequest{IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_RULE, Slug: "r", InlineContent: "be terse"})
	e.target(rule, userGlobal, "", &v1.Audience{OrgWide: true})
	r := call(e.admin.GetArtifact(ctx, connect.NewRequest(&v1.GetArtifactRequest{ArtifactId: rule}))).must(t)
	if r.GetInlineContent() != "be terse" || len(r.GetFiles()) != 0 || len(r.GetArtifact().GetPlacements()) != 1 {
		t.Fatalf("rule = %v", r)
	}

	_, err := e.admin.GetArtifact(ctx, connect.NewRequest(&v1.GetArtifactRequest{ArtifactId: "00000000-0000-0000-0000-00000000000f"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown artifact: %v", err)
	}
}

func TestStatusCounts(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	a, aTok := e.person("a")
	b, bTok := e.person("b")
	c, _ := e.person("c")
	team := e.team("t", a, b)
	rule := e.upsert(&v1.UpsertArtifactRequest{IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_RULE, Slug: "r", InlineContent: "rule"})
	e.target(rule, userGlobal, "", &v1.Audience{OrgWide: true})
	agent := e.upsert(&v1.UpsertArtifactRequest{IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_AGENT, Slug: "g", InlineContent: "agent"})
	e.target(agent, userGlobal, "", &v1.Audience{TeamIds: []string{team}})
	doc := e.upsert(&v1.UpsertArtifactRequest{IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_CLAUDE_MD, Slug: "d", InlineContent: "# doc"})
	e.target(doc, repoLocal, "acme/app", &v1.Audience{PersonIds: []string{a, c}})
	untargeted := e.upsert(&v1.UpsertArtifactRequest{IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_RULE, Slug: "u", InlineContent: "u"})

	e.report(aTok, &v1.ReportStateRequest{
		Installed:     []*v1.InstalledArtifact{installed(".claude/rules/r.md", blob.Hash([]byte("rule")))},
		ApplyFailures: []*v1.ApplyFailure{{ArtifactId: agent, ContentHash: blob.Hash([]byte("agent")), Reason: "denied"}},
	})
	e.report(bTok, &v1.ReportStateRequest{})

	want := map[string]*v1.StatusCounts{
		rule:       {Installed: 1, Pending: 1, Excluded: 1},
		agent:      {Failed: 1, Pending: 1},
		doc:        {Excluded: 2},
		untargeted: {},
	}
	check := func() {
		t.Helper()
		list := call(e.admin.ListArtifacts(ctx, connect.NewRequest(&v1.ListArtifactsRequest{}))).must(t)
		if len(list.GetArtifacts()) != len(want) {
			t.Fatalf("listed %d artifacts, want %d", len(list.GetArtifacts()), len(want))
		}
		for _, s := range list.GetArtifacts() {
			if !proto.Equal(s.GetStatusCounts(), want[s.GetArtifactId()]) {
				t.Fatalf("%s counts = %v, want %v", s.GetSlug(), s.GetStatusCounts(), want[s.GetArtifactId()])
			}
			// The counts agree with the per-person rows they summarise.
			rows := call(e.admin.GetEnforcementStatus(ctx, connect.NewRequest(&v1.GetEnforcementStatusRequest{ArtifactId: s.GetArtifactId()}))).must(t)
			tally := map[v1.EnforcementStatus]int32{}
			for _, r := range rows.GetStatuses() {
				tally[r.GetStatus()]++
			}
			sc := s.GetStatusCounts()
			if tally[v1.EnforcementStatus_ENFORCEMENT_STATUS_INSTALLED] != sc.GetInstalled() || tally[v1.EnforcementStatus_ENFORCEMENT_STATUS_PENDING] != sc.GetPending() ||
				tally[v1.EnforcementStatus_ENFORCEMENT_STATUS_FAILED] != sc.GetFailed() || tally[v1.EnforcementStatus_ENFORCEMENT_STATUS_EXCLUDED] != sc.GetExcluded() ||
				tally[v1.EnforcementStatus_ENFORCEMENT_STATUS_EXPIRED] != sc.GetExpired() {
				t.Fatalf("%s counts %v disagree with rows %v", s.GetSlug(), sc, tally)
			}
		}
		one := call(e.admin.GetArtifact(ctx, connect.NewRequest(&v1.GetArtifactRequest{ArtifactId: rule}))).must(t)
		if !proto.Equal(one.GetArtifact().GetStatusCounts(), want[rule]) {
			t.Fatalf("GetArtifact counts = %v, want %v", one.GetArtifact().GetStatusCounts(), want[rule])
		}
	}
	check()

	e.clock.Advance(16 * 24 * time.Hour)
	want = map[string]*v1.StatusCounts{rule: {Expired: 3}, agent: {Expired: 2}, doc: {Expired: 2}, untargeted: {}}
	check()
}

func TestGetSettings(t *testing.T) {
	ctx := context.Background()
	off := newEnv(t, false)
	s := call(off.admin.GetSettings(ctx, connect.NewRequest(&v1.GetSettingsRequest{}))).must(t)
	if s.GetEnforcementPaused() || s.GetHooksEnabled() || s.GetEnforcementWindow().AsDuration() != 15*24*time.Hour || s.GetMaxEnforcementWindow().AsDuration() != 90*24*time.Hour {
		t.Fatalf("settings = %v", s)
	}
	call(off.admin.PauseEnforcement(ctx, connect.NewRequest(&v1.PauseEnforcementRequest{}))).must(t)
	if !call(off.admin.GetSettings(ctx, connect.NewRequest(&v1.GetSettingsRequest{}))).must(t).GetEnforcementPaused() {
		t.Fatal("pause not reported")
	}
	on := newEnv(t, true)
	if !call(on.admin.GetSettings(ctx, connect.NewRequest(&v1.GetSettingsRequest{}))).must(t).GetHooksEnabled() {
		t.Fatal("hooks opt-in not reported")
	}
	// The handler checks no credential; the host in front of it does.
	if _, err := harnessctlv1connect.NewAdminServiceClient(on.srv.Client(), on.srv.URL).GetSettings(ctx, connect.NewRequest(&v1.GetSettingsRequest{})); err != nil {
		t.Fatalf("settings without an Authorization header: %v", err)
	}
}

func TestListRetractedArtifacts(t *testing.T) {
	e := newEnv(t, false)
	ctx := context.Background()
	_, goneTok := e.person("gone")
	_, keptTok := e.person("kept")
	agent := v1.ArtifactType_ARTIFACT_TYPE_AGENT
	id := e.upsert(&v1.UpsertArtifactRequest{IdeType: claude, Type: agent, Slug: "reviewer", InlineContent: "review"})
	e.target(id, userGlobal, "", &v1.Audience{OrgWide: true})
	path, hash := ".claude/agents/reviewer.md", blob.Hash([]byte("review"))
	e.report(goneTok, &v1.ReportStateRequest{Installed: []*v1.InstalledArtifact{installed(path, hash)}})
	e.report(keptTok, &v1.ReportStateRequest{Installed: []*v1.InstalledArtifact{installed(path, hash)}})
	call(e.admin.RemoveArtifact(ctx, connect.NewRequest(&v1.RemoveArtifactRequest{ArtifactId: id}))).must(t)
	e.report(goneTok, &v1.ReportStateRequest{})
	live := e.upsert(&v1.UpsertArtifactRequest{IdeType: claude, Type: agent, Slug: "reviewer", InlineContent: "review v2"})

	list := call(e.admin.ListArtifacts(ctx, connect.NewRequest(&v1.ListArtifactsRequest{}))).must(t)
	if len(list.GetArtifacts()) != 1 || list.GetArtifacts()[0].GetArtifactId() != live {
		t.Fatalf("default listing = %v", list.GetArtifacts())
	}
	list = call(e.admin.ListArtifacts(ctx, connect.NewRequest(&v1.ListArtifactsRequest{IncludeRetracted: true}))).must(t)
	if len(list.GetArtifacts()) != 2 || list.GetArtifacts()[0].GetRetractedAt() != nil {
		t.Fatalf("listing with retracted = %v", list.GetArtifacts())
	}
	r := list.GetArtifacts()[1]
	if r.GetArtifactId() != id || r.GetRetractedAt() == nil || len(r.GetPlacements()) != 1 || !r.GetPlacements()[0].GetAudience().GetOrgWide() {
		t.Fatalf("retracted summary = %v", r)
	}
	if !proto.Equal(r.GetStatusCounts(), &v1.StatusCounts{RemovalPending: 1, Removed: 1}) {
		t.Fatalf("retracted counts = %v", r.GetStatusCounts())
	}

	got := call(e.admin.GetArtifact(ctx, connect.NewRequest(&v1.GetArtifactRequest{ArtifactId: id}))).must(t)
	if got.GetArtifact().GetRetractedAt() == nil || got.GetInlineContent() != "review" {
		t.Fatalf("retracted artifact = %v", got)
	}
}
