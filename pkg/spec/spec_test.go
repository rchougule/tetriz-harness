package spec

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
)

const (
	claude = v1.IdeType_IDE_TYPE_CLAUDE_CODE
	cursor = v1.IdeType_IDE_TYPE_CURSOR
	codex  = v1.IdeType_IDE_TYPE_CODEX
	kiro   = v1.IdeType_IDE_TYPE_KIRO
	noIde  = v1.IdeType_IDE_TYPE_UNSPECIFIED

	skill    = v1.ArtifactType_ARTIFACT_TYPE_SKILL
	hook     = v1.ArtifactType_ARTIFACT_TYPE_HOOK
	agent    = v1.ArtifactType_ARTIFACT_TYPE_AGENT
	rule     = v1.ArtifactType_ARTIFACT_TYPE_RULE
	claudeMd = v1.ArtifactType_ARTIFACT_TYPE_CLAUDE_MD
	agentsMd = v1.ArtifactType_ARTIFACT_TYPE_AGENTS_MD
	noType   = v1.ArtifactType_ARTIFACT_TYPE_UNSPECIFIED

	userGlobal = v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL
	repoLocal  = v1.ArtifactScope_ARTIFACT_SCOPE_REPO_LOCAL
	noScope    = v1.ArtifactScope_ARTIFACT_SCOPE_UNSPECIFIED
)

// A drift in any row silently breaks the status join, which matches on this exact path.
func TestInstallPath(t *testing.T) {
	tests := []struct {
		name    string
		ide     v1.IdeType
		typ     v1.ArtifactType
		slug    string
		scope   v1.ArtifactScope
		want    string
		wantErr error
	}{
		// Claude Code, user scope.
		{"claude claude_md user", claude, claudeMd, "anything", userGlobal, ".claude/CLAUDE.md", nil},
		{"claude skill is its SKILL.md", claude, skill, "code-review", userGlobal, ".claude/skills/code-review/SKILL.md", nil},
		{"claude agent user", claude, agent, "planner", userGlobal, ".claude/agents/planner.md", nil},
		{"claude rule user", claude, rule, "no-secrets", userGlobal, ".claude/rules/no-secrets.md", nil},
		{"claude hook user", claude, hook, "guard", userGlobal, ".claude/hooks/guard.sh", nil},
		// Claude Code, repo scope.
		{"claude claude_md repo root", claude, claudeMd, "anything", repoLocal, "CLAUDE.md", nil},
		{"claude skill repo", claude, skill, "code-review", repoLocal, ".claude/skills/code-review/SKILL.md", nil},
		{"claude agent repo", claude, agent, "planner", repoLocal, ".claude/agents/planner.md", nil},
		{"claude rule repo", claude, rule, "no-secrets", repoLocal, ".claude/rules/no-secrets.md", nil},
		{"claude hook repo", claude, hook, "guard", repoLocal, ".claude/hooks/guard.sh", nil},
		// Cursor.
		{"cursor rule user is .mdc", cursor, rule, "no-secrets", userGlobal, ".cursor/rules/no-secrets.mdc", nil},
		{"cursor agent user", cursor, agent, "planner", userGlobal, ".cursor/agents/planner.md", nil},
		{"cursor skill user", cursor, skill, "code-review", userGlobal, ".cursor/skills/code-review/SKILL.md", nil},
		{"cursor claude_md user", cursor, claudeMd, "anything", userGlobal, ".claude/CLAUDE.md", nil},
		{"cursor hook user", cursor, hook, "guard", userGlobal, ".cursor/hooks/guard.sh", nil},
		{"cursor rule repo", cursor, rule, "no-secrets", repoLocal, ".cursor/rules/no-secrets.mdc", nil},
		{"cursor agent repo", cursor, agent, "planner", repoLocal, ".cursor/agents/planner.md", nil},
		{"cursor skill repo", cursor, skill, "code-review", repoLocal, ".cursor/skills/code-review/SKILL.md", nil},
		{"cursor claude_md repo", cursor, claudeMd, "anything", repoLocal, "CLAUDE.md", nil},
		{"cursor hook repo", cursor, hook, "guard", repoLocal, ".cursor/hooks/guard.sh", nil},
		// Codex: skills in the cross-tool tree, user-scope hooks relative to the Codex home.
		{"codex skill user", codex, skill, "code-review", userGlobal, ".agents/skills/code-review/SKILL.md", nil},
		{"codex hook user drops .codex", codex, hook, "guard", userGlobal, "hooks/guard.sh", nil},
		{"codex claude_md user", codex, claudeMd, "anything", userGlobal, ".claude/CLAUDE.md", nil},
		{"codex rule user unsupported", codex, rule, "x", userGlobal, "", ErrIdeNotSupported},
		{"codex agent user unsupported", codex, agent, "x", userGlobal, "", ErrIdeNotSupported},
		{"codex rule repo", codex, rule, "no-secrets", repoLocal, ".codex/rules/no-secrets.md", nil},
		{"codex agent repo", codex, agent, "planner", repoLocal, ".codex/agents/planner.md", nil},
		{"codex skill repo", codex, skill, "code-review", repoLocal, ".agents/skills/code-review/SKILL.md", nil},
		{"codex hook repo keeps .codex", codex, hook, "guard", repoLocal, ".codex/hooks/guard.sh", nil},
		// Kiro: one layout at both scopes; hooks are repo-only manifests.
		{"kiro rule user", kiro, rule, "no-secrets", userGlobal, ".kiro/steering/no-secrets.md", nil},
		{"kiro skill user", kiro, skill, "code-review", userGlobal, ".kiro/skills/code-review/SKILL.md", nil},
		{"kiro agent user", kiro, agent, "planner", userGlobal, ".kiro/agents/planner.md", nil},
		{"kiro rule repo", kiro, rule, "no-secrets", repoLocal, ".kiro/steering/no-secrets.md", nil},
		{"kiro hook repo is a manifest", kiro, hook, "guard", repoLocal, ".kiro/hooks/guard.kiro.hook", nil},
		{"kiro hook user unsupported", kiro, hook, "guard", userGlobal, "", ErrIdeNotSupported},
		// AGENTS.md: repo root for every tool, never at user scope.
		{"claude agents_md repo", claude, agentsMd, "anything", repoLocal, "AGENTS.md", nil},
		{"codex agents_md repo", codex, agentsMd, "anything", repoLocal, "AGENTS.md", nil},
		{"kiro agents_md repo", kiro, agentsMd, "anything", repoLocal, "AGENTS.md", nil},
		{"cursor agents_md user unsupported", cursor, agentsMd, "anything", userGlobal, "", ErrIdeNotSupported},
		{"codex agents_md user unsupported", codex, agentsMd, "anything", userGlobal, "", ErrIdeNotSupported},
		// Fail-closed boundaries.
		{"unspecified scope", claude, skill, "s", noScope, "", ErrScopeNotSupported},
		{"unspecified type", claude, noType, "s", userGlobal, "", ErrTypeNotEnforceable},
		{"unknown ide skill", noIde, skill, "s", userGlobal, "", ErrIdeNotSupported},
		{"unknown ide rule repo", noIde, rule, "x", repoLocal, "", ErrIdeNotSupported},
		{"unknown ide hook", noIde, hook, "guard", userGlobal, "", ErrIdeNotSupported},
		{"traversal slug", claude, skill, "../../evil", userGlobal, "", ErrInvalidSlug},
		{"uppercase slug", claude, rule, "NoCaps", userGlobal, "", ErrInvalidSlug},
		{"slash slug", claude, agent, "a/b", userGlobal, "", ErrInvalidSlug},
		{"hook traversal slug", claude, hook, "../evil", userGlobal, "", ErrInvalidSlug},
		{"over-long slug", claude, rule, strings.Repeat("a", MaxSlugLen+1), userGlobal, "", ErrInvalidSlug},
		{"max-length slug", claude, rule, strings.Repeat("a", MaxSlugLen), userGlobal, ".claude/rules/" + strings.Repeat("a", MaxSlugLen) + ".md", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := InstallPath(tt.ide, tt.typ, tt.slug, tt.scope)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("path = %q, want %q", got, tt.want)
			}
		})
	}
}

// The command is a function of (ide, scope, slug) only; no event or matcher
// input exists, so neither can change what runs.
func TestHookCommand(t *testing.T) {
	tests := []struct {
		name    string
		ide     v1.IdeType
		scope   v1.ArtifactScope
		slug    string
		want    string
		wantErr error
	}{
		{"claude user", claude, userGlobal, "guard", `"$HOME"/.claude/hooks/guard.sh`, nil},
		{"cursor user", cursor, userGlobal, "guard", `"$HOME"/.cursor/hooks/guard.sh`, nil},
		{"codex user", codex, userGlobal, "guard", `"${CODEX_HOME:-$HOME/.codex}"/hooks/guard.sh`, nil},
		{"claude repo", claude, repoLocal, "guard", `"$CLAUDE_PROJECT_DIR"/.claude/hooks/guard.sh`, nil},
		{"cursor repo is repo-relative", cursor, repoLocal, "guard", `.cursor/hooks/guard.sh`, nil},
		{"codex repo", codex, repoLocal, "guard", `"$(git rev-parse --show-toplevel)"/.codex/hooks/guard.sh`, nil},
		{"kiro repo self-activates", kiro, repoLocal, "guard", "", nil},
		{"kiro user self-activates", kiro, userGlobal, "guard", "", nil},
		{"unspecified scope", claude, noScope, "guard", "", ErrScopeNotSupported},
		{"traversal slug", claude, userGlobal, "../evil", "", ErrInvalidSlug},
		{"unknown ide", noIde, userGlobal, "guard", "", ErrIdeNotSupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HookCommand(tt.ide, tt.scope, tt.slug)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("command = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateHookEvent(t *testing.T) {
	tests := []struct {
		name    string
		ide     v1.IdeType
		event   string
		wantErr error
	}{
		{"claude PreToolUse", claude, "PreToolUse", nil},
		{"cursor beforeSubmitPrompt", cursor, "beforeSubmitPrompt", nil},
		{"claude event on cursor", cursor, "PreToolUse", ErrInvalidHookEvent},
		{"kiro PostFileSave", kiro, "PostFileSave", nil},
		{"kiro PreToolUse", kiro, "PreToolUse", nil},
		{"claude-only event on kiro", kiro, "Notification", ErrInvalidHookEvent},
		{"codex SubagentStart", codex, "SubagentStart", nil},
		{"empty event", claude, "", ErrInvalidHookEvent},
		{"unknown ide", noIde, "PreToolUse", ErrIdeNotSupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateHookEvent(tt.ide, tt.event); !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateRelativePath(t *testing.T) {
	tests := []struct {
		rel    string
		wantOK bool
	}{
		{"SKILL.md", true},
		{"helper.py", true},
		{"scripts/run.sh", true},
		{"a_b-c.txt", true},
		{"reference/palette.md", true},
		{".gitignore", true},
		{"", false},
		{"/abs/path", false},
		{"../evil", false},
		{"a/../b", false},
		{"..", false},
		{".", false},
		{"a//b", false},
		{"a/./b", false},
		{"sp ace.md", false},
		{"-rf", false},
		{"--force", false},
		{"scripts/-rf", false},
		{strings.Repeat("a", MaxRelativePathLen+1), false},
	}
	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			err := ValidateRelativePath(tt.rel)
			if tt.wantOK && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tt.wantOK && !errors.Is(err, ErrInvalidRelativePath) {
				t.Fatalf("err = %v, want ErrInvalidRelativePath", err)
			}
		})
	}
}

// The primary (relPath "") must land exactly at root so a single-file artifact is
// unchanged by the multi-file model; helpers are siblings, not children, of it.
func TestFileInstallPath(t *testing.T) {
	root := ".claude/skills/code-review/SKILL.md"
	tests := []struct {
		name string
		rel  string
		want string
	}{
		{"primary lands at root", "", root},
		{"primary by name lands at root", "SKILL.md", root},
		{"helper is a sibling", "helper.py", ".claude/skills/code-review/helper.py"},
		{"nested helper", "scripts/run.sh", ".claude/skills/code-review/scripts/run.sh"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FileInstallPath(root, tt.rel); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
	if got := PrimaryFileName(root); got != "SKILL.md" {
		t.Fatalf("PrimaryFileName = %q, want SKILL.md", got)
	}
}

func TestPreview(t *testing.T) {
	tests := []struct {
		name    string
		ide     v1.IdeType
		typ     v1.ArtifactType
		slug    string
		files   []string
		want    []PreviewFile
		wantErr error
	}{
		{"claude skill is its SKILL.md", claude, skill, "code-review", nil, []PreviewFile{{".claude/skills/code-review/SKILL.md", true}}, nil},
		{"claude rule", claude, rule, "no-secrets", nil, []PreviewFile{{".claude/rules/no-secrets.md", true}}, nil},
		{"claude agent", claude, agent, "planner", nil, []PreviewFile{{".claude/agents/planner.md", true}}, nil},
		{"claude hook is its script", claude, hook, "guard", nil, []PreviewFile{{".claude/hooks/guard.sh", true}}, nil},
		{"cursor rule is .mdc", cursor, rule, "style", nil, []PreviewFile{{".cursor/rules/style.mdc", true}}, nil},
		{"codex skill cross-tool", codex, skill, "lint", nil, []PreviewFile{{".agents/skills/lint/SKILL.md", true}}, nil},
		{
			"directory expands in request order", claude, skill, "reviewer", []string{"SKILL.md", "scripts/run.sh"},
			[]PreviewFile{{".claude/skills/reviewer/SKILL.md", true}, {".claude/skills/reviewer/scripts/run.sh", false}}, nil,
		},
		{
			"primary need not be first", claude, skill, "reviewer", []string{"helper.py", "SKILL.md"},
			[]PreviewFile{{".claude/skills/reviewer/helper.py", false}, {".claude/skills/reviewer/SKILL.md", true}}, nil,
		},
		{"unknown ide", noIde, skill, "s", nil, nil, ErrIdeNotSupported},
		{"unspecified type", claude, noType, "x", nil, nil, ErrTypeNotEnforceable},
		{"multi-file non-skill", claude, rule, "r", []string{"a.md", "b.md"}, nil, ErrMultiFileTypeNotAllowed},
		{"single-file non-skill", claude, rule, "r", []string{"r.md"}, nil, ErrMultiFileTypeNotAllowed},
		{"missing primary", claude, skill, "s", []string{"helper.py"}, nil, ErrPrimaryFileMissing},
		{"duplicate path", claude, skill, "s", []string{"SKILL.md", "SKILL.md"}, nil, ErrDuplicateRelativePath},
		{"traversal file", claude, skill, "s", []string{"SKILL.md", "../x"}, nil, ErrInvalidRelativePath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Preview(tt.ide, tt.typ, tt.slug, repoLocal, tt.files)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			if err == nil {
				// The preview is exactly the enforce path's REPO_LOCAL derivation.
				root, _ := InstallPath(tt.ide, tt.typ, tt.slug, repoLocal)
				for _, f := range got {
					if f.IsPrimary && f.RelativePath != root {
						t.Fatalf("primary %q != InstallPath %q", f.RelativePath, root)
					}
				}
			}
		})
	}
}

func TestPreviewUserScope(t *testing.T) {
	tests := []struct {
		name    string
		ide     v1.IdeType
		typ     v1.ArtifactType
		want    string
		wantErr error
	}{
		{"claude skill under home", claude, skill, ".claude/skills/s/SKILL.md", nil},
		{"codex hook under the codex home", codex, hook, "hooks/s.sh", nil},
		{"claude md under home", claude, v1.ArtifactType_ARTIFACT_TYPE_CLAUDE_MD, ".claude/CLAUDE.md", nil},
		{"no user-scope agents md", claude, v1.ArtifactType_ARTIFACT_TYPE_AGENTS_MD, "", ErrIdeNotSupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Preview(tt.ide, tt.typ, "s", v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL, nil)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err == nil && (len(got) != 1 || got[0].RelativePath != tt.want || !got[0].IsPrimary) {
				t.Fatalf("got %+v, want %q", got, tt.want)
			}
		})
	}
}

func TestTypeSets(t *testing.T) {
	tests := []struct {
		typ         v1.ArtifactType
		instruction bool
		enforceable bool
		rootDoc     bool
	}{
		{skill, true, true, false},
		{agent, true, true, false},
		{rule, true, true, false},
		{claudeMd, true, true, true},
		{agentsMd, true, true, true},
		{hook, false, true, false},
		{noType, false, false, false},
		{v1.ArtifactType(3), false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.typ.String(), func(t *testing.T) {
			if got := IsInstructionType(tt.typ); got != tt.instruction {
				t.Errorf("IsInstructionType = %v", got)
			}
			if got := IsEnforceable(tt.typ); got != tt.enforceable {
				t.Errorf("IsEnforceable = %v", got)
			}
			if got := IsRootDocType(tt.typ); got != tt.rootDoc {
				t.Errorf("IsRootDocType = %v", got)
			}
		})
	}
}

func TestInstallRoot(t *testing.T) {
	tests := []struct {
		name    string
		ide     v1.IdeType
		typ     v1.ArtifactType
		scope   v1.ArtifactScope
		roots   Roots
		want    string
		wantErr error
	}{
		{"claude user is home", claude, rule, userGlobal, Roots{Home: "/h"}, "/h", nil},
		{"codex hook defaults to home/.codex", codex, hook, userGlobal, Roots{Home: "/h"}, "/h/.codex", nil},
		{"codex hook honours codex home", codex, hook, userGlobal, Roots{Home: "/h", CodexHome: "/c"}, "/c", nil},
		{"codex skill is home", codex, skill, userGlobal, Roots{Home: "/h", CodexHome: "/c"}, "/h", nil},
		{"codex claude_md is home", codex, claudeMd, userGlobal, Roots{Home: "/h", CodexHome: "/c"}, "/h", nil},
		{"repo local is the checkout", codex, hook, repoLocal, Roots{Home: "/h", RepoRoot: "/r"}, "/r", nil},
		{"repo root missing", claude, rule, repoLocal, Roots{Home: "/h"}, "", ErrRootUnknown},
		{"home missing", claude, rule, userGlobal, Roots{}, "", ErrRootUnknown},
		{"unspecified scope", claude, rule, noScope, Roots{Home: "/h"}, "", ErrScopeNotSupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := InstallRoot(tt.ide, tt.typ, tt.scope, tt.roots)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("root = %q, want %q", got, tt.want)
			}
		})
	}
}
