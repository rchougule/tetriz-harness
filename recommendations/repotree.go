// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/settings"
)

// RepoTree compaction (harness recommendation spec#50). The connector writes a
// raw, unstripped, FLAT recursive git tree to the content store (repo-trees/{org}/.../{sha}.json);
// EncodeRepoTree turns it into the compact, paths-only, directory-grouped
// RepoTree the derivation LLM cites file paths from. All token-minimization
// lives here — the connector does none.

// TruncationReason labels why a RepoTree was emitted incomplete, for the
// recommendations_derivation_repo_tree_truncated_total{reason} warn metric (design notes
// #49/#50). Empty when the tree is complete.
type TruncationReason string

const (
	// TruncationNone means the encoded tree carries every citable path.
	TruncationNone TruncationReason = ""
	// TruncationVendor means the vendor truncated the tree at fetch time (the
	// blob arrived marked truncated); the engine uses it anyway and warns.
	TruncationVendor TruncationReason = "vendor"
	// TruncationTokenCap means the encoding exceeded RepoTreeMaxTokens and was
	// cut — an outlier repo, the edge backstop the cap exists for.
	TruncationTokenCap TruncationReason = "token_cap"
)

// tokensPerChar estimates LLM tokens as chars/4 — the standard rough heuristic.
// The cap is an edge backstop, not a precise budget (token figures are working
// estimates, spec M8), so a coarse estimate is sufficient to catch an outlier.
const charsPerToken = 4

// destKind classifies what an AI-config file is a destination for: a RULE (an agent
// rules-instruction file), a SKILL (a per-skill/command/prompt file), an AGENT (a
// per-subagent definition file), or a HOOK (a lifecycle/file-event automation config).
type destKind int

const (
	destRule destKind = iota
	destSkill
	destAgent
	destHook
)

// artifactConvention is one tool's file/dir convention: which tool owns it and which
// artifact kind lives there.
type artifactConvention struct {
	tool commonv1.IdeType
	kind destKind
}

// artifactDir is a directory-based artifact convention plus how a NEW artifact file is
// named within it — so a resolved fallback destination names a file the tool actually
// loads. A new artifact is either a package ("<dir>/<slug>/SKILL.md", skillPackage — the
// Claude Code and skill layout) or a single file ("<dir>/<slug>" + the canonical ext).
// exts are the extensions the tool loads in this dir (Cursor .mdc, Copilot instructions
// .instructions.md, rules .md/.mdc/.rules); the FIRST is canonical — used when naming
// a new file. exts is nil for a skillPackage dir (layout is <slug>/SKILL.md) and for a dir
// that accepts any extension.
type artifactDir struct {
	prefix       string
	conv         artifactConvention
	exts         []string
	skillPackage bool
}

// canonicalExt is the extension used when NAMING a new file in this dir (the first accepted
// extension), "" when the dir accepts any extension or is a skillPackage.
func (a artifactDir) canonicalExt() string {
	if len(a.exts) == 0 {
		return ""
	}
	return a.exts[0]
}

// matchesExt reports whether filePath carries one of this dir's accepted extensions; a dir
// with no exts accepts any file (the isStrippedPath / ingest policy still filters cruft).
func (a artifactDir) matchesExt(filePath string) bool {
	if len(a.exts) == 0 {
		return true
	}
	lower := strings.ToLower(filePath)
	for _, ext := range a.exts {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// trimExt strips whichever accepted extension filePath ends with (the longest match, so a
// compound ".instructions.md" wins over ".md"); returns the input unchanged if none match.
func (a artifactDir) trimExt(filePath string) string {
	best := ""
	for _, ext := range a.exts {
		if strings.HasSuffix(strings.ToLower(filePath), ext) && len(ext) > len(best) {
			best = ext
		}
	}
	// best matched case-insensitively; trim by length so a mixed-case extension (Foo.MD) is
	// still stripped rather than left on the name.
	return filePath[:len(filePath)-len(best)]
}

// artifactFileBasenames, artifactExactPaths, and artifactDirPrefixes are the SINGLE
// SOURCE OF TRUTH for AI-artifact-config detection: every consumer (isRuleFile /
// isRuleDestination / isSkillDestination / RuleFileTool / the fallback namer) derives
// from them, so a tool is added in exactly ONE place and its detection, destination-kind,
// label, and new-file layout can never drift apart. Only tools present in common.v1.IdeType
// appear here — a recognized file must map to a known tool (TestArtifactConventionsMapToKnownTool
// guards it). Basenames match the leaf name in any directory (a nested CLAUDE.md is detected);
// exact paths match one repo-relative path only; dir prefixes match an anchored path segment.
// Re-confirm names/extensions against each tool's live docs — they drift.
var artifactFileBasenames = map[string]artifactConvention{
	"claude.md":    {commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, destRule},
	"agents.md":    {commonv1.IdeType_IDE_TYPE_CODEX, destRule}, // cross-tool standard; labelled by its originating tool
	"gemini.md":    {commonv1.IdeType_IDE_TYPE_ANTIGRAVITY, destRule},
	".cursorrules": {commonv1.IdeType_IDE_TYPE_CURSOR, destRule},
}

// artifactExactPaths are conventions matched at a SINGLE repo-relative path, not by
// basename in any directory. GitHub Copilot loads its repository instruction file only at
// .github/copilot-instructions.md — a copilot-instructions.md elsewhere (docs/, an example
// tree) is not a Copilot config and must not be treated as one.
var artifactExactPaths = map[string]artifactConvention{
	copilotRepoWideInstructions: {commonv1.IdeType_IDE_TYPE_COPILOT, destRule},
	// Antigravity's hook config is the single .agents/hooks.json file (a merge target).
	// It shares the .agents/ root with's .agents/skills/ but is disambiguated by
	// sub-path: the anchored dir-prefix scan matches .agents/skills/ only, so this exact
	// path never collides with it (see the harness recommendation spec for the .agents/
	// disambiguation — the one anchor for the per-tool artifact conventions below).
	".agents/hooks.json": {commonv1.IdeType_IDE_TYPE_ANTIGRAVITY, destHook},
	// The other tools' hook configs that are a single SHARED file (no per-slug segment) —
	// the merge-target counterparts to Antigravity's .agents/hooks.json above. Detecting a
	// checked-in one is what lets a hook/agent-only repo resolve to its tool; note that
	// .claude/settings.json standardizes a repo on Claude Code even when it holds only
	// permissions/env and no hooks. Per-slug hook dirs (Copilot/Kiro) are dir-prefixes below.
	".claude/settings.json": {commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, destHook},
	".codex/hooks.json":     {commonv1.IdeType_IDE_TYPE_CODEX, destHook},
	".cursor/hooks.json":    {commonv1.IdeType_IDE_TYPE_CURSOR, destHook},
}

var artifactDirPrefixes = []artifactDir{
	{".cursor/rules/", artifactConvention{commonv1.IdeType_IDE_TYPE_CURSOR, destRule}, []string{".mdc", ".md"}, false}, // Cursor rules are .mdc or .md (cursorAssets.ts)
	{".claude/rules/", artifactConvention{commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, destRule}, []string{".md"}, false},    // standalone per-topic files, auto-discovered and loaded at launch (no CLAUDE.md import needed)
	{".github/instructions/", artifactConvention{commonv1.IdeType_IDE_TYPE_COPILOT, destRule}, []string{".instructions.md"}, false},
	{".kiro/steering/", artifactConvention{commonv1.IdeType_IDE_TYPE_KIRO, destRule}, []string{".md"}, false},
	// rules accept .md/.mdc (frontmatter) and .rules (plain); the desktop scanner reads
	// all three (codexAssets.ts), so VCS ingestion recognizes the same set.
	{".codex/rules/", artifactConvention{commonv1.IdeType_IDE_TYPE_CODEX, destRule}, []string{".md", ".mdc", ".rules"}, false},
	{".claude/skills/", artifactConvention{commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, destSkill}, nil, true},                // <slug>/SKILL.md package
	{".claude/commands/", artifactConvention{commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, destSkill}, []string{".md"}, false}, // single-file slash-command form
	{".cursor/commands/", artifactConvention{commonv1.IdeType_IDE_TYPE_CURSOR, destSkill}, []string{".md"}, false},      // Cursor single-file slash command
	{".agents/skills/", artifactConvention{commonv1.IdeType_IDE_TYPE_CODEX, destSkill}, nil, true},                      // <slug>/SKILL.md package
	{".cursor/skills/", artifactConvention{commonv1.IdeType_IDE_TYPE_CURSOR, destSkill}, nil, true},                     // Cursor <slug>/SKILL.md package (cursorAssets.ts)
	{".kiro/skills/", artifactConvention{commonv1.IdeType_IDE_TYPE_KIRO, destSkill}, nil, true},                         // Kiro <slug>/SKILL.md package (IDE Agent Skills, kiro.dev/docs/skills)
	{".github/prompts/", artifactConvention{commonv1.IdeType_IDE_TYPE_COPILOT, destSkill}, []string{".prompt.md"}, false},
	// Antigravity agents live at .agents/agents/<slug>/agent.md — anchored one segment
	// deeper than's .agents/skills/, so the two never collide (the same .agents/
	// disambiguation noted on .agents/hooks.json above). The ".md" ext is nominal: a
	// destAgent dir is resolved by the
	// per-tool toolArtifactTargets table (destAgentPath), not the rule/skill fallback
	// namer (newArtifactPath), and destAgent is excluded from every rule/skill code path
	// — the entry exists only so conventionFor/RuleFileTool recognize and label a
	// checked-in Antigravity agent file.
	{".agents/agents/", artifactConvention{commonv1.IdeType_IDE_TYPE_ANTIGRAVITY, destAgent}, []string{".md"}, false},
	// The other tools' per-slug AGENT-definition dirs (one file per subagent) and per-slug
	// HOOK dirs (one manifest per hook) — the destAgent/destHook destinations from
	// toolArtifactTargets whose template carries a <slug> segment. As with Antigravity's
	// .agents/agents/ above, the ext is NOMINAL: a destAgent/destHook target is resolved by
	// toolArtifactTargets (destAgentPath/destHookPath), not the rule/skill fallback namer
	// (newArtifactPath), and both kinds are excluded from every rule/skill code path — these
	// entries exist only so conventionFor/RuleFileTool recognize and label a checked-in
	// agent/hook file. .kiro/hooks/ uses the two-part .kiro.hook ext (not in
	// strippedExtensions, so isStrippedPath keeps it).
	{".claude/agents/", artifactConvention{commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, destAgent}, []string{".md"}, false},
	{".codex/agents/", artifactConvention{commonv1.IdeType_IDE_TYPE_CODEX, destAgent}, []string{".toml", ".md"}, false}, // agents are .toml or .md (codexAssets.ts)
	{".github/agents/", artifactConvention{commonv1.IdeType_IDE_TYPE_COPILOT, destAgent}, []string{".agent.md"}, false},
	{".kiro/agents/", artifactConvention{commonv1.IdeType_IDE_TYPE_KIRO, destAgent}, []string{".md"}, false},
	{".cursor/agents/", artifactConvention{commonv1.IdeType_IDE_TYPE_CURSOR, destAgent}, []string{".md"}, false},
	{".github/hooks/", artifactConvention{commonv1.IdeType_IDE_TYPE_COPILOT, destHook}, []string{".json"}, false},
	{".kiro/hooks/", artifactConvention{commonv1.IdeType_IDE_TYPE_KIRO, destHook}, []string{".kiro.hook"}, false},
}

// conventionFor returns the artifact-config convention for a path and whether the path
// is a recognized artifact file at all. Directory prefix is checked BEFORE the exact-path
// and basename lookups: directory placement is the more specific, structural signal, so a
// file whose leaf name collides with a reserved rule basename but sits inside a skill
// directory (e.g. ".claude/skills/agents.md") is correctly a Claude Code SKILL, not a rule.
func conventionFor(filePath string) (artifactConvention, bool) {
	if entry, ok := dirEntryFor(filePath); ok {
		return entry.conv, true
	}
	lower := strings.ToLower(filePath)
	if conv, ok := artifactExactPaths[lower]; ok {
		return conv, true
	}
	if conv, ok := artifactFileBasenames[strings.ToLower(path.Base(filePath))]; ok {
		return conv, true
	}
	return artifactConvention{}, false
}

// dirEntryFor returns the full artifactDir entry a path matches by anchored directory
// prefix (incl. its new-file naming rule), and whether it did. Basenames/exact paths are
// NOT considered here — conventionFor layers those on top. Shared by conventionFor,
// matchesRuleDir, and the fallback namer so the dir-prefix scan lives in one place.
func dirEntryFor(filePath string) (artifactDir, bool) {
	lower := strings.ToLower(filePath)
	for _, entry := range artifactDirPrefixes {
		if pathHasDirPrefix(lower, entry.prefix) {
			return entry, true
		}
	}
	return artifactDir{}, false
}

// EnumeratedArtifactFile is one file of an enumerated artifact, with the git blob
// SHA that is its per-file change-detection baseline.
type EnumeratedArtifactFile struct {
	RelativePath string // relative to GroupKey for a package member ("SKILL.md", "scripts/run.sh"); the bare basename for a single-file artifact
	BlobSHA      string
	IsPrimary    bool // the anchor file (a skill's SKILL.md; or the file itself for a single-file artifact)
}

// EnumeratedArtifact is one logical harness artifact discovered in a repo tree: its identity
// plus every file it comprises; it is what the VCS work-list RPC diffs against the stored
// manifest. SourcePath is the PRIMARY file path (the identity key). GroupKey is the artifact's
// fetch/diff root and its meaning differs by shape: for a PACKAGE it is the package directory
// and each file's RelativePath is relative to it (full path = GroupKey + "/" + RelativePath);
// for a SINGLE-FILE artifact it is the file's own path (== SourcePath), its one RelativePath is
// the bare basename, and the full path is GroupKey itself. So SourcePath == GroupKey marks a
// single-file artifact — the shape kept per-file unique so a sibling's deletion never aliases
// another's fetch/diff unit.
type EnumeratedArtifact struct {
	SourcePath   string
	Name         string
	IdeType      commonv1.IdeType
	ArtifactType commonv1.HarnessArtifactType
	GroupKey     string
	Files        []EnumeratedArtifactFile
}

const skillPrimaryFile = "SKILL.md"

// artifactTypeByDirPrefix overrides the destKind→HarnessArtifactType fallback for a
// directory whose kind bucket does not match its artifact type. .claude/commands/ shares
// the destSkill bucket with .claude/skills/ (they place the same way), but a checked-in
// slash command is a COMMAND, not a skill — without this override it would classify as
// SKILL and never join the command usage/Measure rows.
var artifactTypeByDirPrefix = []struct {
	prefix string
	typ    commonv1.HarnessArtifactType
}{
	{".claude/commands/", commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND},
	{".cursor/commands/", commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND},
}

// harnessArtifactType resolves the stored artifact_type: a directory-prefix override
// first (the COMMAND case), then the instruction-file basenames, then the destKind bucket.
func harnessArtifactType(filePath string, kind destKind) commonv1.HarnessArtifactType {
	lower := strings.ToLower(filePath)
	for _, override := range artifactTypeByDirPrefix {
		if pathHasDirPrefix(lower, override.prefix) {
			return override.typ
		}
	}
	// The CLAUDE_MD / AGENTS_MD basename types apply only OUTSIDE a directory convention:
	// a .claude/rules/CLAUDE.md is a RULE and a .claude/agents/AGENTS.md is an AGENT (the
	// directory convention wins), so a top-level CLAUDE.md/AGENTS.md is the only claude_md/agents_md.
	if _, isDir := dirEntryFor(filePath); !isDir {
		switch strings.ToLower(path.Base(filePath)) {
		case "claude.md":
			return commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD
		case "agents.md":
			return commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENTS_MD
		}
	}
	switch kind {
	case destSkill:
		return commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL
	case destAgent:
		return commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT
	case destHook:
		return commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_HOOK
	default: // destRule
		return commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE
	}
}

// skillPackageRoots maps each skill-package root directory to its convention, mirroring the
// desktop walkSkillsDir: a directory holding a SKILL.md directly (<prefix><slug>) is a package; a
// depth-2 directory (<prefix><group>/<slug>) is a package only when its parent holds no SKILL.md
// of its own (a grouped layout). A SKILL.md deeper than depth 2, or one whose slug/group is
// dot-prefixed (desktop skips dot-entries) or sits under a stripped dependency tree, defines no
// root. Computed once so a member file can be attached to the SKILL.md that governs it — which
// also makes a helper whose own path matches another convention (a nested .cursor/rules/x.md)
// resolve to its package rather than a phantom standalone artifact.
func skillPackageRoots(entries []repoTreeEntry) map[string]artifactConvention {
	depth1 := map[string]artifactConvention{}
	type groupedRoot struct {
		root, parent string
		conv         artifactConvention
	}
	var depth2 []groupedRoot
	for _, entry := range entries {
		if entry.Type != blobEntryType || path.Base(entry.Path) != skillPrimaryFile {
			continue
		}
		dirEntry, isDir := dirEntryFor(entry.Path)
		if !isDir || !dirEntry.skillPackage {
			continue
		}
		rest, found := afterDirPrefix(entry.Path, dirEntry.prefix)
		if !found {
			continue
		}
		prefixPart := entry.Path[:len(entry.Path)-len(rest)]
		// rest is "<slug>/SKILL.md" or "<group>/<slug>/SKILL.md"; a dot-prefixed slug/group is a
		// hidden entry the desktop scanner skips (SKILL.md itself never starts with ".").
		if hasStrippedDirSegment(prefixPart) || hasHiddenSegment(rest) {
			continue
		}
		switch segs := strings.Split(rest, "/"); len(segs) {
		case 2: // <slug>/SKILL.md
			depth1[prefixPart+segs[0]] = dirEntry.conv
		case 3: // <group>/<slug>/SKILL.md — a root only when <group> is not itself a package
			depth2 = append(depth2, groupedRoot{root: prefixPart + segs[0] + "/" + segs[1], parent: prefixPart + segs[0], conv: dirEntry.conv})
		}
	}
	roots := make(map[string]artifactConvention, len(depth1)+len(depth2))
	for root, conv := range depth1 {
		roots[root] = conv
	}
	for _, grouped := range depth2 {
		if _, parentIsPackage := depth1[grouped.parent]; !parentIsPackage {
			roots[grouped.root] = grouped.conv
		}
	}
	return roots
}

// skillRootFor returns the deepest package root that is an ancestor directory of filePath, its
// convention, and whether one governs the file. Roots never nest (a depth-2 root exists only when
// its depth-1 parent is not itself a package), so the deepest match is unambiguous.
func skillRootFor(roots map[string]artifactConvention, filePath string) (string, artifactConvention, bool) {
	best := ""
	var bestConv artifactConvention
	for root, conv := range roots {
		if strings.HasPrefix(filePath, root+"/") && len(root) > len(best) {
			best, bestConv = root, conv
		}
	}
	return best, bestConv, best != ""
}

// TreePath returns f's repo-absolute path within a. It is the key a per-path
// change set is stated in.
//
// The rule differs by artifact shape and is easy to get wrong from outside:
// SourcePath == GroupKey marks a SINGLE-FILE artifact, whose GroupKey already IS
// the file's full path, while a PACKAGE's GroupKey is the directory that each
// member's RelativePath hangs off. Deriving it here keeps that distinction with
// the type that defines it rather than duplicated at each caller.
func (a EnumeratedArtifact) TreePath(f EnumeratedArtifactFile) string {
	if a.SourcePath == a.GroupKey {
		return a.GroupKey
	}
	return a.GroupKey + "/" + f.RelativePath
}

// EnumerateArtifactFiles turns a connector tree blob into the logical harness artifacts
// it contains — the read side of VCS artifact-content ingestion (spec:
// harness-vcs-artifact-content-ingestion). A skill package (a <slug>/SKILL.md dir plus its
// helper files) is ONE artifact grouped under its package directory; every other
// convention (a rule, a slash command, an agent, a CLAUDE.md) is a single-file artifact,
// and a file under a directory convention must match that convention's extension (a
// helper like .claude/commands/util.sh is not a loadable command). Inside a package the
// authored files are kept (ingestAsPackageMember: any text, any subdirectory; only
// binaries/lockfiles/dependency-trees skipped), NOT the whole-repo strip; a standalone
// artifact stays subject to the whole-repo strip. A skill package missing its SKILL.md
// primary is dropped. The output is sorted by SourcePath for a deterministic work-list.
func EnumerateArtifactFiles(rawBlob []byte) ([]EnumeratedArtifact, error) {
	var blob repoTreeBlob
	if err := json.Unmarshal(rawBlob, &blob); err != nil {
		return nil, fmt.Errorf("unmarshalling repo tree blob for artifact enumeration: %w", err)
	}

	bySource := map[string]*EnumeratedArtifact{}
	skillRoots := skillPackageRoots(blob.Entries)
	for _, entry := range blob.Entries {
		if entry.Type != blobEntryType {
			continue
		}
		// A file under a detected skill-package root is a MEMBER of that package, whichever inner
		// convention its own path also matches — resolve the root FIRST so a helper like
		// .claude/skills/deploy/.cursor/rules/example.md joins deploy rather than emitting a phantom
		// standalone Cursor rule. skillPackageRoots already excluded roots with a hidden/stripped slug.
		if root, rootConv, isMember := skillRootFor(skillRoots, entry.Path); isMember {
			memberRel := entry.Path[len(root)+1:]
			// A package's files are AUTHORED (any text, any subdir); apply only the package-scoped
			// policy — skip binaries, lockfiles, generated stubs, dependency/VCS trees. The
			// whole-repo strip is deliberately NOT applied to a member (a build/ helper is content).
			if !ingestAsPackageMember(memberRel) {
				continue
			}
			artifact := bySource[root+"/"+skillPrimaryFile]
			if artifact == nil {
				artifact = &EnumeratedArtifact{
					SourcePath:   root + "/" + skillPrimaryFile,
					Name:         path.Base(root),
					IdeType:      rootConv.tool,
					ArtifactType: harnessArtifactType(root+"/"+skillPrimaryFile, rootConv.kind),
					GroupKey:     root,
				}
				bySource[artifact.SourcePath] = artifact
			}
			artifact.Files = append(artifact.Files, EnumeratedArtifactFile{
				RelativePath: memberRel, BlobSHA: entry.SHA, IsPrimary: memberRel == skillPrimaryFile,
			})
			continue
		}

		conv, ok := conventionFor(entry.Path)
		if !ok {
			continue // not a harness artifact convention
		}
		dirEntry, isDir := dirEntryFor(entry.Path)
		if conv.kind == destHook && !isDir && !settings.IsSettingsCarrier(conv.tool, entry.Path) {
			// A SHARED-MANIFEST hook config holds many hooks or permission-only settings in one
			// file, so a tree entry can't yield a per-hook (script, event) identity. The one
			// exception is .claude/settings.json (the settings carrier): it IS enumerated here as
			// a single-file fetch carrier, and its body is fanned out into per-hook + permissions
			// children at the content stage — see settings.ParseSettingsArtifacts. Every OTHER
			// exact-path hook config (.codex/hooks.json, .cursor/hooks.json, …) stays excluded
			// until such a parsing contract exists. A PER-FILE hook dir (.github/hooks/<slug>.json,
			// .kiro/hooks/<slug>.kiro.hook) is one hook per file — the file IS the artifact, so it
			// is enumerated by the per-slug branch below (matching the desktop Kiro/Copilot collectors).
			continue
		}

		file := EnumeratedArtifactFile{BlobSHA: entry.SHA}
		var sourcePath, groupKey, name string
		switch {
		case isDir && dirEntry.skillPackage:
			// A FLAT skill (a .md file directly under the skills dir); package members were handled
			// above. A nested file here has no governing SKILL.md (else the member branch caught it),
			// so it is an orphan; a dot-prefixed or non-.md loose file is not a skill either. A flat
			// skill is a standalone single file, so the whole-repo strip applies (a flat skill under
			// vendor/ or node_modules/ is third-party, not org content) — unlike a package member.
			if isStrippedPath(entry.Path) {
				continue
			}
			rest, found := afterDirPrefix(entry.Path, dirEntry.prefix)
			if !found || strings.Contains(rest, "/") {
				continue
			}
			// A bare SKILL.md (no slug folder) is a malformed package primary, not a flat skill.
			if rest == skillPrimaryFile || strings.HasPrefix(rest, ".") || !strings.HasSuffix(strings.ToLower(rest), ".md") {
				continue
			}
			sourcePath = entry.Path
			groupKey = entry.Path
			name = strings.TrimSuffix(rest, path.Ext(entry.Path))
			file.RelativePath = rest
			file.IsPrimary = true
		case isDir && (conv.kind == destAgent || conv.kind == destHook):
			// An agent, or a per-file hook, is one file per slug (toolArtifactTargets), never a
			// recursively-loaded tree: a nested or extra file under the dir is not an artifact the
			// tool loads, so gate by the per-slug shape and name by the slug (not the nested path).
			if isStrippedPath(entry.Path) {
				continue
			}
			slug, ok := perSlugSlug(conv, dirEntry, entry.Path)
			if !ok {
				continue
			}
			sourcePath = entry.Path
			groupKey = entry.Path
			name = slug
			file.RelativePath = path.Base(entry.Path)
			file.IsPrimary = true
		case isDir:
			// A non-package directory convention (rule / command / prompt; agents & per-file hooks
			// handled above, shared-manifest hooks excluded above) is FLAT: the desktop collectors
			// scan only direct children, so a nested file (.claude/rules/frontend/react.md) is not
			// loaded there and is rejected here too. One direct-child file, one artifact.
			if isStrippedPath(entry.Path) {
				continue
			}
			rest, found := afterDirPrefix(entry.Path, dirEntry.prefix)
			if !found || strings.Contains(rest, "/") {
				continue // nested under a flat convention — not a loadable artifact
			}
			if strings.HasPrefix(rest, ".") || !dirEntry.matchesExt(entry.Path) {
				continue // hidden entry, or wrong extension
			}
			sourcePath = entry.Path
			groupKey = entry.Path // single-file artifact: GroupKey is the file itself (see EnumeratedArtifact)
			name = dirEntry.trimExt(rest)
			file.RelativePath = path.Base(entry.Path)
			file.IsPrimary = true
		default:
			// A basename / exact-path convention (CLAUDE.md, AGENTS.md, .cursorrules, …): the
			// whole filename is the artifact's identity.
			if isStrippedPath(entry.Path) {
				continue
			}
			sourcePath = entry.Path
			groupKey = entry.Path // single-file artifact: GroupKey is the file itself (see EnumeratedArtifact)
			name = path.Base(entry.Path)
			file.RelativePath = path.Base(entry.Path)
			file.IsPrimary = true
		}

		if existing, seen := bySource[sourcePath]; seen {
			existing.Files = append(existing.Files, file)
			continue
		}
		// ArtifactType is resolved from sourcePath (the SKILL.md for a package), so a package
		// helper that happens to be named CLAUDE.md/AGENTS.md cannot flip the package's type.
		bySource[sourcePath] = &EnumeratedArtifact{
			SourcePath:   sourcePath,
			Name:         name,
			IdeType:      conv.tool,
			ArtifactType: harnessArtifactType(sourcePath, conv.kind),
			GroupKey:     groupKey,
			Files:        []EnumeratedArtifactFile{file},
		}
	}

	out := make([]EnumeratedArtifact, 0, len(bySource))
	for _, artifact := range bySource {
		if !hasPrimaryFile(artifact.Files) {
			continue // a skill package with helpers but no SKILL.md is not a loadable artifact
		}
		sort.Slice(artifact.Files, func(i, j int) bool { return artifact.Files[i].RelativePath < artifact.Files[j].RelativePath })
		out = append(out, *artifact)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourcePath < out[j].SourcePath })
	return out, nil
}

// hasPrimaryFile reports whether the manifest carries its anchor file.
func hasPrimaryFile(files []EnumeratedArtifactFile) bool {
	for _, f := range files {
		if f.IsPrimary {
			return true
		}
	}
	return false
}

// packageDependencyDirs are the only directory segments skipped INSIDE a detected artifact
// package: universal dependency / VCS / machine-generated-cache trees that are never authored
// skill content. This is deliberately narrower than the whole-repo strippedDirSegments (which
// also drops build/, dist/, out/, target/, … — names a skill author may legitimately use for
// real content). __pycache__ is not such a name: running a skill's own Python payload leaves
// one behind, and stripping .pyc alone would still let the directory join the package.
var packageDependencyDirs = map[string]struct{}{
	"node_modules": {}, "vendor": {}, ".git": {}, "__pycache__": {},
}

// ingestAsPackageMember is the single policy for which files inside a detected artifact
// package are ingested as part of that artifact (relPath is package-root-relative). Unlike
// the whole-repo isStrippedPath heuristic, a curated package's files are AUTHORED, so this
// keeps authored text of any extension in any subdirectory and skips only what is never
// skill content: binaries / minified / generated blobs, lockfiles, and dependency/VCS trees.
// One decision point — extend here (e.g. to store a class currently skipped) rather than in
// scattered strip lists. Change-detection note: a skipped file's add/remove/edit is NOT
// tracked; detecting binary-asset changes is the deferred `trackOnly` extension (Decision on
// package-member disposition), which would keep a body-absent manifest entry for it.
func ingestAsPackageMember(relPath string) bool {
	base := strings.ToLower(path.Base(relPath))
	if _, ok := strippedBasenames[base]; ok {
		return false // lockfiles / generated manifests
	}
	if _, ok := strippedExtensions[lowerExt(base)]; ok {
		return false // binary / minified / map blobs — not analyzable text content
	}
	if isGeneratedStub(base) {
		return false // generated proto/grpc stub — not authored content, even inside a package
	}
	for _, segment := range strings.Split(relPath, "/") {
		if _, ok := packageDependencyDirs[segment]; ok {
			return false // vendored dependency / VCS tree, not authored content
		}
	}
	return true
}
