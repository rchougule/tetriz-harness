package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/tetriz-ai/tetriz-harness/gen/go/harnessctl/v1"
)

var update = flag.Bool("update", false, "rewrite testdata/install-paths.json from the current Go behaviour")

const fixturePath = "../../testdata/install-paths.json"

// fixtureCase is one row of the shared conformance fixtures. Enum fields carry
// the proto value names, which are also the JSON wire names every client sees.
type fixtureCase struct {
	Ide       string `json:"ide"`
	Scope     string `json:"scope"`
	Type      string `json:"type"`
	Slug      string `json:"slug"`
	Home      string `json:"home"`
	CodexHome string `json:"codex_home,omitempty"`
	RepoRoot  string `json:"repo_root,omitempty"`

	// Error is set instead of the paths when the combination is refused.
	Error        string `json:"error,omitempty"`
	InstallPath  string `json:"install_path,omitempty"`
	AbsolutePath string `json:"absolute_path,omitempty"`

	HookCommand string `json:"hook_command,omitempty"`
	// HookCommandTarget is the file the hook command runs once its variables
	// expand; it should equal AbsolutePath.
	HookCommandTarget string `json:"hook_command_target,omitempty"`

	// TSAbsolutePath is set only where the TypeScript agent's root rule places
	// the file somewhere other than AbsolutePath.
	TSAbsolutePath string `json:"ts_absolute_path,omitempty"`
}

type fixtureFile struct {
	Description string        `json:"description"`
	Cases       []fixtureCase `json:"cases"`
}

var errNames = []struct {
	err  error
	name string
}{
	{ErrScopeNotSupported, "scope_not_supported"},
	{ErrIdeNotSupported, "ide_not_supported"},
	{ErrTypeNotEnforceable, "type_not_enforceable"},
	{ErrInvalidSlug, "invalid_slug"},
}

func errName(t *testing.T, err error) string {
	t.Helper()
	for _, e := range errNames {
		if errors.Is(err, e.err) {
			return e.name
		}
	}
	t.Fatalf("unnamed error %v", err)
	return ""
}

// tsInstallRoot is the root rule the TypeScript agent applies at user scope:
// every Codex path resolves against the Codex home except one under .agents/.
// Kept here so the fixtures record exactly where it and InstallRoot disagree.
func tsInstallRoot(ide v1.IdeType, scope v1.ArtifactScope, installPath string, roots Roots) string {
	if scope == v1.ArtifactScope_ARTIFACT_SCOPE_REPO_LOCAL {
		return roots.RepoRoot
	}
	if strings.HasPrefix(installPath, ".agents/") {
		return roots.Home
	}
	if ide == v1.IdeType_IDE_TYPE_CODEX {
		if roots.CodexHome != "" {
			return roots.CodexHome
		}
		return path.Join(roots.Home, ".codex")
	}
	return roots.Home
}

// commandTarget expands the variables a derived hook command uses.
func commandTarget(cmd string, roots Roots) string {
	codexHome := roots.CodexHome
	if codexHome == "" {
		codexHome = path.Join(roots.Home, ".codex")
	}
	r := strings.NewReplacer(
		`"${CODEX_HOME:-$HOME/.codex}"`, codexHome,
		`"$HOME"`, roots.Home,
		`"$CLAUDE_PROJECT_DIR"`, roots.RepoRoot,
		`"$(git rev-parse --show-toplevel)"`, roots.RepoRoot,
	)
	out := r.Replace(cmd)
	if !strings.HasPrefix(out, "/") {
		// A bare command path is run from the checkout root.
		out = path.Join(roots.RepoRoot, out)
	}
	return out
}

func buildFixtures(t *testing.T) fixtureFile {
	t.Helper()
	ides := []v1.IdeType{v1.IdeType_IDE_TYPE_CLAUDE_CODE, v1.IdeType_IDE_TYPE_CURSOR, v1.IdeType_IDE_TYPE_CODEX, v1.IdeType_IDE_TYPE_KIRO}
	types := []v1.ArtifactType{
		v1.ArtifactType_ARTIFACT_TYPE_SKILL, v1.ArtifactType_ARTIFACT_TYPE_HOOK, v1.ArtifactType_ARTIFACT_TYPE_AGENT,
		v1.ArtifactType_ARTIFACT_TYPE_RULE, v1.ArtifactType_ARTIFACT_TYPE_CLAUDE_MD, v1.ArtifactType_ARTIFACT_TYPE_AGENTS_MD,
	}
	scopes := []v1.ArtifactScope{v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL, v1.ArtifactScope_ARTIFACT_SCOPE_REPO_LOCAL}
	base := Roots{Home: "/home/dev", RepoRoot: "/work/acme/app"}
	customCodex := Roots{Home: "/home/dev", CodexHome: "/opt/codex-home", RepoRoot: "/work/acme/app"}

	var cases []fixtureCase
	add := func(ide v1.IdeType, typ v1.ArtifactType, scope v1.ArtifactScope, slug string, roots Roots) {
		c := fixtureCase{
			Ide: ide.String(), Scope: scope.String(), Type: typ.String(), Slug: slug,
			Home: roots.Home, CodexHome: roots.CodexHome,
		}
		if scope == v1.ArtifactScope_ARTIFACT_SCOPE_REPO_LOCAL {
			c.RepoRoot = roots.RepoRoot
		}
		p, err := InstallPath(ide, typ, slug, scope)
		if err != nil {
			c.Error = errName(t, err)
			cases = append(cases, c)
			return
		}
		root, err := InstallRoot(ide, typ, scope, roots)
		if err != nil {
			t.Fatalf("InstallRoot(%v, %v, %v): %v", ide, typ, scope, err)
		}
		c.InstallPath = p
		c.AbsolutePath = path.Join(root, p)
		if typ == v1.ArtifactType_ARTIFACT_TYPE_HOOK {
			cmd, err := HookCommand(ide, scope, slug)
			if err != nil {
				t.Fatalf("HookCommand(%v, %v): %v", ide, scope, err)
			}
			c.HookCommand = cmd
			if cmd != "" {
				c.HookCommandTarget = commandTarget(cmd, roots)
			}
		}
		if ts := path.Join(tsInstallRoot(ide, scope, p, roots), p); ts != c.AbsolutePath {
			c.TSAbsolutePath = ts
		}
		cases = append(cases, c)
	}
	for _, ide := range ides {
		for _, typ := range types {
			for _, scope := range scopes {
				add(ide, typ, scope, "code-review", base)
			}
		}
	}
	// Codex user-scope rows again with a relocated Codex home.
	for _, typ := range types {
		add(v1.IdeType_IDE_TYPE_CODEX, typ, v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL, "code-review", customCodex)
	}
	// Refusals every implementation must share.
	add(v1.IdeType_IDE_TYPE_UNSPECIFIED, v1.ArtifactType_ARTIFACT_TYPE_SKILL, v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL, "code-review", base)
	add(v1.IdeType_IDE_TYPE_CLAUDE_CODE, v1.ArtifactType_ARTIFACT_TYPE_SKILL, v1.ArtifactScope_ARTIFACT_SCOPE_UNSPECIFIED, "code-review", base)
	add(v1.IdeType_IDE_TYPE_CLAUDE_CODE, v1.ArtifactType_ARTIFACT_TYPE_UNSPECIFIED, v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL, "code-review", base)
	add(v1.IdeType_IDE_TYPE_CLAUDE_CODE, v1.ArtifactType_ARTIFACT_TYPE_SKILL, v1.ArtifactScope_ARTIFACT_SCOPE_USER_GLOBAL, "../escape", base)
	add(v1.IdeType_IDE_TYPE_CLAUDE_CODE, v1.ArtifactType_ARTIFACT_TYPE_HOOK, v1.ArtifactScope_ARTIFACT_SCOPE_REPO_LOCAL, "Upper", base)

	return fixtureFile{
		Description: "Install-path conformance fixtures, generated from pkg/spec. install_path is relative to the scope root; " +
			"absolute_path joins it to that root. ts_absolute_path marks a row where the TypeScript agent's root rule disagrees.",
		Cases: cases,
	}
}

func encode(t *testing.T, f fixtureFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f); err != nil {
		t.Fatalf("encoding fixtures: %v", err)
	}
	return buf.Bytes()
}

// TestConformanceFixtures fails when pkg/spec and the committed fixtures drift.
// Run with -update after an intended change and review the diff.
func TestConformanceFixtures(t *testing.T) {
	want := encode(t, buildFixtures(t))
	if *update {
		if err := os.WriteFile(filepath.FromSlash(fixturePath), want, 0o644); err != nil {
			t.Fatalf("writing fixtures: %v", err)
		}
		return
	}
	got, err := os.ReadFile(filepath.FromSlash(fixturePath))
	if err != nil {
		t.Fatalf("reading fixtures (run go test ./pkg/spec -update): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("testdata/install-paths.json is stale; run go test ./pkg/spec -run TestConformanceFixtures -update and review the diff")
	}
}

// TestConformanceFixturesDecode asserts the committed file row by row, so a
// hand edit to the JSON is caught with the row that broke.
func TestConformanceFixturesDecode(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(fixturePath))
	if err != nil {
		t.Fatalf("reading fixtures: %v", err)
	}
	var f fixtureFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decoding fixtures: %v", err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("no fixture cases")
	}
	for _, c := range f.Cases {
		name := strings.Join([]string{c.Ide, c.Type, c.Scope, c.Slug, c.CodexHome}, "/")
		t.Run(name, func(t *testing.T) {
			ide := v1.IdeType(v1.IdeType_value[c.Ide])
			typ := v1.ArtifactType(v1.ArtifactType_value[c.Type])
			scope := v1.ArtifactScope(v1.ArtifactScope_value[c.Scope])
			p, err := InstallPath(ide, typ, c.Slug, scope)
			if c.Error != "" {
				if err == nil || errName(t, err) != c.Error {
					t.Fatalf("err = %v, want %s", err, c.Error)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p != c.InstallPath {
				t.Fatalf("install_path = %q, want %q", p, c.InstallPath)
			}
			root, err := InstallRoot(ide, typ, scope, Roots{Home: c.Home, CodexHome: c.CodexHome, RepoRoot: c.RepoRoot})
			if err != nil {
				t.Fatalf("InstallRoot: %v", err)
			}
			if abs := path.Join(root, p); abs != c.AbsolutePath {
				t.Fatalf("absolute_path = %q, want %q", abs, c.AbsolutePath)
			}
			if typ == v1.ArtifactType_ARTIFACT_TYPE_HOOK {
				cmd, err := HookCommand(ide, scope, c.Slug)
				if err != nil || cmd != c.HookCommand {
					t.Fatalf("hook_command = %q (%v), want %q", cmd, err, c.HookCommand)
				}
			}
		})
	}
}

// TestHookCommandRunsPlacedFile asserts every derived hook command, once its
// variables expand, runs the exact file the placement wrote.
func TestHookCommandRunsPlacedFile(t *testing.T) {
	for _, c := range buildFixtures(t).Cases {
		if c.HookCommandTarget == "" {
			continue
		}
		if c.HookCommandTarget != c.AbsolutePath {
			t.Errorf("%s/%s codex_home=%q: command runs %q, file placed at %q", c.Ide, c.Scope, c.CodexHome, c.HookCommandTarget, c.AbsolutePath)
		}
	}
}
