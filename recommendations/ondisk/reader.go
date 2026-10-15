// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// Package ondisk is the zero-setup DETECTED entry point: it walks a harness ON DISK (a repo root, or
// a user-global harness directory), classifies every known harness surface file per tool, reads the
// bodies, fans a Claude Code settings.json into its synthetic PERMISSIONS + HOOK children, and records
// the full repo file-path set the path lenses need. The result is seeded into the harness-inventory
// tables so the config-only DETECTED detectors (the Conflict lens, Instruction-Intent, and the
// permission inventory) run against a real committed harness with NO session/usage data upstream.
//
// The reader is pure disk I/O: it imports the common proto enums and the vendored settings parser, but
// no DB and no engine. Scan (scan.go) composes it with the engine.
//
// Scope coverage (v1): the on-disk path wires only the REPO_COMMITTED scope — a repository root's
// committed, org-canonical harness. A USER_GLOBAL (or REPO_LOCAL personal) harness directory can be read
// and seeded under a different scope, but the lenses that act on personal harness (personal / never-used,
// personal-shadows-committed) need per-person usage data that a disk walk does not carry, so they produce
// nothing on disk today. This is a stated v1 non-goal, not a silent gap: Scan reports the skipped
// families in Result.Coverage.
package ondisk

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/settings"
)

// maxBodyBytes caps a single harness file body read into memory. A file past the cap is recorded as a
// repo path (so the path lenses still see it) but not read as a harness body — a crafted or accidental
// giant instruction file cannot exhaust memory.
const maxBodyBytes = 4 << 20 // 4 MiB

// Artifact is one classified on-disk harness surface, carrying its body. SourcePath is the
// repo-relative path (the identity the detectors key on — the rule-ledger basename, dup-id "also
// defined in <path>", etc.); for a settings.json child it is the vendored parser's suffixed child path.
type Artifact struct {
	Type       commonv1.HarnessArtifactType
	IdeType    commonv1.IdeType
	Scope      commonv1.HarnessArtifactScope
	Name       string
	SourcePath string
	Body       []byte
}

// Inventory is the classified harness surface plus the full file-path set of one scanned root. It is
// the on-disk twin of the in-memory aggregate the engine builds from the monorepo's VCS ingest.
type Inventory struct {
	// Artifacts are the classified harness files (and the settings.json fan-out), one per detectable
	// surface.
	Artifacts []Artifact
	// RepoPaths is every regular file in the tree (repo-relative, forward-slashed), the committed
	// file-path set the dead-glob and broken-path lenses resolve against.
	RepoPaths []string
	// MCPServerNames are the MCP servers declared on disk (.mcp.json / .cursor/mcp.json). Collected for
	// completeness; the per-repo DETECTED path does not consume them (the org MCP lens reads them in the
	// org scan, which has no on-disk wiring in v1).
	MCPServerNames []string
	// SettingsCapped is true when a settings.json fan-out hit the vendored parser's child cap.
	SettingsCapped bool
}

// Read walks root and returns the classified harness inventory. scope is stamped on every classified
// artifact: HARNESS_ARTIFACT_SCOPE_REPO_COMMITTED for a repository root (the committed, org-canonical
// harness the per-repo detectors read), or a USER_GLOBAL/REPO_LOCAL scope for a personal harness
// directory. The configured root and its ancestors are trusted. Descendant traversal and reads
// stay inside that opened root; symlinks and special files are skipped. The .git directory is
// skipped. A file that cannot be read is skipped (never fatal) so a
// permission-denied file in a large tree does not abort the scan.
//
// When root is a git work tree, git-ignored untracked files and directories are skipped, so a .gitignore'd
// path (a node_modules/ tree, a build output) neither inflates RepoPaths — the file-path set the dead-glob
// and broken-path lenses resolve against — nor is classified as a harness artifact. A rule whose scoping
// glob matches only an ignored file is then correctly reported as a dead glob. When root is not a git
// checkout (or git is unavailable), there is no work tree for git to resolve ignores against, so the
// full tree is walked unchanged: every file — including any a .gitignore in the directory would list —
// is counted and classified. Ignore-awareness is a git-checkout feature by design; point the scan at a
// checkout to get it.
func Read(root string, scope commonv1.HarnessArtifactScope) (*Inventory, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat harness dir %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("harness dir %q is not a directory", root)
	}

	anchor, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("opening harness root: %w", err)
	}
	defer func() { _ = anchor.Close() }()

	inv := &Inventory{}
	mcpNames := map[string]struct{}{}
	ignoredFiles, ignoredDirs := gitIgnoredEntries(root)

	walkErr := fs.WalkDir(anchor.FS(), ".", func(rel string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return fs.SkipDir
			}
			if _, ignored := ignoredDirs[rel]; ignored {
				return fs.SkipDir
			}
			return nil
		}
		if _, ignored := ignoredFiles[rel]; ignored {
			// A git-ignored untracked file is not part of the committed repo, so it must not enter the
			// file-path set nor be classified as a harness artifact.
			return nil
		}
		// Symlinks and special files are not committed file bodies. The anchored
		// open below also checks the descriptor, since a regular entry can change.
		if !entry.Type().IsRegular() {
			return nil
		}
		inv.RepoPaths = append(inv.RepoPaths, rel)

		// Collect declared MCP servers (not an artifact type in this engine — kept for completeness).
		if rel == ".mcp.json" || rel == ".cursor/mcp.json" {
			if body, ok := readBody(anchor, rel); ok {
				for _, name := range mcpServerNames(body) {
					mcpNames[name] = struct{}{}
				}
			}
			return nil
		}

		classified, capped, classifyErr := classify(anchor, rel, scope)
		if classifyErr != nil {
			return classifyErr
		}
		if capped {
			inv.SettingsCapped = true
		}
		inv.Artifacts = append(inv.Artifacts, classified...)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walking harness dir %q: %w", root, walkErr)
	}

	sort.Strings(inv.RepoPaths)
	for name := range mcpNames {
		inv.MCPServerNames = append(inv.MCPServerNames, name)
	}
	sort.Strings(inv.MCPServerNames)
	return inv, nil
}

// classify maps one repo-relative path to zero or more artifacts, per the supported-tool surface map.
// A .claude/settings.json is fanned into its synthetic children via the vendored parser; every other
// matched surface yields exactly one artifact carrying its own body. capped reports a settings cap hit.
func classify(anchor *os.Root, rel string, scope commonv1.HarnessArtifactScope) (artifacts []Artifact, capped bool, err error) {
	base := path.Base(rel)

	// Claude Code settings.json — fan into PERMISSIONS + per-hook HOOK children sharing the carrier body.
	if settings.IsSettingsCarrier(commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, rel) {
		return settingsChildren(anchor, rel, scope)
	}

	type rule struct {
		match func(string, string) bool
		ide   commonv1.IdeType
		typ   commonv1.HarnessArtifactType
	}
	rules := []rule{
		// Claude Code.
		{func(r, b string) bool { return b == "CLAUDE.md" }, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD},
		{func(r, b string) bool { return under(r, ".claude/rules/") && md(b) }, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE},
		{func(r, b string) bool { return under(r, ".claude/skills/") && b == "SKILL.md" }, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL},
		{func(r, b string) bool { return under(r, ".claude/commands/") && md(b) }, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND},
		{func(r, b string) bool { return under(r, ".claude/agents/") && md(b) }, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT},
		//.
		{func(r, b string) bool { return b == "AGENTS.md" }, commonv1.IdeType_IDE_TYPE_CODEX, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENTS_MD},
		{func(r, b string) bool { return under(r, ".codex/prompts/") }, commonv1.IdeType_IDE_TYPE_CODEX, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND},
		{func(r, b string) bool { return under(r, ".agents/skills/") && b == "SKILL.md" }, commonv1.IdeType_IDE_TYPE_CODEX, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL},
		{func(r, b string) bool { return under(r, ".agents/agents/") && md(b) }, commonv1.IdeType_IDE_TYPE_CODEX, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT},
		// Cursor.
		{func(r, b string) bool { return under(r, ".cursor/rules/") && strings.HasSuffix(b, ".mdc") }, commonv1.IdeType_IDE_TYPE_CURSOR, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE},
		{func(r, b string) bool { return r == ".cursorrules" }, commonv1.IdeType_IDE_TYPE_CURSOR, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE},
		// Kiro.
		{func(r, b string) bool { return under(r, ".kiro/steering/") && md(b) }, commonv1.IdeType_IDE_TYPE_KIRO, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE},
		{func(r, b string) bool { return under(r, ".kiro/hooks/") }, commonv1.IdeType_IDE_TYPE_KIRO, commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_HOOK},
	}

	for _, candidate := range rules {
		if !candidate.match(rel, base) {
			continue
		}
		body, ok := readBody(anchor, rel)
		if !ok {
			return nil, false, nil
		}
		return []Artifact{{
			Type:       candidate.typ,
			IdeType:    candidate.ide,
			Scope:      scope,
			Name:       rel,
			SourcePath: rel,
			Body:       body,
		}}, false, nil
	}
	return nil, false, nil
}

// settingsChildren reads a Claude Code settings.json and fans it, via the vendored parser, into the
// synthetic PERMISSIONS + per-hook HOOK artifacts the detectors evaluate. Every child shares the carrier
// body (so the permission-inventory lens sees the same settings.json it parsed). A malformed body is
// non-fatal: it yields no children (the scan degrades to fewer findings, never an error).
func settingsChildren(anchor *os.Root, rel string, scope commonv1.HarnessArtifactScope) ([]Artifact, bool, error) {
	body, ok := readBody(anchor, rel)
	if !ok {
		return nil, false, nil
	}
	children, capped, parseErr := settings.ParseSettingsArtifacts(body)
	if parseErr != nil {
		// A bad committed settings file must not fail the scan — derive no children.
		return nil, false, nil
	}
	artifacts := make([]Artifact, 0, len(children))
	for _, child := range children {
		artifacts = append(artifacts, Artifact{
			Type:       child.ArtifactType,
			IdeType:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			Scope:      scope,
			Name:       child.Name,
			SourcePath: child.SourcePath,
			Body:       body,
		})
	}
	return artifacts, capped, nil
}

// mcpServerNames reads the declared MCP server names from an .mcp.json / .cursor/mcp.json body (the
// standard `{"mcpServers": {"<name>": {...}}}` shape). A malformed body yields none.
func mcpServerNames(body []byte) []string {
	var parsed struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	names := make([]string, 0, len(parsed.MCPServers))
	for name := range parsed.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// readBody reads a file body, returning ok=false for an unreadable or over-cap file (skipped, never
// fatal).
func readBody(anchor *os.Root, name string) ([]byte, bool) {
	// Nonblocking open prevents a swapped-in FIFO from hanging before Stat.
	// os.Root contains every pathname lookup even if descendants change.
	f, err := anchor.OpenFile(name, os.O_RDONLY|bodyOpenFlags, 0)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBodyBytes {
		return nil, false
	}
	return readBoundedBody(f)
}

func readBoundedBody(r io.Reader) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		return nil, false
	}
	return body, true
}

// gitIgnoredEntries returns the git-ignored UNTRACKED files and directories under root, each keyed by its
// slash-relative path, so the walk can skip them. It runs `git ls-files --others --ignored` with
// `--directory`, which collapses a wholly-ignored directory to a single `dir/` entry (listed in dirs with
// the trailing slash stripped) and lists every other ignored file in files. `--others` lists only
// untracked paths, so a TRACKED file is never returned even if a .gitignore pattern would match it —
// correct, because a tracked file is part of the repo regardless of .gitignore.
//
// When root is not a git work tree, git is not installed, or the command fails for any reason, it returns
// empty sets: the caller then walks the whole tree exactly as before gitignore-awareness. A git failure
// must never break the scan.
func gitIgnoredEntries(root string) (files, dirs map[string]struct{}) {
	files = map[string]struct{}{}
	dirs = map[string]struct{}{}

	cmd := exec.Command("git", "-C", root, "-c", "core.quotePath=false",
		"ls-files", "--others", "--ignored", "--exclude-standard", "--directory")
	out, cmdErr := cmd.Output()
	if cmdErr != nil {
		return files, dirs
	}
	for _, line := range strings.Split(string(out), "\n") {
		// Trim only the line terminator, not the path itself — a name may legitimately contain spaces.
		entry := strings.TrimRight(line, "\r")
		if entry == "" {
			continue
		}
		if strings.HasSuffix(entry, "/") {
			dirs[strings.TrimSuffix(entry, "/")] = struct{}{}
			continue
		}
		files[entry] = struct{}{}
	}
	return files, dirs
}

// under reports whether rel is a path inside dirPrefix (which ends in "/").
func under(rel, dirPrefix string) bool { return strings.HasPrefix(rel, dirPrefix) }

// md reports whether base is a Markdown file.
func md(base string) bool { return strings.HasSuffix(base, ".md") }
