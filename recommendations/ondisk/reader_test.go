// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package ondisk

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
)

// TestRead_ClassifiesPerToolSurfaces proves the reader maps each supported on-disk harness surface to
// its artifact type and IDE, fans .claude/settings.json into PERMISSIONS + HOOK children via the
// vendored parser, records every file in the repo path set, and collects declared MCP servers. It is a
// pure disk read — no DB — so it always runs.
func TestRead_ClassifiesPerToolSurfaces(t *testing.T) {
	inv, err := Read("testdata/sample-repo", commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	byType := map[commonv1.HarnessArtifactType]int{}
	bySourcePath := map[string]Artifact{}
	for _, a := range inv.Artifacts {
		byType[a.Type]++
		bySourcePath[a.SourcePath] = a
	}

	wantType := map[commonv1.HarnessArtifactType]int{
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD:   1, // CLAUDE.md
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENTS_MD:   1, // AGENTS.md (Codex)
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE:        2, // .claude/rules/{a,b}.md
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL:       1, // .claude/skills/demo/SKILL.md
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND:     1, // .claude/commands/run.md
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT:       1, // .claude/agents/helper.md
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_PERMISSIONS: 1, // settings.json permissions block
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_HOOK:        1, // settings.json PreToolUse hook
	}
	for typ, want := range wantType {
		if got := byType[typ]; got != want {
			t.Errorf("artifact type %s: got %d, want %d", typ, got, want)
		}
	}

	// The settings.json carrier itself is never an artifact — only its suffixed children are.
	if _, carrier := bySourcePath[".claude/settings.json"]; carrier {
		t.Error("the .claude/settings.json carrier was persisted as its own artifact; it must only fan into children")
	}
	// The PERMISSIONS child carries the full settings.json body so the permission-inventory lens parses it.
	permissions := bySourcePath[".claude/settings.json#permissions"]
	if permissions.Type != commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_PERMISSIONS {
		t.Fatalf("no PERMISSIONS child at .claude/settings.json#permissions; got %+v", permissions)
	}
	if permissions.IdeType != commonv1.IdeType_IDE_TYPE_CLAUDE_CODE {
		t.Errorf("PERMISSIONS child ide = %s, want CLAUDE_CODE", permissions.IdeType)
	}
	if len(permissions.Body) == 0 {
		t.Error("PERMISSIONS child carries no body (must share the settings.json carrier body)")
	}

	// The AGENTS.md is stamped with the IDE, not Claude Code.
	if agents, ok := bySourcePath["AGENTS.md"]; !ok || agents.IdeType != commonv1.IdeType_IDE_TYPE_CODEX {
		t.Errorf("AGENTS.md classification = %+v, want IDE_TYPE_CODEX", agents)
	}

	// Every regular file is in the repo path set (the path lenses' committed-file universe).
	wantPaths := []string{
		"CLAUDE.md", "AGENTS.md", "README.md", ".mcp.json", "src/main.go",
		".claude/rules/a.md", ".claude/rules/b.md", ".claude/settings.json",
	}
	pathSet := map[string]struct{}{}
	for _, p := range inv.RepoPaths {
		pathSet[p] = struct{}{}
	}
	for _, p := range wantPaths {
		if _, ok := pathSet[p]; !ok {
			t.Errorf("repo path %q missing from RepoPaths", p)
		}
	}

	if len(inv.MCPServerNames) != 1 || inv.MCPServerNames[0] != "filesystem" {
		t.Errorf("MCPServerNames = %v, want [filesystem]", inv.MCPServerNames)
	}
}

// TestRead_RejectsNonDir guards the clear error on a bad target.
func TestRead_RejectsNonDir(t *testing.T) {
	if _, err := Read("testdata/sample-repo/README.md", commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED); err == nil {
		t.Fatal("Read on a file (not a dir) returned nil error")
	}
}

// TestReadBody_CapAndErrors is the direct guard on the body-read cap and its fail-soft contract: a file
// at or under maxBodyBytes reads, a file past the cap is NOT read (ok=false), and a missing file is not
// read. It kills the disabling mutation of dropping the `info.Size() > maxBodyBytes` guard — without the
// guard the over-cap file would read ok=true and this test fails.
func TestReadBody_CapAndErrors(t *testing.T) {
	dir := t.TempDir()
	anchor, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = anchor.Close() }()

	underCap := filepath.Join(dir, "under.md")
	if err := os.WriteFile(underCap, []byte("small body"), 0o600); err != nil {
		t.Fatalf("writing under-cap file: %v", err)
	}
	if body, ok := readBody(anchor, "under.md"); !ok || string(body) != "small body" {
		t.Errorf("readBody(under-cap) = (%q, %v), want (\"small body\", true)", body, ok)
	}

	overCap := filepath.Join(dir, "over.md")
	if err := os.WriteFile(overCap, make([]byte, maxBodyBytes+1), 0o600); err != nil {
		t.Fatalf("writing over-cap file: %v", err)
	}
	if body, ok := readBody(anchor, "over.md"); ok {
		t.Errorf("readBody(over-cap) ok = true (read %d bytes), want false — the %d-byte cap must not be exceeded", len(body), maxBodyBytes)
	}

	if _, ok := readBody(anchor, "does-not-exist.md"); ok {
		t.Error("readBody(missing file) ok = true, want false")
	}
}

// TestRead_OverCapHarnessFileRecordedButNotClassified proves an over-cap harness surface (a CLAUDE.md
// past the body cap) is still recorded in RepoPaths — so the dead-glob / broken-path lenses still see
// the file — but is NOT classified as an artifact, because its body is never read. This is the
// end-to-end face of the cap: a crafted giant instruction file cannot be slurped into memory, yet it
// does not vanish from the committed file-path universe.
func TestRead_OverCapHarnessFileRecordedButNotClassified(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), make([]byte, maxBodyBytes+1), 0o600); err != nil {
		t.Fatalf("writing over-cap CLAUDE.md: %v", err)
	}

	inv, err := Read(dir, commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	var inPaths bool
	for _, p := range inv.RepoPaths {
		if p == "CLAUDE.md" {
			inPaths = true
		}
	}
	if !inPaths {
		t.Errorf("over-cap CLAUDE.md missing from RepoPaths %v; the path lenses must still see it", inv.RepoPaths)
	}
	for _, a := range inv.Artifacts {
		if a.SourcePath == "CLAUDE.md" {
			t.Errorf("over-cap CLAUDE.md was classified as an artifact (%d-byte body); its body must not be read past the %d-byte cap", len(a.Body), maxBodyBytes)
		}
	}
}

// TestRead_UnreadableFileSkippedNotFatal proves a committed harness file that cannot be read (mode 000)
// is skipped — recorded in RepoPaths but not classified — and never aborts the walk. A
// permission-denied file in a large tree must degrade the scan, not fail it.
func TestRead_UnreadableFileSkippedNotFatal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses file permissions; the unreadable-file path cannot be exercised")
	}
	dir := t.TempDir()
	unreadable := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(unreadable, []byte("instructions"), 0o000); err != nil {
		t.Fatalf("writing unreadable CLAUDE.md: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) }) // let t.TempDir cleanup remove it

	inv, err := Read(dir, commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED)
	if err != nil {
		t.Fatalf("Read must not fail on an unreadable file: %v", err)
	}

	var inPaths bool
	for _, p := range inv.RepoPaths {
		if p == "CLAUDE.md" {
			inPaths = true
		}
	}
	if !inPaths {
		t.Errorf("unreadable CLAUDE.md missing from RepoPaths %v", inv.RepoPaths)
	}
	for _, a := range inv.Artifacts {
		if a.SourcePath == "CLAUDE.md" {
			t.Error("unreadable CLAUDE.md was classified as an artifact; a body that cannot be read must be skipped")
		}
	}
}

// TestRead_SkipsGitIgnoredFiles reproduces the ticket: in a git work tree, a .gitignore'd path
// (node_modules/) must not enter RepoPaths — the committed file-path universe the dead-glob and
// broken-path lenses resolve against — nor be classified as a harness artifact. Before the fix the walk
// added every file on disk, so a rule whose scoping glob matched ONLY an ignored file was not reported as
// a dead glob (the glob "matched" the ignored file). After the fix the ignored file is gone from the set,
// so such a glob matches nothing. The rule body here carries `paths: node_modules/**`, which matches only
// the ignored file; asserting that file is absent from RepoPaths is exactly what makes the dead-glob lens
// fire (see the scan-level TestScan_DeadGlobFiresForGitIgnoredOnlyMatch). It also plants an ignored harness
// surface (node_modules/dep/.claude/rules/dep.md) to prove an ignored file is not classified. A TRACKED
// file matching .gitignore is still kept, because `--others` lists only untracked paths.
func TestRead_SkipsGitIgnoredFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; the gitignore-aware walk cannot be exercised")
	}
	dir := t.TempDir()

	writeFile(t, dir, ".gitignore", "node_modules/\n*.log\n")
	writeFile(t, dir, ".claude/rules/scoped.md", "---\npaths: node_modules/**\n---\nrule body\n")
	writeFile(t, dir, "src/main.go", "package main\n")
	writeFile(t, dir, "node_modules/dep/index.js", "module.exports = {}\n")
	writeFile(t, dir, "node_modules/dep/.claude/rules/dep.md", "---\npaths: '**/*.go'\n---\nignored rule\n")
	writeFile(t, dir, "build.log", "noise\n")  // untracked + matches *.log → ignored → skipped.
	writeFile(t, dir, "tracked.log", "kept\n") // matches *.log but tracked → must be kept.

	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	// Force-add the .gitignore-matching file so it is TRACKED: `--others` then never lists it, which is
	// exactly why a tracked file matching .gitignore must still be kept in RepoPaths.
	runGit(t, dir, "add", "-f", "tracked.log")

	inv, err := Read(dir, commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	pathSet := map[string]struct{}{}
	for _, p := range inv.RepoPaths {
		pathSet[p] = struct{}{}
	}

	// (a) The ignored paths are excluded from the file-path set.
	for _, ignored := range []string{
		"node_modules/dep/index.js",
		"node_modules/dep/.claude/rules/dep.md",
		"build.log",
	} {
		if _, ok := pathSet[ignored]; ok {
			t.Errorf("git-ignored path %q is in RepoPaths; it must be excluded so the dead-glob lens counts it as a non-existent file", ignored)
		}
	}

	// The non-ignored files are kept, including a TRACKED file that a pattern would otherwise ignore.
	for _, kept := range []string{".gitignore", ".claude/rules/scoped.md", "src/main.go", "tracked.log"} {
		if _, ok := pathSet[kept]; !ok {
			t.Errorf("path %q missing from RepoPaths %v; only git-ignored untracked files may be dropped", kept, inv.RepoPaths)
		}
	}

	// (b) The ignored harness surface under node_modules/ is not classified as an artifact.
	for _, a := range inv.Artifacts {
		if a.SourcePath == "node_modules/dep/.claude/rules/dep.md" {
			t.Errorf("a git-ignored harness file was classified as an artifact (%+v); ignored files must not be scanned", a)
		}
	}
}

// TestRead_NonGitDirWalksEverything pins the fallback: when root is NOT a git checkout, the full tree is
// walked and nothing is filtered — a node_modules/ file is still recorded. This guards against the
// gitignore-aware walk silently dropping files in a plain (non-git) harness directory.
func TestRead_NonGitDirWalksEverything(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".gitignore", "node_modules/\n")
	writeFile(t, dir, "node_modules/dep/index.js", "module.exports = {}\n")
	writeFile(t, dir, "CLAUDE.md", "root instructions\n")

	inv, err := Read(dir, commonv1.HarnessArtifactScope_HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	pathSet := map[string]struct{}{}
	for _, p := range inv.RepoPaths {
		pathSet[p] = struct{}{}
	}
	// With no .git, the .gitignore is inert: every file is walked, including node_modules/.
	for _, want := range []string{".gitignore", "node_modules/dep/index.js", "CLAUDE.md"} {
		if _, ok := pathSet[want]; !ok {
			t.Errorf("path %q missing from RepoPaths %v; a non-git dir must walk the whole tree (no gitignore filtering)", want, inv.RepoPaths)
		}
	}
}

// writeFile writes rel (slash-relative to dir), creating parent directories, failing the test on error.
func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir for %q: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %q: %v", rel, err)
	}
}

// runGit runs a git command in dir, failing the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
