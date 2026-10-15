// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"encoding/json"
	"path"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
)

// makeBlob builds a connector-shaped tree blob JSON from file paths (all emitted
// as "blob" entries) plus optional extra entries, for EncodeRepoTree input.
func makeBlob(t *testing.T, vendorTruncated bool, filePaths []string, extra ...repoTreeEntry) []byte {
	t.Helper()
	entries := make([]repoTreeEntry, 0, len(filePaths)+len(extra))
	for _, p := range filePaths {
		entries = append(entries, repoTreeEntry{Path: p, Type: "blob"})
	}
	entries = append(entries, extra...)
	raw, err := json.Marshal(repoTreeBlob{
		TreeSHA:   "sha123",
		Branch:    "main",
		FetchedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Truncated: vendorTruncated,
		Entries:   entries,
	})
	require.NoError(t, err)
	return raw
}

// reconstructPaths expands a directory-grouped directory_structure back to its
// full path set — the lossless round-trip check.
func reconstructPaths(directoryStructure string) []string {
	var paths []string
	for _, line := range strings.Split(directoryStructure, "\n") {
		if line == "" {
			continue
		}
		sep := strings.Index(line, ": ")
		dir := line[:sep]
		for _, file := range strings.Fields(line[sep+2:]) {
			paths = append(paths, path.Join(dir, file))
		}
	}
	sort.Strings(paths)
	return paths
}

func TestEncodeRepoTree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		vendorTruncated bool
		filePaths       []string
		extra           []repoTreeEntry
		primaryHint     string
		maxTokens       int
		wantKept        []string // kept source paths, sorted (the lossless set)
		wantRuleFiles   []string
		wantPrimary     string
		wantHints       []string
		wantTruncated   bool
		wantReason      TruncationReason
	}{
		{
			name:        "lossless round-trip with directory grouping",
			filePaths:   []string{"main.go", "dao/repo.go", "dao/repo_test.go", "dao/model/m.go"},
			maxTokens:   0,
			wantKept:    []string{"dao/model/m.go", "dao/repo.go", "dao/repo_test.go", "main.go"},
			wantPrimary: "Go",
		},
		{
			name: "strips generated, binary, vendored, lockfiles",
			filePaths: []string{
				"main.go",
				"service.pb.go",             // generated proto stub
				"web/app.min.js",            // minified
				"assets/logo.png",           // binary
				"package-lock.json",         // lockfile
				"node_modules/dep/index.js", // vendored dir
				"vendor/x/y.go",             // vendored dir
			},
			maxTokens:   0,
			wantKept:    []string{"main.go"},
			wantPrimary: "Go",
		},
		{
			name:          "detects rule files",
			filePaths:     []string{"CLAUDE.md", ".cursorrules", ".cursor/rules/style.mdc", "main.go"},
			maxTokens:     0,
			wantKept:      []string{".cursor/rules/style.mdc", ".cursorrules", "CLAUDE.md", "main.go"},
			wantRuleFiles: []string{".cursor/rules/style.mdc", ".cursorrules", "CLAUDE.md"},
			wantPrimary:   "Go",
		},
		{
			name:        "language primary and hints by frequency",
			filePaths:   []string{"a.go", "b.go", "c.go", "x.ts", "y.py"},
			maxTokens:   0,
			wantKept:    []string{"a.go", "b.go", "c.go", "x.ts", "y.py"},
			wantPrimary: "Go",
			wantHints:   []string{"Python", "TypeScript"}, // tie at 1 broken by name
		},
		{
			name:        "vendor-detected language hint wins as primary",
			filePaths:   []string{"a.go", "b.go", "x.rb"},
			primaryHint: "Ruby",
			maxTokens:   0,
			wantKept:    []string{"a.go", "b.go", "x.rb"},
			wantPrimary: "Ruby",
			wantHints:   []string{"Go"},
		},
		{
			name:        "excludes directory (tree) entries",
			filePaths:   []string{"main.go"},
			extra:       []repoTreeEntry{{Path: "dao", Type: "tree"}, {Path: "dao/model", Type: "tree"}},
			maxTokens:   0,
			wantKept:    []string{"main.go"},
			wantPrimary: "Go",
		},
		{
			name:            "vendor-truncated blob is used and flagged",
			vendorTruncated: true,
			filePaths:       []string{"main.go"},
			maxTokens:       0,
			wantKept:        []string{"main.go"},
			wantPrimary:     "Go",
			wantTruncated:   true,
			wantReason:      TruncationVendor,
		},
		{
			name:          "token cap truncates and flags",
			filePaths:     []string{"a/1.go", "b/2.go", "c/3.go", "d/4.go", "e/5.go"},
			maxTokens:     2, // ~8 chars — only the first dir line fits
			wantTruncated: true,
			wantReason:    TruncationTokenCap,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			blob := makeBlob(t, tc.vendorTruncated, tc.filePaths, tc.extra...)
			tree, reason, err := EncodeRepoTree(blob, "owner/repo", tc.primaryHint, tc.maxTokens)
			require.NoError(t, err)
			require.NotNil(t, tree)

			assert.Equal(t, "owner/repo", tree.GetRepoFullName())
			assert.Equal(t, tc.wantTruncated, tree.GetTruncated())
			assert.Equal(t, tc.wantReason, reason)

			if tc.wantReason != TruncationTokenCap {
				// Lossless round-trip only holds when nothing was cap-dropped.
				assert.Equal(t, tc.wantKept, reconstructPaths(tree.GetDirectoryStructure()),
					"directory_structure must losslessly preserve the kept path set")
				assert.Equal(t, int32(len(tc.wantKept)), tree.GetFileCount())
			}
			if tc.wantPrimary != "" {
				assert.Equal(t, tc.wantPrimary, tree.GetPrimaryLanguage())
			}
			if tc.wantHints != nil {
				assert.Equal(t, tc.wantHints, tree.GetLanguageHints())
			}
			if tc.wantRuleFiles != nil {
				assert.Equal(t, tc.wantRuleFiles, tree.GetRuleFiles())
			}
		})
	}
}

// TestEncodeRepoTree_RoundTripsInput asserts the encoding reconstructs EXACTLY
// the (space-free) input path set — compared against the input itself, not a
// hand-maintained expectation, so the lossless guarantee can't silently drift.
// TestEncodeRepoTree_ConnectorWireContract guards repoTreeBlob/repoTreeEntry's
// JSON tags against the stable repo-tree wire shape a VCS ingest writes. This
// package re-declares that shape rather than importing the producer, so this test
// feeds the exact bytes the producer writes (including the mode/sha fields this
// package ignores) so a producer-side tag rename surfaces as a test failure here
// instead of a silent zero-entry decode.
func TestEncodeRepoTree_ConnectorWireContract(t *testing.T) {
	t.Parallel()
	// Mirrors connector treewalker.TreeEntry (path/type/size/mode/sha) wrapped in
	// the fetch_repo_tree blob (tree_sha/branch/fetched_at/truncated/entries).
	const connectorBlob = `{
		"tree_sha": "abc123",
		"branch": "main",
		"fetched_at": "2026-01-02T03:04:05Z",
		"truncated": false,
		"entries": [
			{"path": "main.go", "type": "blob", "size": 120, "mode": "100644", "sha": "d1"},
			{"path": "internal", "type": "tree", "mode": "040000", "sha": "d2"},
			{"path": "internal/svc.go", "type": "blob", "size": 88, "mode": "100644", "sha": "d3"}
		]
	}`
	tree, reason, err := EncodeRepoTree([]byte(connectorBlob), "owner/repo", "Go", 0)
	require.NoError(t, err)
	assert.Equal(t, TruncationNone, reason)
	assert.False(t, tree.GetTruncated())
	// Both blob files decoded and reached the output; the tree entry is dropped.
	assert.ElementsMatch(t, []string{"main.go", "internal/svc.go"}, reconstructPaths(tree.GetDirectoryStructure()))
	assert.Equal(t, int32(2), tree.GetFileCount())
}

func TestEncodeRepoTree_RoundTripsInput(t *testing.T) {
	t.Parallel()
	input := []string{"main.go", "dao/a.go", "dao/b.go", "dao/model/m.go", "internal/x/y/z.go"}
	blob := makeBlob(t, false, input)
	tree, reason, err := EncodeRepoTree(blob, "owner/repo", "", 0)
	require.NoError(t, err)
	require.Equal(t, TruncationNone, reason)
	want := append([]string(nil), input...)
	sort.Strings(want)
	assert.Equal(t, want, reconstructPaths(tree.GetDirectoryStructure()),
		"every input path reconstructs from directory_structure")
}

// TestEncodeRepoTree_DirectoryWrittenOnce asserts a parent directory's name is
// written exactly once regardless of how many files it holds (the prefix-token
// saving that motivates the directory-grouped encoding#14b).
func TestEncodeRepoTree_DirectoryWrittenOnce(t *testing.T) {
	t.Parallel()
	blob := makeBlob(t, false, []string{"dao/a.go", "dao/b.go", "dao/c.go"})
	tree, _, err := EncodeRepoTree(blob, "owner/repo", "", 0)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(tree.GetDirectoryStructure(), "dao: "),
		"the dao/ prefix must appear once, not per child file")
	assert.Contains(t, tree.GetDirectoryStructure(), "dao: a.go b.go c.go")
}

// TestEncodeRepoTree_DirectoryGroupedSmallerThanFlat validates the core token
// claim: the directory-grouped encoding is materially smaller
// than the equivalent flat absolute-path listing for a realistically nested tree.
func TestEncodeRepoTree_DirectoryGroupedSmallerThanFlat(t *testing.T) {
	t.Parallel()
	var paths []string
	for _, dir := range []string{"internal/service/dao", "internal/service/engine", "pkg/config/loader"} {
		for _, f := range []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go"} {
			paths = append(paths, dir+"/"+f)
		}
	}
	blob := makeBlob(t, false, paths)
	tree, _, err := EncodeRepoTree(blob, "owner/repo", "", 0)
	require.NoError(t, err)

	grouped := len(tree.GetDirectoryStructure())
	flat := len(strings.Join(paths, "\n"))
	assert.Less(t, grouped, flat, "directory-grouped (%d) must be smaller than flat paths (%d)", grouped, flat)
	t.Logf("flat=%d grouped=%d (%.0f%% of flat)", flat, grouped, float64(grouped)/float64(flat)*100)
}

// TestIsRuleFile_MultiTool pins detection across every supported tool's rule-file
// convention (multi-IDE): basenames in any directory, and directory-prefix rules.
func TestIsRuleFile_MultiTool(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want bool
	}{
		{"CLAUDE.md", true},
		{"web/CLAUDE.md", true},
		{"AGENTS.md", true},
		{"backend/AGENTS.md", true},
		{"GEMINI.md", true},
		{".cursorrules", true},
		{".cursor/rules/style.mdc", true},
		{".claude/rules/go-errors.md", true},
		// Windsurf is not a supported IdeType, so its file is deliberately NOT detected
		// (a detected file must map to a known tool — see TestArtifactConventionsMapToKnownTool).
		{".windsurfrules", false},
		{".github/copilot-instructions.md", true},
		{".github/instructions/go.instructions.md", true},
		{".github/prompts/review.prompt.md", true},
		{".claude/skills/spec/SKILL.md", true},
		{".claude/commands/ship.md", true},
		{".agents/skills/build/SKILL.md", true},
		{".kiro/steering/product.md", true},
		{"README.md", false},
		{"backend/main.go", false},
		{"docs/agents.txt", false},
		// copilot-instructions.md is a Copilot config ONLY at .github/ root — a copy of
		// that name elsewhere (docs, an example tree) is not, so it must not match.
		{"docs/copilot-instructions.md", false},
		{"copilot-instructions.md", false},
		// Mid-path substring collisions must NOT match — the prefix is anchored to a
		// real path-segment boundary, not a bare substring.
		{"vendor/my.cursor/rules/config.yaml", false},
		{"docs/archive.github/prompts/note.txt", false},
		{"team.claude/skills/onboarding.md", false},
	}
	for _, tc := range tests {
		if got := isRuleFile(tc.path); got != tc.want {
			t.Errorf("isRuleFile(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestRuleFileTool pins the best-effort file→tool label used for the artifact's
// target_tool icon. AGENTS.md maps to CODEX (the tool that set the convention) even
// though it is cross-tool; an unknown path is UNSPECIFIED.
func TestRuleFileTool(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want commonv1.IdeType
	}{
		{"CLAUDE.md", commonv1.IdeType_IDE_TYPE_CLAUDE_CODE},
		{".claude/skills/x.md", commonv1.IdeType_IDE_TYPE_CLAUDE_CODE},
		{"AGENTS.md", commonv1.IdeType_IDE_TYPE_CODEX},
		{"GEMINI.md", commonv1.IdeType_IDE_TYPE_ANTIGRAVITY},
		{".cursorrules", commonv1.IdeType_IDE_TYPE_CURSOR},
		{".cursor/rules/x.mdc", commonv1.IdeType_IDE_TYPE_CURSOR},
		{".claude/rules/x.md", commonv1.IdeType_IDE_TYPE_CLAUDE_CODE},
		{".github/copilot-instructions.md", commonv1.IdeType_IDE_TYPE_COPILOT},
		{".github/prompts/x.prompt.md", commonv1.IdeType_IDE_TYPE_COPILOT},
		{".agents/skills/build/SKILL.md", commonv1.IdeType_IDE_TYPE_CODEX},
		{".kiro/steering/x.md", commonv1.IdeType_IDE_TYPE_KIRO},
		// copilot-instructions.md is anchored to .github/ root — elsewhere it is unknown.
		{"docs/copilot-instructions.md", commonv1.IdeType_IDE_TYPE_UNSPECIFIED},
		{"docs/notes.md", commonv1.IdeType_IDE_TYPE_UNSPECIFIED},
		// A mid-path substring collision must not map to a tool.
		{"vendor/my.cursor/rules/x.yaml", commonv1.IdeType_IDE_TYPE_UNSPECIFIED},
		{"team.claude/skills/x.md", commonv1.IdeType_IDE_TYPE_UNSPECIFIED},
		// Directory placement wins over a colliding rule basename: a file named like a
		// rule but living in a skill dir is that dir's tool, not the basename's tool.
		{".claude/skills/agents.md", commonv1.IdeType_IDE_TYPE_CLAUDE_CODE},
		{".github/prompts/copilot-instructions.md", commonv1.IdeType_IDE_TYPE_COPILOT},
	}
	for _, tc := range tests {
		if got := RuleFileTool(tc.path); got != tc.want {
			t.Errorf("RuleFileTool(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestArtifactConventionsMapToKnownTool is the drift guard for the single-source
// detection tables: every recognized rule/skill destination MUST map to a known
// IdeType via RuleFileTool. It fails the moment someone adds a basename/dir prefix
// (a new tool's convention) without giving it a tool — the exact silent-mislabel gap
// that let an unsupported ".windsurfrules" slip in before.
func TestArtifactConventionsMapToKnownTool(t *testing.T) {
	t.Parallel()
	for base, conv := range artifactFileBasenames {
		assert.NotEqual(t, commonv1.IdeType_IDE_TYPE_UNSPECIFIED, conv.tool, "basename %q must map to a known tool", base)
		assert.NotEqual(t, commonv1.IdeType_IDE_TYPE_UNSPECIFIED, RuleFileTool(base), "RuleFileTool(%q) must not be UNSPECIFIED", base)
	}
	for exactPath, conv := range artifactExactPaths {
		assert.NotEqual(t, commonv1.IdeType_IDE_TYPE_UNSPECIFIED, conv.tool, "exact path %q must map to a known tool", exactPath)
		assert.NotEqual(t, commonv1.IdeType_IDE_TYPE_UNSPECIFIED, RuleFileTool(exactPath), "RuleFileTool(%q) must not be UNSPECIFIED", exactPath)
	}
	for _, entry := range artifactDirPrefixes {
		sample := entry.prefix + "x.md"
		assert.NotEqual(t, commonv1.IdeType_IDE_TYPE_UNSPECIFIED, entry.conv.tool, "dir %q must map to a known tool", entry.prefix)
		assert.NotEqual(t, commonv1.IdeType_IDE_TYPE_UNSPECIFIED, RuleFileTool(sample), "RuleFileTool(%q) must not be UNSPECIFIED", sample)
		// A skillPackage dir builds "<slug>/SKILL.md"; a single-file dir needs at least one
		// extension. Neither can be empty-and-flat, else newArtifactPath would produce "<dir>/<slug>".
		if !entry.skillPackage {
			assert.NotEmpty(t, entry.exts, "single-file dir %q must declare at least one extension", entry.prefix)
		}
	}
}

// TestAgentsPrefixDisambiguation pins the .agents/ sub-path split: the
// anchored dir-prefix scan routes .agents/agents/ to an Antigravity AGENT destination and
// .agents/skills/ to a SKILL destination — they share the .agents/ root but never
// collide, and .agents/hooks.json is an Antigravity HOOK config.
func TestAgentsPrefixDisambiguation(t *testing.T) {
	t.Parallel()
	antigravityAgent, ok := conventionFor(".agents/agents/reviewer/agent.md")
	assert.True(t, ok, ".agents/agents/ file must be a recognized convention")
	assert.Equal(t, commonv1.IdeType_IDE_TYPE_ANTIGRAVITY, antigravityAgent.tool)
	assert.Equal(t, destAgent, antigravityAgent.kind)

	codexSkill, ok := conventionFor(".agents/skills/build/SKILL.md")
	assert.True(t, ok, ".agents/skills/ file must still be a skill")
	assert.Equal(t, commonv1.IdeType_IDE_TYPE_CODEX, codexSkill.tool)
	assert.Equal(t, destSkill, codexSkill.kind)

	antigravityHook, ok := conventionFor(".agents/hooks.json")
	assert.True(t, ok, ".agents/hooks.json must be a recognized convention")
	assert.Equal(t, commonv1.IdeType_IDE_TYPE_ANTIGRAVITY, antigravityHook.tool)
	assert.Equal(t, destHook, antigravityHook.kind)

	// Neither Antigravity path is mistaken for a rule/skill destination (so they can never
	// be routed a rule or skill body).
	assert.False(t, isRuleDestination(".agents/agents/reviewer/agent.md"))
	assert.False(t, isSkillDestination(".agents/agents/reviewer/agent.md"))
}

// TestDestAgentHookPaths pins the per-tool agent-file and hook-config destinations and
// their placements: every tool has file-backed agents (incl. Cursor); a per-slug hook
// path (Kiro's .kiro.hook manifest, Copilot's per-hook json) is a standalone file
// (NEW_REFERENCED_FILE) while a shared settings/hooks file is merged into (COPY_MERGE).
func TestDestAgentHookPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		tool          commonv1.IdeType
		wantAgent     string
		wantHook      string
		wantHookPlace promptlibraryv1.ArtifactTargetPlacement
	}{
		{commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, ".claude/agents/reviewer.md", ".claude/settings.json", promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_COPY_MERGE},
		{commonv1.IdeType_IDE_TYPE_CODEX, ".codex/agents/reviewer.toml", ".codex/hooks.json", promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_COPY_MERGE},
		{commonv1.IdeType_IDE_TYPE_COPILOT, ".github/agents/reviewer.agent.md", ".github/hooks/reviewer.json", promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE}, // per-slug hook file → created, not merged
		{commonv1.IdeType_IDE_TYPE_ANTIGRAVITY, ".agents/agents/reviewer/agent.md", ".agents/hooks.json", promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_COPY_MERGE},
		{commonv1.IdeType_IDE_TYPE_KIRO, ".kiro/agents/reviewer.md", ".kiro/hooks/reviewer.kiro.hook", promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE},
		{commonv1.IdeType_IDE_TYPE_CURSOR, ".cursor/agents/reviewer.md", ".cursor/hooks.json", promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_COPY_MERGE},
	}
	for _, tc := range tests {
		t.Run(tc.tool.String(), func(t *testing.T) {
			assert.Equal(t, tc.wantAgent, destAgentPath(tc.tool, "reviewer"))
			assert.Equal(t, tc.wantHook, destHookPath(tc.tool, "reviewer"))
			assert.Equal(t, tc.wantHookPlace, hookTargetPlacement(tc.tool))
			// The canonical destinations round-trip through the honor-LLM-path check.
			assert.True(t, isToolAgentTarget(tc.wantAgent, tc.tool), "canonical agent path must validate for its tool")
			assert.True(t, isToolHookTarget(tc.wantHook, tc.tool), "canonical hook path must validate for its tool")
		})
	}
	// An UNSPECIFIED tool yields no destination (the gate hides a tool-less hook/agent).
	assert.Empty(t, destAgentPath(commonv1.IdeType_IDE_TYPE_UNSPECIFIED, "reviewer"))
	assert.Empty(t, destHookPath(commonv1.IdeType_IDE_TYPE_UNSPECIFIED, "reviewer"))
	// A wrong-tool path does not validate (a Claude agent path is not a agent target).
	assert.False(t, isToolAgentTarget(".claude/agents/reviewer.md", commonv1.IdeType_IDE_TYPE_CODEX))
}

// TestConventionPrecedence_DirWinsOverBasename pins that directory placement beats a
// colliding rule basename: a rule-named file inside a skill dir is a SKILL destination
// (not an appendable rule target), so a rule can never be routed into it.
func TestConventionPrecedence_DirWinsOverBasename(t *testing.T) {
	t.Parallel()
	assert.True(t, isSkillDestination(".claude/skills/agents.md"), "a rule-named file in a skill dir is a skill destination")
	assert.False(t, isRuleDestination(".claude/skills/agents.md"), "and NOT a rule destination")
	// A rule basename at repo root (no dir prefix) is still a rule destination.
	assert.True(t, isRuleDestination("AGENTS.md"))
	assert.False(t, isSkillDestination("AGENTS.md"))
	// .claude/rules/ is a rule destination (a per-rule dir), NOT a skill dir.
	assert.True(t, isRuleDestination(".claude/rules/x.md"))
	assert.False(t, isSkillDestination(".claude/rules/x.md"))
}

// TestRuleFileClassification pins the shared-file vs per-rule-dir split that drives
// placement coercion, and the skill-destination-by-tool map that lets a skill-less
// tool's rule file host a skill.
func TestRuleFileClassification(t *testing.T) {
	t.Parallel()
	// Shared single rules files: appended to, never "created and referenced from self".
	for _, shared := range []string{"CLAUDE.md", "web/CLAUDE.md", "AGENTS.md", ".cursorrules", "GEMINI.md", ".github/copilot-instructions.md"} {
		assert.True(t, isSharedRuleFile(shared), "%q is a shared rules file", shared)
		assert.False(t, matchesRuleDir(shared), "%q is not a per-rule dir", shared)
	}
	// Per-rule directory files: each rule is its own file, so NOT shared.
	for _, perRule := range []string{".cursor/rules/x.mdc", ".claude/rules/x.md", ".kiro/steering/x.md", ".github/instructions/x.md"} {
		assert.True(t, matchesRuleDir(perRule), "%q is a per-rule dir", perRule)
		assert.False(t, isSharedRuleFile(perRule), "%q is not a shared file", perRule)
	}
	// A skill file is neither.
	assert.False(t, isSharedRuleFile(".claude/skills/x.md"))
	assert.False(t, matchesRuleDir(".claude/skills/x.md"))

	// Only Cursor merges skills into its rule file; every other rule-only tool authors
	// standalone frontmatter skill files, so its rule file is not a skill target.
	assert.True(t, skillsMergeIntoRuleFile(commonv1.IdeType_IDE_TYPE_CURSOR))
	assert.False(t, skillsMergeIntoRuleFile(commonv1.IdeType_IDE_TYPE_CLAUDE_CODE))
	assert.False(t, skillsMergeIntoRuleFile(commonv1.IdeType_IDE_TYPE_CODEX))
	assert.False(t, skillsMergeIntoRuleFile(commonv1.IdeType_IDE_TYPE_KIRO))
	assert.False(t, skillsMergeIntoRuleFile(commonv1.IdeType_IDE_TYPE_ANTIGRAVITY))
}

// TestToolArtifactTargetsAreDetected ensures every hook and agent DESTINATION in
// toolArtifactTargets must be recognized by conventionFor as that tool's destHook/destAgent,
// binding the two halves of the two-place-edit invariant (destination table + detection
// collections). Without it a tool wired for emission is silently undetectable when checked
// in, so a hook/agent-only repo resolves tool-less.
func TestToolArtifactTargetsAreDetected(t *testing.T) {
	t.Parallel()
	for tool := range toolArtifactTargets {
		agentPath := destAgentPath(tool, "sample")
		conv, ok := conventionFor(agentPath)
		assert.True(t, ok, "agent path %q for %v must be a detected convention", agentPath, tool)
		assert.Equal(t, tool, conv.tool, "agent path %q must map to %v", agentPath, tool)
		assert.Equal(t, destAgent, conv.kind, "agent path %q must be destAgent", agentPath)

		hookPath := destHookPath(tool, "sample")
		hookConv, hookOK := conventionFor(hookPath)
		assert.True(t, hookOK, "hook path %q for %v must be a detected convention", hookPath, tool)
		assert.Equal(t, tool, hookConv.tool, "hook path %q must map to %v", hookPath, tool)
		assert.Equal(t, destHook, hookConv.kind, "hook path %q must be destHook", hookPath)
	}
}

// TestResolveDominantTool_HookAgentOnlyRepo pins the resolver precedence: a repo whose only
// checked-in AI signal is a hook or agent config resolves to that tool, a checked-in
// hook/agent overrides a conflicting ide_breakdown, and a rule file still outranks a
// hook/agent of another tool (rule > hook/agent > ide_breakdown).
func TestResolveDominantTool_HookAgentOnlyRepo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		ruleFiles    []string
		ideBreakdown []*promptlibraryv1.IDESummary
		want         commonv1.IdeType
	}{
		{"settings.json only", []string{".claude/settings.json"}, nil, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE},
		{"cursor hooks only", []string{".cursor/hooks.json"}, nil, commonv1.IdeType_IDE_TYPE_CURSOR},
		{"claude agent file only", []string{".claude/agents/reviewer.md"}, nil, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE},
		{"copilot hook file only", []string{".github/hooks/lint.json"}, nil, commonv1.IdeType_IDE_TYPE_COPILOT},
		// A checked-in hook overrides a conflicting session mix.
		{
			"hook overrides ide_breakdown",
			[]string{".cursor/hooks.json"},
			[]*promptlibraryv1.IDESummary{{Ide: commonv1.IdeType_IDE_TYPE_CLAUDE_CODE.String()}},
			commonv1.IdeType_IDE_TYPE_CURSOR,
		},
		// A rule file still wins over a hook config of another tool (rule > hook/agent).
		{"rule beats hook", []string{".cursor/hooks.json", "CLAUDE.md"}, nil, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE},
		// Two tools' configs: rule_files is sorted, so the alphabetically-first path's tool
		// wins deterministically (.claude/settings.json < .codex/hooks.json → Claude).
		{"two configs, sorted-first wins", []string{".claude/settings.json", ".codex/hooks.json"}, nil, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE},
		// ide_breakdown fallback (no checked-in config): a legacy lowercase ideType label must
		// still resolve — AggregateByIDE returns usage_metadata.ideType verbatim.
		{"legacy-lowercase ide_breakdown resolves", nil, []*promptlibraryv1.IDESummary{{Ide: "cursor"}}, commonv1.IdeType_IDE_TYPE_CURSOR},
		{"canonical ide_breakdown resolves", nil, []*promptlibraryv1.IDESummary{{Ide: commonv1.IdeType_IDE_TYPE_CODEX.String()}}, commonv1.IdeType_IDE_TYPE_CODEX},
		// A dominant legacy tool-row wins over a lower-ranked canonical row of another tool.
		{"legacy dominant beats lower canonical", nil, []*promptlibraryv1.IDESummary{{Ide: "cursor"}, {Ide: commonv1.IdeType_IDE_TYPE_CODEX.String()}}, commonv1.IdeType_IDE_TYPE_CURSOR},
		// A non-tool IDE (no enum) is skipped so a later tool row can resolve.
		{"non-tool ide skipped, next resolves", nil, []*promptlibraryv1.IDESummary{{Ide: "vscode"}, {Ide: "claude-code"}}, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE},
		// Multi-tool repo (AGENTS.md=Codex + CLAUDE.md=Claude): the session IDE mix breaks the
		// tie, NOT path sort order (which would pick AGENTS.md=Codex).
		{"multi-tool rules, Claude-dominant session → Claude", []string{"AGENTS.md", "CLAUDE.md"}, []*promptlibraryv1.IDESummary{{Ide: commonv1.IdeType_IDE_TYPE_CLAUDE_CODE.String()}}, commonv1.IdeType_IDE_TYPE_CLAUDE_CODE},
		{"multi-tool rules, Codex-dominant session → Codex", []string{"AGENTS.md", "CLAUDE.md"}, []*promptlibraryv1.IDESummary{{Ide: commonv1.IdeType_IDE_TYPE_CODEX.String()}}, commonv1.IdeType_IDE_TYPE_CODEX},
		// Dominant IDE not among the committed tools → sorted-first fallback (AGENTS.md=Codex).
		{"multi-tool rules, unrelated dominant IDE → sorted-first", []string{"AGENTS.md", "CLAUDE.md"}, []*promptlibraryv1.IDESummary{{Ide: commonv1.IdeType_IDE_TYPE_CURSOR.String()}}, commonv1.IdeType_IDE_TYPE_CODEX},
		// securityOnly (nil ide_breakdown) → sorted-first fallback, deterministic.
		{"multi-tool rules, no session signal → sorted-first", []string{"AGENTS.md", "CLAUDE.md"}, nil, commonv1.IdeType_IDE_TYPE_CODEX},
		// Multi-tool at the config tier disambiguates the same way (settings.json=Claude,
		// codex hooks=Codex) —-dominant session picks over the sorted-first Claude.
		{"multi-tool configs,-dominant session →", []string{".claude/settings.json", ".codex/hooks.json"}, []*promptlibraryv1.IDESummary{{Ide: commonv1.IdeType_IDE_TYPE_CODEX.String()}}, commonv1.IdeType_IDE_TYPE_CODEX},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tree := &promptlibraryv1.RepoTree{RuleFiles: tc.ruleFiles}
			assert.Equal(t, tc.want, resolveDominantTool(tc.ideBreakdown, tree))
		})
	}
}

// TestIsKnownForAnySupportedTool pins the cross-tool-vs-drift split: a lifecycle event owned
// by any mapped open-vocab tool is "known" (case-insensitively), a globally-unknown event is
// not, and Kiro's closed file-event vocabulary is excluded so it never reads as a cross-tool
// lifecycle mismatch.
func TestIsKnownForAnySupportedTool(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		event string
		want  bool
	}{
		{name: "claude lifecycle event", event: "PreToolUse", want: true},
		{name: "cursor-only event", event: "beforeShellExecution", want: true},
		{name: "copilot camelCase equals claude token via lowercasing", event: "preToolUse", want: true},
		{name: "shared token across tools", event: "Stop", want: true},
		{name: "case-insensitive match", event: "pretooluse", want: true},
		{name: "whitespace trimmed", event: "  SessionStart  ", want: true},
		{name: "globally-unknown event", event: "onFooBarNovel", want: false},
		{name: "kiro file event excluded", event: "PostFileSave", want: false},
		{name: "empty event", event: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isKnownForAnySupportedTool(tc.event))
		})
	}
}

// TestEnumerateArtifactFiles covers the read-side enumerator: a skill package groups its
// files under one artifact (primary SKILL.md, per-file blob SHAs), binary/generated files
// are dropped, and a checked-in slash command classifies as COMMAND rather than SKILL.
func TestEnumerateArtifactFiles(t *testing.T) {
	t.Parallel()
	entry := func(p, sha string) repoTreeEntry { return repoTreeEntry{Path: p, Type: "blob", SHA: sha} }

	tests := []struct {
		name    string
		entries []repoTreeEntry
		want    []EnumeratedArtifact
	}{
		{
			name: "skill package keeps authored text in any subdir; skips binary/lockfile/deps",
			entries: []repoTreeEntry{
				entry(".claude/skills/deploy/SKILL.md", "b1"),
				entry(".claude/skills/deploy/scripts/run.sh", "b2"),
				entry(".claude/skills/deploy/build/steps.md", "b4"),       // authored text under a "build" dir — KEPT (not whole-repo cruft)
				entry(".claude/skills/deploy/diagram.png", "b3"),          // binary — skipped
				entry(".claude/skills/deploy/package-lock.json", "b5"),    // lockfile — skipped
				entry(".claude/skills/deploy/node_modules/dep.js", "b6"),  // dependency tree — skipped
				entry(".claude/skills/deploy/__pycache__/notes.md", "b7"), // authored-looking text inside a cache dir — skipped on the dir segment alone
				entry(".claude/skills/deploy/scripts/stray.pyc", "b8"),    // bytecode outside a cache dir — skipped on the extension alone
			},
			want: []EnumeratedArtifact{{
				SourcePath:   ".claude/skills/deploy/SKILL.md",
				Name:         "deploy",
				IdeType:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
				ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
				GroupKey:     ".claude/skills/deploy",
				Files: []EnumeratedArtifactFile{
					{RelativePath: "SKILL.md", BlobSHA: "b1", IsPrimary: true},
					{RelativePath: "build/steps.md", BlobSHA: "b4", IsPrimary: false},
					{RelativePath: "scripts/run.sh", BlobSHA: "b2", IsPrimary: false},
				},
			}},
		},
		{
			name: "skill package with no SKILL.md is dropped (no loadable primary)",
			entries: []repoTreeEntry{
				entry(".claude/skills/orphan/scripts/run.sh", "o1"),
			},
			want: []EnumeratedArtifact{},
		},
		{
			name: "package rooted under a stripped dependency tree is rejected; a real package survives",
			entries: []repoTreeEntry{
				entry("vendor/acme/.claude/skills/deploy/SKILL.md", "v1"), // vendored third-party skill — rejected
				entry("node_modules/pkg/.agents/skills/x/SKILL.md", "v2"), // dependency tree — rejected
				entry(".claude/skills/mine/SKILL.md", "k1"),               // real org package — kept
			},
			want: []EnumeratedArtifact{{
				SourcePath:   ".claude/skills/mine/SKILL.md",
				Name:         "mine",
				IdeType:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
				ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
				GroupKey:     ".claude/skills/mine",
				Files:        []EnumeratedArtifactFile{{RelativePath: "SKILL.md", BlobSHA: "k1", IsPrimary: true}},
			}},
		},
		{
			name: "flat and grouped skill layouts (mirrors desktop walkSkillsDir levels)",
			entries: []repoTreeEntry{
				entry(".claude/skills/deploy.md", "f1"),                   // flat level-0 .md file → skill "deploy"
				entry(".claude/skills/team/onboard/SKILL.md", "g1"),       // grouped level-2, no team/SKILL.md → skill "onboard"
				entry(".claude/skills/team/onboard/scripts/run.sh", "g2"), // member of the grouped package
			},
			want: []EnumeratedArtifact{
				{
					SourcePath:   ".claude/skills/deploy.md",
					Name:         "deploy",
					IdeType:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
					GroupKey:     ".claude/skills/deploy.md",
					Files:        []EnumeratedArtifactFile{{RelativePath: "deploy.md", BlobSHA: "f1", IsPrimary: true}},
				},
				{
					SourcePath:   ".claude/skills/team/onboard/SKILL.md",
					Name:         "onboard",
					IdeType:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
					GroupKey:     ".claude/skills/team/onboard",
					Files: []EnumeratedArtifactFile{
						{RelativePath: "SKILL.md", BlobSHA: "g1", IsPrimary: true},
						{RelativePath: "scripts/run.sh", BlobSHA: "g2", IsPrimary: false},
					},
				},
			},
		},
		{
			name: "depth-1 package with a nested SKILL.md keeps the nested file as a member, not its own skill",
			entries: []repoTreeEntry{
				entry(".claude/skills/deploy/SKILL.md", "p1"),
				entry(".claude/skills/deploy/sub/SKILL.md", "p2"), // parent is a package → nested SKILL.md is a member
			},
			want: []EnumeratedArtifact{{
				SourcePath:   ".claude/skills/deploy/SKILL.md",
				Name:         "deploy",
				IdeType:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
				ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
				GroupKey:     ".claude/skills/deploy",
				Files: []EnumeratedArtifactFile{
					{RelativePath: "SKILL.md", BlobSHA: "p1", IsPrimary: true},
					{RelativePath: "sub/SKILL.md", BlobSHA: "p2", IsPrimary: false},
				},
			}},
		},
		{
			name: "cursor skills/commands and codex rules (.rules/.mdc) conventions are recognized",
			entries: []repoTreeEntry{
				entry(".cursor/skills/deploy/SKILL.md", "cs1"),
				entry(".cursor/commands/ship.md", "cc1"),
				entry(".codex/rules/team.rules", "cr1"),
				entry(".codex/rules/style.mdc", "cr2"),
			},
			want: []EnumeratedArtifact{
				{
					SourcePath:   ".codex/rules/style.mdc",
					Name:         "style",
					IdeType:      commonv1.IdeType_IDE_TYPE_CODEX,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE,
					GroupKey:     ".codex/rules/style.mdc",
					Files:        []EnumeratedArtifactFile{{RelativePath: "style.mdc", BlobSHA: "cr2", IsPrimary: true}},
				},
				{
					SourcePath:   ".codex/rules/team.rules",
					Name:         "team",
					IdeType:      commonv1.IdeType_IDE_TYPE_CODEX,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE,
					GroupKey:     ".codex/rules/team.rules",
					Files:        []EnumeratedArtifactFile{{RelativePath: "team.rules", BlobSHA: "cr1", IsPrimary: true}},
				},
				{
					SourcePath:   ".cursor/commands/ship.md",
					Name:         "ship",
					IdeType:      commonv1.IdeType_IDE_TYPE_CURSOR,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND,
					GroupKey:     ".cursor/commands/ship.md",
					Files:        []EnumeratedArtifactFile{{RelativePath: "ship.md", BlobSHA: "cc1", IsPrimary: true}},
				},
				{
					SourcePath:   ".cursor/skills/deploy/SKILL.md",
					Name:         "deploy",
					IdeType:      commonv1.IdeType_IDE_TYPE_CURSOR,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
					GroupKey:     ".cursor/skills/deploy",
					Files:        []EnumeratedArtifactFile{{RelativePath: "SKILL.md", BlobSHA: "cs1", IsPrimary: true}},
				},
			},
		},
		{
			name: "slash commands classify as COMMAND; wrong-extension helper rejected",
			entries: []repoTreeEntry{
				entry(".claude/commands/foo.md", "c1"),
				entry(".claude/commands/bar.md", "c2"),
				entry(".claude/commands/util.sh", "c3"), // not a .md command — rejected
			},
			want: []EnumeratedArtifact{
				{
					SourcePath:   ".claude/commands/bar.md",
					Name:         "bar",
					IdeType:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND,
					GroupKey:     ".claude/commands/bar.md",
					Files:        []EnumeratedArtifactFile{{RelativePath: "bar.md", BlobSHA: "c2", IsPrimary: true}},
				},
				{
					SourcePath:   ".claude/commands/foo.md",
					Name:         "foo",
					IdeType:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND,
					GroupKey:     ".claude/commands/foo.md",
					Files:        []EnumeratedArtifactFile{{RelativePath: "foo.md", BlobSHA: "c1", IsPrimary: true}},
				},
			},
		},
		{
			name: "compound-extension convention derives the slug name",
			entries: []repoTreeEntry{
				entry(".github/instructions/testing.instructions.md", "g1"),
			},
			want: []EnumeratedArtifact{{
				SourcePath:   ".github/instructions/testing.instructions.md",
				Name:         "testing", // full ".instructions.md" stripped, not just ".md"
				IdeType:      commonv1.IdeType_IDE_TYPE_COPILOT,
				ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE,
				GroupKey:     ".github/instructions/testing.instructions.md",
				Files:        []EnumeratedArtifactFile{{RelativePath: "testing.instructions.md", BlobSHA: "g1", IsPrimary: true}},
			}},
		},
		{
			name: "single-file rule, CLAUDE.md, and AGENTS.md",
			entries: []repoTreeEntry{
				entry(".claude/rules/comments.md", "r1"),
				entry("CLAUDE.md", "m1"),
				entry("AGENTS.md", "a1"),
			},
			want: []EnumeratedArtifact{
				{
					SourcePath:   ".claude/rules/comments.md",
					Name:         "comments",
					IdeType:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE,
					GroupKey:     ".claude/rules/comments.md",
					Files:        []EnumeratedArtifactFile{{RelativePath: "comments.md", BlobSHA: "r1", IsPrimary: true}},
				},
				{
					SourcePath:   "AGENTS.md",
					Name:         "AGENTS.md",
					IdeType:      commonv1.IdeType_IDE_TYPE_CODEX,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENTS_MD,
					GroupKey:     "AGENTS.md",
					Files:        []EnumeratedArtifactFile{{RelativePath: "AGENTS.md", BlobSHA: "a1", IsPrimary: true}},
				},
				{
					SourcePath:   "CLAUDE.md",
					Name:         "CLAUDE.md",
					IdeType:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD,
					GroupKey:     "CLAUDE.md",
					Files:        []EnumeratedArtifactFile{{RelativePath: "CLAUDE.md", BlobSHA: "m1", IsPrimary: true}},
				},
			},
		},
		{
			name: "directory convention wins over reserved basename; Antigravity agent named by slug",
			entries: []repoTreeEntry{
				entry(".agents/agents/reviewer/agent.md", "ag1"), // Antigravity: <slug>/agent.md → slug is the name
				entry(".claude/rules/CLAUDE.md", "rc1"),          // a rule named CLAUDE.md stays a RULE, not CLAUDE_MD
			},
			want: []EnumeratedArtifact{
				{
					SourcePath:   ".agents/agents/reviewer/agent.md",
					Name:         "reviewer",
					IdeType:      commonv1.IdeType_IDE_TYPE_ANTIGRAVITY,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT,
					GroupKey:     ".agents/agents/reviewer/agent.md",
					Files:        []EnumeratedArtifactFile{{RelativePath: "agent.md", BlobSHA: "ag1", IsPrimary: true}},
				},
				{
					SourcePath:   ".claude/rules/CLAUDE.md",
					Name:         "CLAUDE",
					IdeType:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE,
					GroupKey:     ".claude/rules/CLAUDE.md",
					Files:        []EnumeratedArtifactFile{{RelativePath: "CLAUDE.md", BlobSHA: "rc1", IsPrimary: true}},
				},
			},
		},
		{
			name: "nested files under a flat single-file convention are rejected; direct children kept",
			entries: []repoTreeEntry{
				entry(".claude/rules/frontend/react.md", "fr1"), // nested rule → rejected (desktop scans direct children only)
				entry(".cursor/commands/team/deploy.md", "cc1"), // nested command → rejected
				entry(".claude/rules/comments.md", "r1"),        // direct child → kept
			},
			want: []EnumeratedArtifact{
				{
					SourcePath:   ".claude/rules/comments.md",
					Name:         "comments",
					IdeType:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE,
					GroupKey:     ".claude/rules/comments.md",
					Files:        []EnumeratedArtifactFile{{RelativePath: "comments.md", BlobSHA: "r1", IsPrimary: true}},
				},
			},
		},
		{
			name: "a helper whose path matches an inner convention resolves to its skill package, not a phantom rule",
			entries: []repoTreeEntry{
				entry(".claude/skills/deploy/SKILL.md", "sp1"),
				entry(".claude/skills/deploy/.cursor/rules/example.md", "sp2"), // matches .cursor/rules/ but is a member of deploy, not a standalone Cursor rule
			},
			want: []EnumeratedArtifact{{
				SourcePath:   ".claude/skills/deploy/SKILL.md",
				Name:         "deploy",
				IdeType:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
				ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
				GroupKey:     ".claude/skills/deploy",
				Files: []EnumeratedArtifactFile{
					{RelativePath: ".cursor/rules/example.md", BlobSHA: "sp2", IsPrimary: false}, // sorts before SKILL.md ('.' < 'S')
					{RelativePath: "SKILL.md", BlobSHA: "sp1", IsPrimary: true},
				},
			}},
		},
		{
			name: "hook manifest is excluded (shared manifest needs per-hook parsing, no contract yet)",
			entries: []repoTreeEntry{
				entry(".codex/hooks.json", "h1"), // a shared-manifest hook config with no per-hook contract — excluded (unlike .claude/settings.json, the carrier)
				entry(".claude/rules/comments.md", "r1"),
			},
			want: []EnumeratedArtifact{
				{
					SourcePath:   ".claude/rules/comments.md",
					Name:         "comments",
					IdeType:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE,
					GroupKey:     ".claude/rules/comments.md",
					Files:        []EnumeratedArtifactFile{{RelativePath: "comments.md", BlobSHA: "r1", IsPrimary: true}},
				},
			},
		},
		{
			name: "agents are one file per slug; nested/extra files under an agents dir are rejected",
			entries: []repoTreeEntry{
				entry(".claude/agents/reviewer.md", "a1"),       // flat <slug>.md → valid agent "reviewer"
				entry(".claude/agents/team/reviewer.md", "a2"),  // nested → not a Claude agent, rejected
				entry(".agents/agents/planner/agent.md", "a3"),  // Antigravity <slug>/agent.md → valid "planner"
				entry(".agents/agents/planner/README.md", "a4"), // extra file in the slug dir → rejected
			},
			want: []EnumeratedArtifact{
				{
					SourcePath:   ".agents/agents/planner/agent.md",
					Name:         "planner",
					IdeType:      commonv1.IdeType_IDE_TYPE_ANTIGRAVITY,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT,
					GroupKey:     ".agents/agents/planner/agent.md",
					Files:        []EnumeratedArtifactFile{{RelativePath: "agent.md", BlobSHA: "a3", IsPrimary: true}},
				},
				{
					SourcePath:   ".claude/agents/reviewer.md",
					Name:         "reviewer",
					IdeType:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT,
					GroupKey:     ".claude/agents/reviewer.md",
					Files:        []EnumeratedArtifactFile{{RelativePath: "reviewer.md", BlobSHA: "a1", IsPrimary: true}},
				},
			},
		},
		{
			name: "cursor .md rule and codex .md agent are recognized (ext parity with desktop)",
			entries: []repoTreeEntry{
				entry(".cursor/rules/style.md", "cr1"),    // .md rule, not just .mdc
				entry(".codex/agents/refactor.md", "ca1"), // .md agent, not just .toml
			},
			want: []EnumeratedArtifact{
				{
					SourcePath:   ".codex/agents/refactor.md",
					Name:         "refactor",
					IdeType:      commonv1.IdeType_IDE_TYPE_CODEX,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_AGENT,
					GroupKey:     ".codex/agents/refactor.md",
					Files:        []EnumeratedArtifactFile{{RelativePath: "refactor.md", BlobSHA: "ca1", IsPrimary: true}},
				},
				{
					SourcePath:   ".cursor/rules/style.md",
					Name:         "style",
					IdeType:      commonv1.IdeType_IDE_TYPE_CURSOR,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE,
					GroupKey:     ".cursor/rules/style.md",
					Files:        []EnumeratedArtifactFile{{RelativePath: "style.md", BlobSHA: "cr1", IsPrimary: true}},
				},
			},
		},
		{
			name: "per-file hooks are enumerated; a shared-manifest hook is excluded",
			entries: []repoTreeEntry{
				entry(".codex/hooks.json", "h1"),               // shared-manifest hook config (no per-hook contract) → excluded
				entry(".kiro/hooks/pre-bash.kiro.hook", "kh1"), // per-file Kiro hook → HOOK "pre-bash"
				entry(".github/hooks/on-save.json", "gh1"),     // per-file Copilot hook → HOOK "on-save"
				entry(".github/hooks/nested/x.json", "gh2"),    // nested → not a per-slug hook, rejected
			},
			want: []EnumeratedArtifact{
				{
					SourcePath:   ".github/hooks/on-save.json",
					Name:         "on-save",
					IdeType:      commonv1.IdeType_IDE_TYPE_COPILOT,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_HOOK,
					GroupKey:     ".github/hooks/on-save.json",
					Files:        []EnumeratedArtifactFile{{RelativePath: "on-save.json", BlobSHA: "gh1", IsPrimary: true}},
				},
				{
					SourcePath:   ".kiro/hooks/pre-bash.kiro.hook",
					Name:         "pre-bash",
					IdeType:      commonv1.IdeType_IDE_TYPE_KIRO,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_HOOK,
					GroupKey:     ".kiro/hooks/pre-bash.kiro.hook",
					Files:        []EnumeratedArtifactFile{{RelativePath: "pre-bash.kiro.hook", BlobSHA: "kh1", IsPrimary: true}},
				},
			},
		},
		{
			name: "hidden dot-prefixed entries under a convention are skipped (match desktop)",
			entries: []repoTreeEntry{
				entry(".cursor/rules/.scratch.mdc", "hd1"),     // hidden rule → skipped
				entry(".claude/skills/.draft/SKILL.md", "hd2"), // hidden skill package → skipped
				entry(".claude/rules/real.md", "hd3"),          // real rule → kept
			},
			want: []EnumeratedArtifact{
				{
					SourcePath:   ".claude/rules/real.md",
					Name:         "real",
					IdeType:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
					ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE,
					GroupKey:     ".claude/rules/real.md",
					Files:        []EnumeratedArtifactFile{{RelativePath: "real.md", BlobSHA: "hd3", IsPrimary: true}},
				},
			},
		},
		{
			name: "generated stubs inside a skill package are skipped",
			entries: []repoTreeEntry{
				entry(".claude/skills/deploy/SKILL.md", "gs1"),
				entry(".claude/skills/deploy/gen/api.pb.go", "gs2"),  // generated → skipped
				entry(".claude/skills/deploy/gen/api_pb.ts", "gs3"),  // generated → skipped
				entry(".claude/skills/deploy/scripts/run.sh", "gs4"), // authored → kept
			},
			want: []EnumeratedArtifact{{
				SourcePath:   ".claude/skills/deploy/SKILL.md",
				Name:         "deploy",
				IdeType:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
				ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
				GroupKey:     ".claude/skills/deploy",
				Files: []EnumeratedArtifactFile{
					{RelativePath: "SKILL.md", BlobSHA: "gs1", IsPrimary: true},
					{RelativePath: "scripts/run.sh", BlobSHA: "gs4", IsPrimary: false},
				},
			}},
		},
		{
			name: "claude settings.json is enumerated as a single-file fetch carrier; other shared-manifest hook configs are not",
			entries: []repoTreeEntry{
				entry(".claude/settings.json", "s1"), // the settings carrier — enumerated (body fanned out at the content stage)
				entry(".codex/hooks.json", "c1"),     // another shared-manifest hook config — still excluded
			},
			want: []EnumeratedArtifact{{
				SourcePath:   ".claude/settings.json",
				Name:         "settings.json",
				IdeType:      commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
				ArtifactType: commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_HOOK,
				GroupKey:     ".claude/settings.json",
				Files:        []EnumeratedArtifactFile{{RelativePath: "settings.json", BlobSHA: "s1", IsPrimary: true}},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(repoTreeBlob{TreeSHA: "tree", Entries: tt.entries})
			require.NoError(t, err)
			got, err := EnumerateArtifactFiles(raw)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
