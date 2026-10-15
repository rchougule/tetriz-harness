// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// Local ids for the internal-package Conflict detector tests.
const (
	cOrgID  = "11111111-1111-1111-1111-111111111111"
	cExecID = "22222222-2222-2222-2222-222222222222"
	cRepoID = "33333333-3333-3333-3333-333333333333"
)

// Detector marker config the Conflict tests run against. These are the neutral defaults
// (DefaultDetectorConfig): the hard-gate markers key the root-restatement / hard-rule-ledger
// families, and the ledger basename names the rule-index file those families scan.
var testHardMarkers = []string{"hard-gated", "blocker"}

const testRuleLedgerBasename = "rule-index.md"

// pathSet builds a repo-path set from the given paths.
func pathSet(paths ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		set[p] = struct{}{}
	}
	return set
}

// claudeRule builds a Claude Code RULE artifact at sourcePath.
func claudeRule(id, sourcePath string) *model.HarnessArtifact {
	return &model.HarnessArtifact{
		ID:         id,
		IdeType:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		Type:       commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE,
		SourcePath: sourcePath,
	}
}

func claudeSkill(id, sourcePath string) *model.HarnessArtifact {
	return &model.HarnessArtifact{
		ID:         id,
		IdeType:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		Type:       commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
		SourcePath: sourcePath,
	}
}

// TestConflictFindings_DupID_RuleAndSkill proves the spec's canonical dup-ID case: a rule-ID DEFINED
// in both a RULE (rule-index.md) and a SKILL's rules-reference fires once. A mere mention in the
// skill (no definition marker) does not.
func TestConflictFindings_DupID_RuleAndSkill(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		skillBody string
		wantCount int
	}{
		{
			name:      "rule-ID defined in both a rule and a skill fires once",
			skillBody: "Follow the rules below.\n- **GO-018**: use the shared errgroup, per the ledger.",
			wantCount: 1,
		},
		{
			name:      "rule-ID only mentioned in the skill (no definition marker) does not fire",
			skillBody: "This skill honors GO-018 when spawning goroutines.",
			wantCount: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			arts := []*model.HarnessArtifact{
				claudeRule("a-rule", ".claude/rules/rule-index.md"),
				claudeSkill("a-skill", ".claude/skills/spawn/SKILL.md"),
			}
			bodies := map[string]string{
				"a-rule":  "## GO-018: use errgroup\nThe ledger definition.",
				"a-skill": tt.skillBody,
			}
			agg := harnessImproveAggregate{committed: arts, bodies: bodies, repoPaths: pathSet()}
			findings := conflictFindings(cOrgID, cRepoID, cExecID, agg, vendorSurfaceMarkers, testHardMarkers, testRuleLedgerBasename)
			require.Len(t, findingsInFamily(t, findings, conflictFamilyDupID), tt.wantCount)
		})
	}
}

// findingsInFamily returns the subset of findings whose slug is in the given Conflict family, and
// asserts each one carries the CONFLICT dimension, confidence 1.0, VISIBLE, and DETECTED source.
func findingsInFamily(t *testing.T, findings []*model.PromptLibraryItem, family string) []*model.PromptLibraryItem {
	t.Helper()
	var out []*model.PromptLibraryItem
	for _, finding := range findings {
		if !strings.Contains(finding.Slug, ":conflict:"+family+":") {
			continue
		}
		require.NotNil(t, finding.Dimension)
		require.Equal(t, promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_CONFLICT, *finding.Dimension)
		require.NotNil(t, finding.RecommendationSource)
		require.Equal(t, promptlibraryv1.ArtifactRecommendationSource_ARTIFACT_RECOMMENDATION_SOURCE_DETECTED, *finding.RecommendationSource)
		require.InDelta(t, 1.0, finding.ConfidenceScore, 1e-9)
		require.Equal(t, promptlibraryv1.ArtifactSurfaceState_ARTIFACT_SURFACE_STATE_VISIBLE, finding.SurfaceState)
		require.Equal(t, cRepoID, finding.ScopeValue)
		out = append(out, finding)
	}
	return out
}

// TestBrokenPathRefs_ClassifyRefCorpus is the DoD precision corpus: broken-path must NOT flag a
// tree-diagram fragment (Unicode or ASCII), an HTTP route, a make-target (even with intervening
// flags), a .ts whose same-stem .tsx sits on the line, a URL, a frontmatter glob, a Markdown link or
// anchor/leading-slash reference to an existing file, or a body behind an unbalanced fence — and MUST
// flag a genuinely missing path, whether bare, Markdown-linked, leading-slash, or http-prefixed.
func TestBrokenPathRefs_ClassifyRefCorpus(t *testing.T) {
	t.Parallel()
	repo := pathSet("README.md", "component.tsx", "src/component.tsx", "backend/main.go", "docs/adr/0001-structure.md", "backend/app/util.go", "web/next.config.ts")
	tests := []struct {
		name       string
		body       string
		sourcePath string // the referencing artifact's path; "" ⇒ repo-root (dir ".")
		wantRefs   []string
	}{
		{
			name:     "tree diagram fragment is not a ref",
			body:     "Layout:\n├── src/missing.go\n└── src/other.go",
			wantRefs: nil,
		},
		{
			name:     "ascii tree fragment is not a ref",
			body:     "Layout:\n|-- src/missing.go\n`-- src/other.go",
			wantRefs: nil,
		},
		{
			name:     "http route is not a ref",
			body:     "The handler lives behind /api/v1/users in the gateway.",
			wantRefs: nil,
		},
		{
			name:     "make target arg is not a ref",
			body:     "Compile everything with make dist/bundle.js first.",
			wantRefs: nil,
		},
		{
			name:     "make target with intervening flags is not a ref",
			body:     "Build it with make -C backend dist/bundle.js before shipping.",
			wantRefs: nil,
		},
		{
			// precededByMake OVER-suppresses on purpose: any path after the token "make" on the line is
			// treated as a build target and dropped, even a prose "make sure … docs/gone.md". This is the
			// precision-safe direction (a missed finding beats a false-positive on a real build target);
			// a multi-target `make proto gen/x` would otherwise flag its non-first targets.
			name:     "path after the word make is suppressed (precision-safe over-suppression)",
			body:     "Please make sure to update docs/gone.md before shipping.",
			wantRefs: nil,
		},
		{
			// Path-shaped so it survives the `require a path separator` gate; the .ts is dropped as a
			// same-stem .tsx sibling, and the .tsx resolves against the committed file.
			name:     "path .ts inside a .tsx discussion is not a ref",
			body:     "The logic in src/component.ts now lives in src/component.tsx entirely.",
			wantRefs: nil,
		},
		{
			// An unrelated path-shaped .ts on a .tsx line is still flagged (not a same-stem sibling).
			name:     "unrelated path .ts on a .tsx line is still flagged",
			body:     "We moved src/service.ts logic into the src/component.tsx wrapper.",
			wantRefs: []string{"src/service.ts"},
		},
		{
			name:     "url is not a ref",
			body:     "See the guide at https://example.com/docs/setup.md for details.",
			wantRefs: nil,
		},
		{
			name:     "go package path with http prefix is flagged",
			body:     "The router lives in httpgateway/routes.go today.",
			wantRefs: []string{"httpgateway/routes.go"},
		},
		{
			name:     "fenced path is not a live ref",
			body:     "Example:\n```\nsee gone/inside/fence.md\n```",
			wantRefs: nil,
		},
		{
			name:     "unbalanced fence suppresses all refs",
			body:     "See .claude/rules/gone.md first.\n```\nunclosed example",
			wantRefs: nil,
		},
		{
			name:     "markdown link to existing file is not a ref",
			body:     "See [the entrypoint](backend/main.go) for the setup.",
			wantRefs: nil,
		},
		{
			name:     "markdown link to missing file is flagged",
			body:     "Read the [setup guide](docs/missing.md) first.",
			wantRefs: []string{"docs/missing.md"},
		},
		{
			name:     "anchor fragment to existing file is not a ref",
			body:     "Jump to backend/main.go#L10 for the handler.",
			wantRefs: nil,
		},
		{
			name:     "leading-slash path to existing file is not a ref",
			body:     "The absolute mention /backend/main.go still resolves.",
			wantRefs: nil,
		},
		{
			name:     "leading-slash path with real extension to missing file is flagged",
			body:     "The config lives at /api/config.json in the bundle.",
			wantRefs: []string{"/api/config.json"},
		},
		{
			name:     "genuinely missing dotfile path is flagged",
			body:     "Follow the steps in .claude/rules/gone.md before shipping.",
			wantRefs: []string{".claude/rules/gone.md"},
		},
		{
			name:     "existing path is not flagged",
			body:     "The entrypoint is backend/main.go in this repo.",
			wantRefs: nil,
		},
		{
			name:     "slash prose without extension is not a ref",
			body:     "Follow the read/write and input/output conventions here.",
			wantRefs: nil,
		},
		{
			name:     "directory reference without extension is not a ref",
			body:     "All processors live under internal/engine/ today.",
			wantRefs: nil,
		},
		{
			name:     "missing path with a source extension is still flagged",
			body:     "The doc moved to docs/gone.md last release.",
			wantRefs: []string{"docs/gone.md"},
		},
		{
			name:     "backticked make command target is not a ref",
			body:     "Compile everything with `make dist/bundle.js` before shipping.",
			wantRefs: nil,
		},
		{
			name:     "line-number citation to an existing file is not a ref",
			body:     "See backend/main.go:42 for the handler.",
			wantRefs: nil,
		},
		{
			// C1: a file-relative `../` link that resolves to a committed file is NOT broken.
			name:       "parent-relative link resolving to an existing file is not a ref",
			body:       "See [ADR-0001](../../docs/adr/0001-structure.md) for the model.",
			sourcePath: "backend/app/CLAUDE.md",
			wantRefs:   nil,
		},
		{
			// C1: a `./`-relative link to a sibling that exists is NOT broken.
			name:       "dot-relative link to an existing sibling is not a ref",
			body:       "Reuse the helper in [util](./util.go).",
			sourcePath: "backend/app/CLAUDE.md",
			wantRefs:   nil,
		},
		{
			// C1: a `../` link to a genuinely missing file IS flagged, carrying the raw token.
			name:       "parent-relative link to a missing file is flagged",
			body:       "See [gone](../../docs/adr/9999-missing.md) here.",
			sourcePath: "backend/app/CLAUDE.md",
			wantRefs:   []string{"../../docs/adr/9999-missing.md"},
		},
		{
			// C1: a `../` chain escaping the repo root cleans to a leading `..` matching no tree
			// key — the cross-repo guard: an out-of-repo link stays unresolved (flagged).
			name:       "link escaping the repo root is flagged",
			body:       "See [x](../../../../outside/other.md) elsewhere.",
			sourcePath: "backend/app/CLAUDE.md",
			wantRefs:   []string{"../../../../outside/other.md"},
		},
		{
			// F4: a BARE sibling Markdown link (no ./) resolves relative to the source file's directory
			// per Markdown semantics, so it grounds against the committed sibling instead of being tested
			// as repo-root and falsely flagged.
			name:       "bare sibling markdown link to an existing file is not a ref",
			body:       "Reuse the helper in [util](util.go).",
			sourcePath: "backend/app/CLAUDE.md",
			wantRefs:   nil,
		},
		{
			// A bare Markdown-link href IS a path (relative to the doc) even without a separator, so a
			// missing one is flagged — the require-path-separator gate applies only to bare PROSE, not
			// hrefs. Neither repo-root nor a committed tail resolves it, so it carries the raw href token.
			name:       "bare sibling markdown link to a missing file is flagged",
			body:       "See [gone](missing.go) here.",
			sourcePath: "backend/app/CLAUDE.md",
			wantRefs:   []string{"missing.go"},
		},
		{
			// A PATH-shaped link to a genuinely missing file IS flagged too (neither repo-root nor a
			// committed tail resolved), carrying the raw href token.
			name:       "path-shaped markdown link to a missing file is flagged",
			body:       "See [gone](sub/missing.go) here.",
			sourcePath: "backend/app/CLAUDE.md",
			wantRefs:   []string{"sub/missing.go"},
		},
		{
			// A bare PROSE filename (no path separator) is a name, not a followable path reference, so it is
			// never flagged — whether or not a same-named file exists. (A path-shaped relative reference
			// still grounds against the artifact's dir; see the `./`/`../` link cases above.)
			name:       "bare prose filename is not flagged (require path separator)",
			body:       "The config lives in next.config.ts at the app root.",
			sourcePath: "web/CLAUDE.md",
			wantRefs:   nil,
		},
		{
			// A framework proper-noun that happens to end in a source extension (`Next.js`, `Node.js`) is a
			// name, not a file path — dropped by the require-path-separator gate.
			name:     "framework proper-noun is not a ref",
			body:     "The dashboard is built with Next.js and server components.",
			wantRefs: nil,
		},
		{
			// A path carrying a date/serial template placeholder (a run of identical placeholder-alphabet
			// letters: `YYYY`, `NNNN`) is filled in at use time, never a committed file.
			name:     "date/serial placeholder path is not a ref",
			body:     "Each run writes triage/runs/YYYY-MM-DD.md at completion.",
			wantRefs: nil,
		},
		{
			// The placeholder-run check is case-insensitive: a lowercase `yyyy-mm-dd` is a placeholder too,
			// so it is dropped rather than false-firing as a broken path.
			name:     "lowercase date placeholder path is not a ref",
			body:     "Each run writes logs/yyyy-mm-dd.md at completion.",
			wantRefs: nil,
		},
		{
			// The placeholder alphabet is exactly Y/M/D/N/X: a 3-run of a letter OUTSIDE it (`AAA`) is NOT a
			// placeholder, so a genuinely-missing path carrying such an acronym is still flagged. This locks
			// the alphabet restriction — widening it to any letter would silently drop this case.
			name:     "non-placeholder-alphabet acronym path is still flagged",
			body:     "The rubric lives in docs/AAApolicy.md today.",
			wantRefs: []string{"docs/AAApolicy.md"},
		},
		{
			// A path carrying a template placeholder (`[feature-name]`) is filled in at use time and is
			// never a committed file — the classifier drops it rather than flagging a phantom broken path.
			name:     "path with a template placeholder is not a ref",
			body:     "Write the spec to docs/prd/[feature-name]-spec.md when done.",
			wantRefs: nil,
		},
		{
			// A bare extension with no filename stem (`.md`) satisfies hasSourceExtension by the extension
			// alone; it is prose about a file type, not a concrete file reference.
			name:     "bare extension token with no stem is not a ref",
			body:     "Every rule is authored as a .md file in the rules directory.",
			wantRefs: nil,
		},
		{
			// A partial-path tail (`app/util.go`) that aligns on segment boundaries with a committed
			// file (`backend/app/util.go`) is a followable reference, not a broken one.
			name:     "partial-path tail of a committed file is not a ref",
			body:     "The helper lives in app/util.go today.",
			wantRefs: nil,
		},
		{
			// A bare PROSE basename (no separator, not a link) is dropped by the require-path-separator gate
			// — a bare word is not a followable path — whether or not a same-named file exists. (The
			// suffix-resolution path for a basename is reached only via a link href; see the href cases above.)
			name:     "bare basename in prose is not a ref",
			body:     "Reuse the helper in util.go before adding a new one.",
			wantRefs: nil,
		},
		{
			// Suffix resolution is SEGMENT-aligned: `dao/util.go` must not resolve against
			// `backend/app/util.go` (whose parent is `app`, not `dao`) — a genuine miss flags.
			name:     "segment-misaligned tail is still flagged",
			body:     "See dao/util.go for the query.",
			wantRefs: []string{"dao/util.go"},
		},
		{
			// A backtick-joined multi-path citation (`` `a.go`/`b.proto` ``) the field split mis-tokenizes
			// into one junk token is not a concrete path.
			name:     "backtick-joined multi-path citation is not a ref",
			body:     "Regenerate both `gone.go`/`missing.proto` after editing.",
			wantRefs: nil,
		},
		{
			// A `$`-prefixed shell/template variable reference is not a committed path.
			name:     "shell/template variable reference is not a ref",
			body:     "Read $UPSTREAM/gone.md before syncing.",
			wantRefs: nil,
		},
		{
			// An elision the writer used to abbreviate a path (`src/.../gone.go`) is not a real path.
			name:     "elided path is not a ref",
			body:     "The consumer sits at src/app/.../gone.go in the tree.",
			wantRefs: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.wantRefs, brokenPathRefs(tt.body, newCommittedTree(repo), tt.sourcePath))
		})
	}
}

// TestConflictFindings_BrokenPathEndToEnd proves a broken path flows through conflictFindings as a
// MEDIUM broken-path finding, and that a truncated tree suppresses the lens entirely.
func TestConflictFindings_BrokenPathEndToEnd(t *testing.T) {
	t.Parallel()
	body := "Follow the steps in .claude/rules/gone.md before shipping."
	arts := []*model.HarnessArtifact{claudeRule("a-1", ".claude/rules/x.md")}
	bodies := map[string]string{"a-1": body}

	tests := []struct {
		name      string
		truncated bool
		repoPaths map[string]struct{}
		wantCount int
	}{
		{name: "broken path fires", truncated: false, repoPaths: pathSet("README.md"), wantCount: 1},
		{name: "truncated tree suppresses lens", truncated: true, repoPaths: pathSet("README.md"), wantCount: 0},
		{name: "empty tree suppresses lens", truncated: false, repoPaths: pathSet(), wantCount: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agg := harnessImproveAggregate{committed: arts, bodies: bodies, repoPaths: tt.repoPaths, treeTruncated: tt.truncated}
			findings := conflictFindings(cOrgID, cRepoID, cExecID, agg, vendorSurfaceMarkers, testHardMarkers, testRuleLedgerBasename)
			broken := findingsInFamily(t, findings, conflictFamilyBrokenPath)
			require.Len(t, broken, tt.wantCount)
			for _, finding := range broken {
				require.Equal(t, priorityMedium, finding.Priority)
				require.Equal(t, ".claude/rules/gone.md", finding.Evidence.GetReferencePoint())
			}
		})
	}
}

// TestConflictFindings_DeadGlob: a rule whose frontmatter glob matches no committed file fires a
// HIGH dead-glob; a glob that matches, an empty tree, and a truncated tree do not.
func TestConflictFindings_DeadGlob(t *testing.T) {
	t.Parallel()
	claudeGlobRule := "---\npaths:\n  - \"src/**/*.ts\"\n---\n# rule body\n"
	codexGlobRule := "---\napplyTo: \"**/*.py\"\n---\n# rule body\n"
	// The brace bomb: comma-BEARING alternation groups delivered via the YAML BLOCK-LIST form, which does
	// NOT comma-split (unlike the inline form), so it reaches the matcher intact. 13 `{a,a}` groups expand
	// to 2^13 concrete patterns — the multiplicative cost the expansion-FACTOR bound caps (a linear
	// operator sum would not). Must be treated as matching, not scanned. (A comma-less `{a}{a}…`, factor 1,
	// is deliberately NOT a bomb and is matched normally — single alternatives don't multiply.)
	braceAltBombRule := "---\npaths:\n  - \"" + strings.Repeat("{a,a}", 13) + "\"\n---\n# rule body\n"
	// An unclosed char-class survives glob cleanup, carries no `*`/`?`/`{`/`,` so passes the expansion cap,
	// and makes doublestar.Match return ErrBadPattern → fail-open (treated as matching, no dead-glob).
	malformedGlobRule := "---\npaths: \"[a-\"\n---\n# rule body\n"
	// A glob over the 256-char length cap (with a single wildcard) is fail-open regardless of expansion.
	longGlobRule := "---\npaths: \"" + strings.Repeat("a/", 130) + "*.md\"\n---\n# rule body\n"
	tests := []struct {
		name      string
		body      string
		repoPaths map[string]struct{}
		truncated bool
		wantGlobs []string
	}{
		{
			name:      "claude paths glob with no match fires",
			body:      claudeGlobRule,
			repoPaths: pathSet("README.md", "main.go"),
			wantGlobs: []string{"src/**/*.ts"},
		},
		{
			name:      "codex applyTo glob with no match fires",
			body:      codexGlobRule,
			repoPaths: pathSet("README.md", "app.ts"),
			wantGlobs: []string{"**/*.py"},
		},
		{
			name:      "glob with a match does not fire",
			body:      claudeGlobRule,
			repoPaths: pathSet("src/app/main.ts"),
			wantGlobs: nil,
		},
		{
			// The block-list brace bomb: comma-bearing alternation groups reach the matcher INTACT (the
			// block-list form does not comma-split), and their PRODUCT (2^13) exceeds the expansion cap, so
			// it is treated as matching and never scanned — this run must complete fast (no 2^n doublestar
			// walk). Drop the product bound and this fires (dead-glob) instead, so the test guards the cap.
			name:      "block-list brace-alternation bomb over the expansion cap does not fire",
			body:      braceAltBombRule,
			repoPaths: pathSet("README.md", "main.go"),
			wantGlobs: nil,
		},
		{
			// A malformed glob (unclosed char-class) fails open: doublestar.Match returns ErrBadPattern, so
			// it is treated as matching and never fires as a false dead-glob.
			name:      "malformed glob fails open and does not fire",
			body:      malformedGlobRule,
			repoPaths: pathSet("README.md", "main.go"),
			wantGlobs: nil,
		},
		{
			// A glob over the length cap is fail-open regardless of wildcard count.
			name:      "over-length glob does not fire",
			body:      longGlobRule,
			repoPaths: pathSet("README.md", "main.go"),
			wantGlobs: nil,
		},
		{
			// `**` must span ZERO path segments: a `**/`-prefixed glob covers a root-level file. Under a
			// doublestar-less matcher the leading `**/` forces a segment before `.claude`, false-firing
			// this real match as dead.
			name:      "doublestar prefix glob matching a root-level file does not fire",
			body:      "---\npaths:\n  - \"**/.claude/rules/*.md\"\n---\n# rule body\n",
			repoPaths: pathSet(".claude/rules/docs-conventions.md", "README.md"),
			wantGlobs: nil,
		},
		{
			// `**` between fixed segments must also match zero directories: `docs/operations/**/*.md`
			// covers a file sitting DIRECTLY in `docs/operations/`. A doublestar-less matcher forces an
			// intervening subdir and false-fires this as dead.
			name:      "doublestar mid glob matching a direct-child file does not fire",
			body:      "---\npaths: \"docs/operations/**/*.md\"\n---\n# rule body\n",
			repoPaths: pathSet("docs/operations/deployment-sop.md"),
			wantGlobs: nil,
		},
		{
			// A trailing YAML inline comment is stripped before the glob is cleaned, so a matching glob
			// with a comment does not degrade into a junk token that false-fires as a dead glob.
			name:      "inline-commented glob that matches does not fire",
			body:      "---\npaths: \"**/*.go\" # backend only\n---\n# rule body\n",
			repoPaths: pathSet("backend/main.go"),
			wantGlobs: nil,
		},
		{
			// The comment is stripped, leaving the clean glob — a genuinely dead glob still fires and its
			// referencePoint is the clean glob (not the comment-laden raw token).
			name:      "inline-commented glob that matches nothing still fires with the clean glob",
			body:      "---\npaths: \"**/*.rs\" # rust only\n---\n# rule body\n",
			repoPaths: pathSet("README.md", "main.go"),
			wantGlobs: []string{"**/*.rs"},
		},
		{
			name:      "truncated tree suppresses lens",
			body:      claudeGlobRule,
			repoPaths: pathSet("README.md"),
			truncated: true,
			wantGlobs: nil,
		},
		{
			name:      "empty tree suppresses lens",
			body:      claudeGlobRule,
			repoPaths: pathSet(),
			wantGlobs: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			arts := []*model.HarnessArtifact{claudeRule("a-1", ".claude/rules/scoped.md")}
			agg := harnessImproveAggregate{
				committed:     arts,
				bodies:        map[string]string{"a-1": tt.body},
				repoPaths:     tt.repoPaths,
				treeTruncated: tt.truncated,
			}
			findings := conflictFindings(cOrgID, cRepoID, cExecID, agg, vendorSurfaceMarkers, testHardMarkers, testRuleLedgerBasename)
			dead := findingsInFamily(t, findings, conflictFamilyDeadGlob)
			var gotGlobs []string
			for _, finding := range dead {
				require.Equal(t, priorityHigh, finding.Priority)
				gotGlobs = append(gotGlobs, finding.Evidence.GetReferencePoint())
			}
			require.Equal(t, tt.wantGlobs, gotGlobs)
		})
	}
}

// TestConflictFindings_DupID: a rule-ID defined at a heading in two distinct rule files fires one
// MEDIUM dup-id finding; a rule-ID merely mentioned in the second file does not.
func TestConflictFindings_DupID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		bodyTwo   string
		wantCount int
	}{
		{
			name:      "defined twice fires once",
			bodyTwo:   "## GO-018: always use the shared errgroup.",
			wantCount: 1,
		},
		{
			name:      "mentioned but not defined does not fire",
			bodyTwo:   "Prose that references GO-018 in passing, not a definition.",
			wantCount: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			arts := []*model.HarnessArtifact{
				claudeRule("a-first", ".claude/rules/errgroup.md"),
				claudeRule("a-second", ".claude/rules/concurrency.md"),
			}
			bodies := map[string]string{
				"a-first":  "# GO-018: use errgroup\nThe first definition.",
				"a-second": tt.bodyTwo,
			}
			agg := harnessImproveAggregate{committed: arts, bodies: bodies, repoPaths: pathSet()}
			findings := conflictFindings(cOrgID, cRepoID, cExecID, agg, vendorSurfaceMarkers, testHardMarkers, testRuleLedgerBasename)
			dup := findingsInFamily(t, findings, conflictFamilyDupID)
			require.Len(t, dup, tt.wantCount)
			for _, finding := range dup {
				require.Equal(t, priorityMedium, finding.Priority)
				require.NotNil(t, finding.HarnessArtifactID)
				require.Equal(t, "a-first", *finding.HarnessArtifactID)
				require.Contains(t, finding.Evidence.GetReferencePoint(), "GO-018")
				require.Contains(t, finding.Evidence.GetReferencePoint(), ".claude/rules/concurrency.md")
			}
		})
	}
}

// TestConflictFindings_DupID_ThreeDefiners: a rule-ID defined in THREE files anchors on the first and
// names BOTH other defining paths in the reference point (not just the second) — so the advisory
// evidence is accurate for a 3-plus-file duplicate.
func TestConflictFindings_DupID_ThreeDefiners(t *testing.T) {
	t.Parallel()
	arts := []*model.HarnessArtifact{
		claudeRule("a-first", ".claude/rules/errgroup.md"),
		claudeRule("a-second", ".claude/rules/concurrency.md"),
		claudeRule("a-third", ".claude/rules/goroutines.md"),
	}
	bodies := map[string]string{
		"a-first":  "# GO-018: use errgroup\nThe first definition.",
		"a-second": "## GO-018: always use the shared errgroup.",
		"a-third":  "# GO-018: prefer errgroup over raw goroutines.",
	}
	agg := harnessImproveAggregate{committed: arts, bodies: bodies, repoPaths: pathSet()}
	findings := conflictFindings(cOrgID, cRepoID, cExecID, agg, vendorSurfaceMarkers, testHardMarkers, testRuleLedgerBasename)
	dup := findingsInFamily(t, findings, conflictFamilyDupID)
	require.Len(t, dup, 1)
	require.Equal(t, "a-first", *dup[0].HarnessArtifactID)
	// Both other definers are named — the fix's earlier "name only the second" shape dropped a-third.
	require.Contains(t, dup[0].Evidence.GetReferencePoint(), ".claude/rules/concurrency.md")
	require.Contains(t, dup[0].Evidence.GetReferencePoint(), ".claude/rules/goroutines.md")
}

// TestConflictFindings_RootRestatement: a hard-gated ledger rule restated in the root CLAUDE.md
// fires a LOW root-restatement finding; with no ledger present the lens is silent (degrade).
func TestConflictFindings_RootRestatement(t *testing.T) {
	t.Parallel()
	rootClaude := &model.HarnessArtifact{
		ID:         "a-root",
		IdeType:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		Type:       commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD,
		SourcePath: "CLAUDE.md",
	}
	rootBody := "We strictly enforce GO-018 across every service."
	ledger := claudeRule("a-ledger", ".claude/rules/rule-index.md")
	ledgerBody := "| GO-018 | errgroup | hard-gated |\n| GO-006 | wrap errors | soft |"

	tests := []struct {
		name      string
		arts      []*model.HarnessArtifact
		bodies    map[string]string
		wantIDs   []string
		wantCount int
	}{
		{
			name: "hard-gated rule restated in root fires",
			arts: []*model.HarnessArtifact{ledger, rootClaude},
			bodies: map[string]string{
				"a-ledger": ledgerBody,
				"a-root":   rootBody,
			},
			wantIDs:   []string{"GO-018"},
			wantCount: 1,
		},
		{
			name:      "no ledger means no finding",
			arts:      []*model.HarnessArtifact{rootClaude},
			bodies:    map[string]string{"a-root": rootBody},
			wantCount: 0,
		},
		{
			name: "soft-only ledger yields an empty hard set so no finding",
			arts: []*model.HarnessArtifact{ledger, rootClaude},
			bodies: map[string]string{
				"a-ledger": "| GO-018 | errgroup | soft |\n| GO-006 | wrap errors | guidance |",
				"a-root":   rootBody,
			},
			wantCount: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agg := harnessImproveAggregate{committed: tt.arts, bodies: tt.bodies, repoPaths: pathSet()}
			findings := conflictFindings(cOrgID, cRepoID, cExecID, agg, vendorSurfaceMarkers, testHardMarkers, testRuleLedgerBasename)
			restatement := findingsInFamily(t, findings, conflictFamilyRootRestatement)
			require.Len(t, restatement, tt.wantCount)
			var gotIDs []string
			for _, finding := range restatement {
				require.Equal(t, priorityLow, finding.Priority)
				require.NotNil(t, finding.HarnessArtifactID)
				require.Equal(t, "a-root", *finding.HarnessArtifactID)
				gotIDs = append(gotIDs, finding.Evidence.GetReferencePoint())
			}
			require.Equal(t, tt.wantIDs, gotIDs)
		})
	}
}

// TestConflictFindings_VendorSurface: a Claude-loaded file citing another vendor's config PATH as
// authority fires a MEDIUM vendor-surface finding; a prose mention of a bare vendor name (the shape
// of a legitimate cross-tool compatibility doc), a substring inside a larger word, and a non-Claude
// file do not.
func TestConflictFindings_VendorSurface(t *testing.T) {
	t.Parallel()
	cursorRule := func(id, source string) *model.HarnessArtifact {
		return &model.HarnessArtifact{
			ID:         id,
			IdeType:    commonv1.IdeType_IDE_TYPE_CURSOR,
			Type:       commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE,
			SourcePath: source,
		}
	}
	tests := []struct {
		name       string
		artifact   *model.HarnessArtifact
		body       string
		wantMarker string
		wantCount  int
	}{
		{
			name:       "claude rule citing .cursorrules as authority fires",
			artifact:   claudeRule("a-1", ".claude/rules/x.md"),
			body:       "Defer to the .cursorrules file for formatting.",
			wantMarker: ".cursorrules",
			wantCount:  1,
		},
		{
			name:       "claude rule citing .kiro/steering as authority fires",
			artifact:   claudeRule("a-kiro", ".claude/rules/k.md"),
			body:       "Follow whatever the .kiro/steering docs mandate.",
			wantMarker: ".kiro/steering",
			wantCount:  1,
		},
		{
			name:       "claude rule citing gemini.md as authority fires",
			artifact:   claudeRule("a-gem", ".claude/rules/g.md"),
			body:       "The authoritative guidance lives in GEMINI.md at the root.",
			wantMarker: "gemini.md",
			wantCount:  1,
		},
		{
			name:      "passing word mention of cursor does not fire",
			artifact:  claudeRule("a-2", ".claude/rules/y.md"),
			body:      "Move the cursor to the end of the buffer before saving.",
			wantCount: 0,
		},
		{
			name:      "codex substring inside a larger word does not fire",
			artifact:  claudeRule("a-3", ".claude/rules/z.md"),
			body:      "The codexes of ancient law are unrelated here.",
			wantCount: 0,
		},
		{
			name:      "bare vendor name in compatibility prose does not fire",
			artifact:  claudeRule("a-4", ".claude/rules/w.md"),
			body:      "Codex uses this file as its root guidance; Cursor reads it too.",
			wantCount: 0,
		},
		{
			name:      "non-claude file is not scanned",
			artifact:  cursorRule("a-5", ".cursor/rules/x.mdc"),
			body:      "Defer to the .cursorrules file for formatting.",
			wantCount: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agg := harnessImproveAggregate{
				committed: []*model.HarnessArtifact{tt.artifact},
				bodies:    map[string]string{tt.artifact.ID: tt.body},
				repoPaths: pathSet(),
			}
			findings := conflictFindings(cOrgID, cRepoID, cExecID, agg, vendorSurfaceMarkers, testHardMarkers, testRuleLedgerBasename)
			vendor := findingsInFamily(t, findings, conflictFamilyVendorSurface)
			require.Len(t, vendor, tt.wantCount)
			for _, finding := range vendor {
				require.Equal(t, priorityMedium, finding.Priority)
				require.Equal(t, tt.wantMarker, finding.Evidence.GetReferencePoint())
			}
		})
	}
}

// TestConflictFindings_VendorSurface_CompatDocExcluded: a cross-tool compatibility doc (basename
// contains "compat", e.g. codex-compatibility.md) is excluded from the vendor-surface lens — its
// vendor-config paths are descriptive, not an authority claim — while the SAME marker in an ordinary
// Claude-loaded file (CLAUDE.md) still fires.
func TestConflictFindings_VendorSurface_CompatDocExcluded(t *testing.T) {
	t.Parallel()
	const body = "Codex uses .codex/agents/*.toml as its adapter surface; translate accordingly."
	claudeMD := func(id, source string) *model.HarnessArtifact {
		return &model.HarnessArtifact{
			ID:         id,
			IdeType:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			Type:       commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD,
			SourcePath: source,
		}
	}
	tests := []struct {
		name      string
		artifact  *model.HarnessArtifact
		wantCount int
	}{
		{
			name:      "compat doc with a vendor marker is excluded",
			artifact:  claudeRule("a-compat", ".claude/rules/codex-compatibility.md"),
			wantCount: 0,
		},
		{
			name:      "ordinary CLAUDE.md with the same marker still fires",
			artifact:  claudeMD("a-root", "CLAUDE.md"),
			wantCount: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			agg := harnessImproveAggregate{
				committed: []*model.HarnessArtifact{tt.artifact},
				bodies:    map[string]string{tt.artifact.ID: body},
				repoPaths: pathSet(),
			}
			findings := conflictFindings(cOrgID, cRepoID, cExecID, agg, vendorSurfaceMarkers, testHardMarkers, testRuleLedgerBasename)
			require.Len(t, findingsInFamily(t, findings, conflictFamilyVendorSurface), tt.wantCount)
		})
	}
}

// TestConflictFindings_DupID_DefinitionMarkup: a rule-ID needs an actual definition marker (heading,
// bold, or list item) to count as defined — a plain-prose mention of the same id in two files is not
// a dup, while a heading or bold definition in two files is.
func TestConflictFindings_DupID_DefinitionMarkup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		bodyOne   string
		bodyTwo   string
		wantCount int
	}{
		{
			name:      "plain-prose mention in both files is not a definition",
			bodyOne:   "GO-018 was replaced by GO-042 last quarter.",
			bodyTwo:   "As we noted, GO-018 was replaced by GO-042 last quarter.",
			wantCount: 0,
		},
		{
			name:      "heading definition (colon and em-dash) in both files fires once",
			bodyOne:   "## GO-018: use errgroup\nThe first definition.",
			bodyTwo:   "# GO-018 — the second definition.",
			wantCount: 1,
		},
		{
			name:      "bold and list definition in both files fires once",
			bodyOne:   "- **GO-018**: use the shared errgroup.",
			bodyTwo:   "**GO-018**: defined here as well.",
			wantCount: 1,
		},
		{
			// A bare technical-term heading (`## SHA-256`) shares the rule-ID token shape but carries no
			// definition delimiter, so it is not a definition and two files heading-ing it do not dup.
			name:      "technical-term heading in both files is not a definition",
			bodyOne:   "## SHA-256 encoding\nWe hash with SHA-256 here.",
			bodyTwo:   "# SHA-256 encoding details\nAlso SHA-256 downstream.",
			wantCount: 0,
		},
		{
			// A marker line whose rule-ID has no trailing delimiter (a bold mention, a bare heading) is
			// not a definition even though the marker is present.
			name:      "marked rule-ID without a delimiter is not a definition",
			bodyOne:   "**GO-018** is discussed in passing here.",
			bodyTwo:   "## GO-018\nA bare heading, no delimiter.",
			wantCount: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			arts := []*model.HarnessArtifact{
				claudeRule("a-first", ".claude/rules/one.md"),
				claudeRule("a-second", ".claude/rules/two.md"),
			}
			bodies := map[string]string{"a-first": tt.bodyOne, "a-second": tt.bodyTwo}
			agg := harnessImproveAggregate{committed: arts, bodies: bodies, repoPaths: pathSet()}
			findings := conflictFindings(cOrgID, cRepoID, cExecID, agg, vendorSurfaceMarkers, testHardMarkers, testRuleLedgerBasename)
			dup := findingsInFamily(t, findings, conflictFamilyDupID)
			require.Len(t, dup, tt.wantCount)
		})
	}
}

// TestLedgerHardRuleIDs_WordBounded: the hard-gate marker is matched as a whole word, so an
// `hard-gated`/`blocker` row is hard-gated while a `soft` row that merely contains "hardening" is not.
func TestLedgerHardRuleIDs_WordBounded(t *testing.T) {
	t.Parallel()
	ledger := "| GO-018 | errgroup | hard-gated |\n" +
		"| SEC-010 | security hardening review | soft |\n" +
		"| GO-006 | error wrapping | blocker |\n" +
		"| GO-029 | error handling is hard | soft |"
	hard := ledgerHardRuleIDs(ledger, hardMarkerRegex(testHardMarkers))
	tests := []struct {
		name   string
		ruleID string
		want   bool
	}{
		{name: "hard-gated row is hard-gated", ruleID: "GO-018", want: true},
		{name: "blocker row is hard-gated", ruleID: "GO-006", want: true},
		{name: "soft row containing hardening is not hard-gated", ruleID: "SEC-010", want: false},
		// The bare word "hard" in soft-row prose must NOT hard-gate the rule.
		{name: "soft row containing the bare word hard is not hard-gated", ruleID: "GO-029", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, ok := hard[tt.ruleID]
			require.Equal(t, tt.want, ok)
		})
	}
}

// TestConflictFindings_BrokenPathCoversSkillBody: a SKILL body with a broken .md path yields a
// broken-path CONFLICT finding — the broken-path lens covers skill/command bodies, not only
// instruction files — and the finding carries the SKILL artifact type.
func TestConflictFindings_BrokenPathCoversSkillBody(t *testing.T) {
	t.Parallel()
	skill := &model.HarnessArtifact{
		ID:         "s-1",
		IdeType:    commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
		Type:       commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
		SourcePath: ".claude/skills/deploy/SKILL.md",
	}
	agg := harnessImproveAggregate{
		committed: []*model.HarnessArtifact{skill},
		bodies:    map[string]string{"s-1": "Read .claude/rules/gone.md before running the deploy."},
		repoPaths: pathSet("README.md"),
	}
	findings := conflictFindings(cOrgID, cRepoID, cExecID, agg, vendorSurfaceMarkers, testHardMarkers, testRuleLedgerBasename)
	broken := findingsInFamily(t, findings, conflictFamilyBrokenPath)
	require.Len(t, broken, 1)
	require.Equal(t, ".claude/rules/gone.md", broken[0].Evidence.GetReferencePoint())
	require.Equal(t, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL, broken[0].ArtifactType)
}

// TestConflictFindings_DeadGlobSkipsNegation: a `!`-prefixed exclusion entry in a paths list is not
// a dead glob (an exclusion matched on its own always looks unmatched), so it never fires.
func TestConflictFindings_DeadGlobSkipsNegation(t *testing.T) {
	t.Parallel()
	body := "---\npaths:\n  - \"**/*.ts\"\n  - \"!**/*.test.ts\"\n---\n# rule body\n"
	agg := harnessImproveAggregate{
		committed: []*model.HarnessArtifact{claudeRule("a-1", ".claude/rules/scoped.md")},
		bodies:    map[string]string{"a-1": body},
		repoPaths: pathSet("src/app.ts"),
	}
	findings := conflictFindings(cOrgID, cRepoID, cExecID, agg, vendorSurfaceMarkers, testHardMarkers, testRuleLedgerBasename)
	require.Empty(t, findingsInFamily(t, findings, conflictFamilyDeadGlob))
}

// TestConflictFindings_FrontmatterPathNotDoubleFlagged: a rule whose only path lives in its `paths:`
// frontmatter yields exactly one finding (dead-glob) — the broken-path lens strips the leading
// frontmatter first, so the same declaration is not also flagged as a prose path reference.
func TestConflictFindings_FrontmatterPathNotDoubleFlagged(t *testing.T) {
	t.Parallel()
	body := "---\npaths:\n  - \"src/legacy.ts\"\n---\n# rule body with no other paths\n"
	agg := harnessImproveAggregate{
		committed: []*model.HarnessArtifact{claudeRule("a-1", ".claude/rules/scoped.md")},
		bodies:    map[string]string{"a-1": body},
		repoPaths: pathSet("README.md"),
	}
	findings := conflictFindings(cOrgID, cRepoID, cExecID, agg, vendorSurfaceMarkers, testHardMarkers, testRuleLedgerBasename)
	require.Len(t, findingsInFamily(t, findings, conflictFamilyDeadGlob), 1)
	require.Empty(t, findingsInFamily(t, findings, conflictFamilyBrokenPath))
}

// TestLedgerHardRuleIDs_NoHardMarkerYieldsEmpty: a ledger with only soft rows has an empty hard set
// (degrade), so the root-restatement lens does not fire on a soft rule mentioned in the root file.
func TestLedgerHardRuleIDs_NoHardMarkerYieldsEmpty(t *testing.T) {
	t.Parallel()
	require.Empty(t, ledgerHardRuleIDs("| GO-018 | errgroup | soft |\n| GO-006 | wrap errors | guidance |", hardMarkerRegex(testHardMarkers)))
}

// TestConflictSlug_DistinctPerDetail: two findings on the SAME artifact+family with different
// details get distinct slugs, so one artifact's several broken paths never collide on one slug.
func TestConflictSlug_DistinctPerDetail(t *testing.T) {
	t.Parallel()
	first := conflictSlug(cRepoID, "artifact-x", conflictFamilyBrokenPath, "a/one.md")
	second := conflictSlug(cRepoID, "artifact-x", conflictFamilyBrokenPath, "a/two.md")
	require.NotEqual(t, first, second)
	require.True(t, strings.HasPrefix(first, cRepoID+":conflict:"+conflictFamilyBrokenPath+":"))
}
