package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
	"github.com/tetriz-ai/tetriz-harness/server/store"
)

const (
	claude = v1.IdeType_IDE_TYPE_CLAUDE_CODE
	skill  = v1.ArtifactType_ARTIFACT_TYPE_SKILL
	rule   = v1.ArtifactType_ARTIFACT_TYPE_RULE
	hook   = v1.ArtifactType_ARTIFACT_TYPE_HOOK
)

// TestUpsertValidation covers the rejections that never reach storage.
func TestUpsertValidation(t *testing.T) {
	hc := &v1.HookConfig{Event: "PreToolUse"}
	tests := []struct {
		name       string
		allowHooks bool
		req        *v1.UpsertArtifactRequest
		wantCode   connect.Code
		wantMsg    string
	}{
		{"hook rejected when disabled", false,
			&v1.UpsertArtifactRequest{IdeType: claude, Type: hook, Slug: "guard", InlineContent: "#!/bin/sh", HookConfig: hc},
			connect.CodeFailedPrecondition, "HARNESS_ALLOW_HOOKS"},
		{"hook needs hook_config", true,
			&v1.UpsertArtifactRequest{IdeType: claude, Type: hook, Slug: "guard", InlineContent: "#!/bin/sh"},
			connect.CodeInvalidArgument, "hook_config is required"},
		{"hook event outside vocabulary", true,
			&v1.UpsertArtifactRequest{IdeType: claude, Type: hook, Slug: "guard", InlineContent: "#!/bin/sh", HookConfig: &v1.HookConfig{Event: "sessionStart"}},
			connect.CodeInvalidArgument, "hook event"},
		{"hook_config on a rule", false,
			&v1.UpsertArtifactRequest{IdeType: claude, Type: rule, Slug: "r", InlineContent: "x", HookConfig: hc},
			connect.CodeInvalidArgument, "only for a HOOK"},
		{"unenforceable type", false,
			&v1.UpsertArtifactRequest{IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_UNSPECIFIED, Slug: "r", InlineContent: "x"},
			connect.CodeInvalidArgument, "not enforceable"},
		{"bad slug", false,
			&v1.UpsertArtifactRequest{IdeType: claude, Type: rule, Slug: "../x", InlineContent: "x"},
			connect.CodeInvalidArgument, "slug"},
		{"long name", false,
			&v1.UpsertArtifactRequest{IdeType: claude, Type: rule, Slug: "r", Name: strings.Repeat("n", MaxNameLen+1), InlineContent: "x"},
			connect.CodeInvalidArgument, "name exceeds"},
		{"create without content", false,
			&v1.UpsertArtifactRequest{IdeType: claude, Type: rule, Slug: "r"},
			connect.CodeInvalidArgument, "required when creating"},
		{"both content sources", false,
			&v1.UpsertArtifactRequest{IdeType: claude, Type: skill, Slug: "s", InlineContent: "x", Files: []*v1.ArtifactFile{{RelativePath: "SKILL.md", Content: "x"}}},
			connect.CodeInvalidArgument, "not both"},
		{"files on a rule", false,
			&v1.UpsertArtifactRequest{IdeType: claude, Type: rule, Slug: "r", Files: []*v1.ArtifactFile{{RelativePath: "a.md", Content: "x"}}},
			connect.CodeInvalidArgument, "only supported for SKILL"},
		{"skill without primary", false,
			&v1.UpsertArtifactRequest{IdeType: claude, Type: skill, Slug: "s", Files: []*v1.ArtifactFile{{RelativePath: "README.md", Content: "x"}}},
			connect.CodeInvalidArgument, "primary file"},
		{"traversing file", false,
			&v1.UpsertArtifactRequest{IdeType: claude, Type: skill, Slug: "s", Files: []*v1.ArtifactFile{{RelativePath: "SKILL.md", Content: "x"}, {RelativePath: "../x", Content: "y"}}},
			connect.CodeInvalidArgument, "path-safe"},
		{"body too large", false,
			&v1.UpsertArtifactRequest{IdeType: claude, Type: rule, Slug: "r", InlineContent: strings.Repeat("x", MaxBodyBytes+1)},
			connect.CodeInvalidArgument, "exceeds"},
		{"enforce_until in the past", false,
			&v1.UpsertArtifactRequest{IdeType: claude, Type: rule, Slug: "r", InlineContent: "x", EnforceUntil: timestamppb.New(fixedNow().Add(-time.Second))},
			connect.CodeInvalidArgument, "in the future"},
		{"enforce_until now", false,
			&v1.UpsertArtifactRequest{IdeType: claude, Type: rule, Slug: "r", InlineContent: "x", EnforceUntil: timestamppb.New(fixedNow())},
			connect.CodeInvalidArgument, "in the future"},
		{"enforce_until beyond the maximum", false,
			&v1.UpsertArtifactRequest{IdeType: claude, Type: rule, Slug: "r", InlineContent: "x", EnforceUntil: timestamppb.New(fixedNow().Add(MaxEnforcementWindow + time.Second))},
			connect.CodeInvalidArgument, "at most 90 days"},
		{"malformed artifact id", false,
			&v1.UpsertArtifactRequest{ArtifactId: "nope", IdeType: claude, Type: rule, Slug: "r"},
			connect.CodeInvalidArgument, "uuid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewAdmin(&fakeStore{}, &fakeDir{}, nil, Config{AllowHooks: tt.allowHooks, Now: fixedNow})
			_, err := a.UpsertArtifact(context.Background(), connect.NewRequest(tt.req))
			if connect.CodeOf(err) != tt.wantCode || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("err = %v, want %v containing %q", err, tt.wantCode, tt.wantMsg)
			}
		})
	}
}

func TestUpsertIdentityImmutable(t *testing.T) {
	id := "00000000-0000-0000-0000-000000000001"
	fs := &fakeStore{artifacts: []*store.Artifact{{ID: id, IdeType: claude, Type: rule, Slug: "r"}}}
	a := NewAdmin(fs, &fakeDir{}, nil, Config{Now: fixedNow})
	_, err := a.UpsertArtifact(context.Background(), connect.NewRequest(&v1.UpsertArtifactRequest{
		ArtifactId: id, IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_AGENT, Slug: "r",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "cannot change") {
		t.Fatalf("err = %v", err)
	}
}

func TestSetTargetsValidation(t *testing.T) {
	id := "00000000-0000-0000-0000-000000000001"
	user := v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL
	repo := v1.ArtifactScope_ARTIFACT_SCOPE_REPO_LOCAL
	fs := &fakeStore{artifacts: []*store.Artifact{
		{ID: id, IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_AGENTS_MD, Slug: "a"},
		{ID: "00000000-0000-0000-0000-000000000002", IdeType: claude, Type: hook, Slug: "h"},
	}}
	tests := []struct {
		name     string
		req      *v1.SetTargetsRequest
		wantCode connect.Code
		wantMsg  string
	}{
		{"repo on user-global", &v1.SetTargetsRequest{ArtifactId: id, Scope: user, Repository: "a/b"}, connect.CodeInvalidArgument, "must be empty"},
		{"repo-local needs repo", &v1.SetTargetsRequest{ArtifactId: id, Scope: repo}, connect.CodeInvalidArgument, "required"},
		{"repo not owner/repo", &v1.SetTargetsRequest{ArtifactId: id, Scope: repo, Repository: "just-a-name"}, connect.CodeInvalidArgument, "owner/repo"},
		{"unspecified scope", &v1.SetTargetsRequest{ArtifactId: id}, connect.CodeInvalidArgument, "scope"},
		{"two selectors", &v1.SetTargetsRequest{ArtifactId: id, Scope: repo, Repository: "a/b",
			Audience: &v1.Audience{OrgWide: true, PersonIds: []string{id}}}, connect.CodeInvalidArgument, "exactly one"},
		{"empty person id", &v1.SetTargetsRequest{ArtifactId: id, Scope: repo, Repository: "a/b",
			Audience: &v1.Audience{PersonIds: []string{""}}}, connect.CodeInvalidArgument, "1-200 bytes"},
		{"long team id", &v1.SetTargetsRequest{ArtifactId: id, Scope: repo, Repository: "a/b",
			Audience: &v1.Audience{TeamIds: []string{strings.Repeat("t", MaxIDLen+1)}}}, connect.CodeInvalidArgument, "1-200 bytes"},
		{"person the host does not list", &v1.SetTargetsRequest{ArtifactId: id, Scope: repo, Repository: "a/b",
			Audience: &v1.Audience{PersonIds: []string{"dev-1", "stranger"}}}, connect.CodeNotFound, "stranger: person not found"},
		{"team the host does not list", &v1.SetTargetsRequest{ArtifactId: id, Scope: repo, Repository: "a/b",
			Audience: &v1.Audience{TeamIds: []string{"nope"}}}, connect.CodeNotFound, "nope: team not found"},
		{"no user-global AGENTS.md", &v1.SetTargetsRequest{ArtifactId: id, Scope: user, Audience: &v1.Audience{OrgWide: true}},
			connect.CodeInvalidArgument, "not supported"},
		{"hook targeting refused when disabled", &v1.SetTargetsRequest{ArtifactId: "00000000-0000-0000-0000-000000000002", Scope: user,
			Audience: &v1.Audience{OrgWide: true}}, connect.CodeFailedPrecondition, "HARNESS_ALLOW_HOOKS"},
		{"unknown artifact", &v1.SetTargetsRequest{ArtifactId: "00000000-0000-0000-0000-00000000000f", Scope: user}, connect.CodeNotFound, "not found"},
	}
	dir := &fakeDir{persons: []Person{{ID: "dev-1"}}, teams: []Team{{ID: "platform"}}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewAdmin(fs, dir, nil, Config{Now: fixedNow})
			_, err := a.SetTargets(context.Background(), connect.NewRequest(tt.req))
			if connect.CodeOf(err) != tt.wantCode || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("err = %v, want %v containing %q", err, tt.wantCode, tt.wantMsg)
			}
		})
	}
}

// TestSetTargetsHostIDs accepts any id the host lists, uuid or not.
func TestSetTargetsHostIDs(t *testing.T) {
	id := "00000000-0000-0000-0000-000000000001"
	fs := &fakeStore{artifacts: []*store.Artifact{{ID: id, IdeType: claude, Type: rule, Slug: "r"}}}
	dir := &fakeDir{persons: []Person{{ID: "dev-1"}, {ID: "ada@example.com"}}, teams: []Team{{ID: "team/platform"}}}
	a := NewAdmin(fs, dir, nil, Config{Now: fixedNow})
	for _, aud := range []*v1.Audience{{PersonIds: []string{"dev-1", "ada@example.com"}}, {TeamIds: []string{"team/platform"}}} {
		if _, err := a.SetTargets(context.Background(), connect.NewRequest(&v1.SetTargetsRequest{
			ArtifactId: id, Scope: v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL, Audience: aud,
		})); err != nil {
			t.Fatalf("SetTargets(%v): %v", aud, err)
		}
	}
	if fs.setCalls != 2 {
		t.Fatalf("store SetTargets ran %d times, want 2", fs.setCalls)
	}
}

// TestDirectoryReads serves the pickers from the host directory.
func TestDirectoryReads(t *testing.T) {
	dir := &fakeDir{
		persons: []Person{{ID: "dev-1", DisplayName: "Dev One"}, {ID: "dev-2", DisplayName: "Dev Two"}},
		teams:   []Team{{ID: "platform", Name: "Platform"}},
		members: map[string][]string{"platform": {"dev-2"}},
	}
	a := NewAdmin(&fakeStore{}, dir, nil, Config{Now: fixedNow})
	ctx := context.Background()
	ps, err := a.ListPersons(ctx, connect.NewRequest(&v1.ListPersonsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := ps.Msg.GetPersons(); len(got) != 2 || got[0].GetPersonId() != "dev-1" || got[1].GetDisplayName() != "Dev Two" {
		t.Fatalf("persons = %v", got)
	}
	ts, err := a.ListTeams(ctx, connect.NewRequest(&v1.ListTeamsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := ts.Msg.GetTeams(); len(got) != 1 || got[0].GetName() != "Platform" || len(got[0].GetMemberPersonIds()) != 1 || got[0].GetMemberPersonIds()[0] != "dev-2" {
		t.Fatalf("teams = %v", got)
	}
}

func TestGetSettingsReportsConfig(t *testing.T) {
	a := NewAdmin(&fakeStore{paused: true}, &fakeDir{}, nil, Config{AllowHooks: true, Version: "1.2.3", Now: fixedNow})
	resp, err := a.GetSettings(context.Background(), connect.NewRequest(&v1.GetSettingsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Msg
	if !m.GetEnforcementPaused() || !m.GetHooksEnabled() || m.GetServerVersion() != "1.2.3" || m.GetMaxEnforcementWindow().AsDuration() != MaxEnforcementWindow {
		t.Fatalf("settings = %v", m)
	}
}

func TestInstallPreviewScope(t *testing.T) {
	codex := v1.IdeType_IDE_TYPE_CODEX
	tests := []struct {
		name     string
		req      *v1.GetInstallPreviewRequest
		wantPath string
		wantRoot string
		wantCode connect.Code
	}{
		{"unspecified is repo-local", &v1.GetInstallPreviewRequest{IdeType: claude, Type: rule, Slug: "r"}, ".claude/rules/r.md", "<checkout>", 0},
		{"user-global", &v1.GetInstallPreviewRequest{IdeType: claude, Type: rule, Slug: "r", Scope: v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL}, ".claude/rules/r.md", "~", 0},
		{"user-global codex hook", &v1.GetInstallPreviewRequest{IdeType: codex, Type: hook, Slug: "h", Scope: v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL}, "hooks/h.sh", "~/.codex", 0},
		{"user-global agents md", &v1.GetInstallPreviewRequest{IdeType: claude, Type: v1.ArtifactType_ARTIFACT_TYPE_AGENTS_MD, Slug: "a", Scope: v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL}, "", "", connect.CodeInvalidArgument},
	}
	a := NewAdmin(&fakeStore{}, &fakeDir{}, nil, Config{Now: fixedNow})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := a.GetInstallPreview(context.Background(), connect.NewRequest(tt.req))
			if tt.wantCode != 0 {
				if connect.CodeOf(err) != tt.wantCode {
					t.Fatalf("err = %v, want %v", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if f := resp.Msg.GetFiles(); len(f) != 1 || f[0].GetRelativePath() != tt.wantPath || resp.Msg.GetRoot() != tt.wantRoot {
				t.Fatalf("preview = %v", resp.Msg)
			}
		})
	}
}
