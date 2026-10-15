// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// Conflict is the deterministic (content-only, no LLM) sibling of the Instruction-Intent lens: it
// finds committed harness content that contradicts the repo it governs — a rule glob that matches
// no file, a path reference that resolves to nothing, a rule-ID defined twice, a hard-gated ledger
// rule restated in the root instruction file, and another vendor's config surfaced as authority in
// a Claude-loaded file. Every finding carries dimension CONFLICT, confidence 1.0 (deterministic),
// surface_state VISIBLE, recommendation_source DETECTED.
//
// Every threshold/heuristic below is PROVISIONAL: no in-repo reference implementation exists and the
// spec specifies no values, so the shapes (which frontmatter keys hold globs, what counts as a path
// token, what a "definition position" or a "hard-gated" ledger entry is, the vendor-marker seed
// list) are engineer-chosen calibration placeholders. Every lens is deliberately conservative — an
// ambiguous token or entry is never flagged, trading recall for precision.

// Conflict finding families — the sub-detector that fired, threaded into the slug and used to pick
// the human title. Stable identifiers, not user copy.
const (
	conflictFamilyDeadGlob        = "dead-glob"
	conflictFamilyBrokenPath      = "broken-path"
	conflictFamilyDupID           = "dup-id"
	conflictFamilyRootRestatement = "root-restatement"
	conflictFamilyVendorSurface   = "vendor-surface"
)

// ruleIDDefinition matches a rule-ID in DEFINITION position at the start of a (marker-stripped) line:
// the rule-ID (an all-caps prefix, a hyphen, then digits — e.g. GO-018, PROTO-003), an optional
// bold-close `**`, then a REQUIRED definition delimiter — a colon or an em/en-dash. Requiring the
// delimiter is what separates a real ledger definition (`GO-018: ...`, `- **GO-018**: ...`,
// `GO-018 — ...`) from a bare technical-term heading (`## SHA-256 encoding`, `UTF-8`, `HTTP-2`), which
// carries the same token shape but no definition delimiter and must not read as a rule definition. The
// capture group is the bare rule-ID.
var ruleIDDefinition = regexp.MustCompile(`^([A-Z][A-Z0-9]*-[0-9]+)(?:\*\*)?\s*[:–—]`)

// ruleIDAnywhere matches a rule-ID token anywhere in a line (word-bounded) — used to read the
// ledger's hard-gated set and to scan the root instruction file for restated ids, where the id
// appears mid-line rather than at a definition lead.
var ruleIDAnywhere = regexp.MustCompile(`\b[A-Z][A-Z0-9]*-[0-9]+\b`)

// hardMarkerRegex builds a whole-word alternation over the configured hard-gate marker tokens
// (DetectorConfig.HardMarkers, by default `hard-gated` and `blocker`). The bare word `hard` is
// deliberately not a default marker: it appears in ordinary soft-row prose
// (`| GO-006 | error handling is hard | soft |`), so treating it as a gate marker would wrongly
// hard-gate that rule. Word-bounded so "hardening"/"hardware" in a soft row also does not read as
// hard-gated. An empty marker set yields a never-matching regex, so a ledger with no configured
// markers degrades to no hard-gated rules.
func hardMarkerRegex(markers []string) *regexp.Regexp {
	if len(markers) == 0 {
		return regexp.MustCompile(`\b\B`) // a position cannot be both a word boundary and not one
	}
	quoted := make([]string, 0, len(markers))
	for _, marker := range markers {
		quoted = append(quoted, regexp.QuoteMeta(marker))
	}
	return regexp.MustCompile(`\b(` + strings.Join(quoted, "|") + `)\b`)
}

// trailingLineSuffix matches a trailing `:<digits>` line-number citation decoration (the `:42` in
// `backend/main.go:42`) the broken-path lens strips before resolving a token against the repo tree.
// Anchored at end and digits-only, so a Windows drive prefix (`C:`) is never mangled — repo paths
// never carry one regardless.
var trailingLineSuffix = regexp.MustCompile(`:[0-9]+$`)

// markdownLink captures the href of a Markdown inline link `[label](href)`. The broken-path lens
// pulls the raw href out as its own candidate token; without this a `[label](path/to/x.md)` field
// tokenizes to the garbled `label](path/to/x.md`, which never resolves and false-positives.
var markdownLink = regexp.MustCompile(`\]\(([^)]+)\)`)

// boxDrawingChars are the Unicode box-drawing runes a tree-diagram fragment is built from; a line
// containing any of them is a rendered directory tree, not a set of path references. ASCII trees
// (built from `|`/“ ` “/`+`/`-`) are caught by isASCIITreeLine instead.
const boxDrawingChars = "├└│─┌┐┘┬┴┤┼"

// pathLikeExtensions are the file extensions that make a slash-less token look like a path
// reference (PROVISIONAL). A token with none of these and no "/" is not path-shaped.
var pathLikeExtensions = map[string]struct{}{
	".md": {}, ".go": {}, ".ts": {}, ".tsx": {}, ".js": {}, ".jsx": {}, ".py": {},
	".java": {}, ".kt": {}, ".rb": {}, ".rs": {}, ".c": {}, ".h": {}, ".cpp": {},
	".cc": {}, ".hpp": {}, ".cs": {}, ".php": {}, ".swift": {}, ".scala": {},
	".sql": {}, ".sh": {}, ".proto": {}, ".yaml": {}, ".yml": {}, ".json": {},
	".toml": {}, ".mdc": {}, ".txt": {},
}

// vendorSurfaceMarkers are the PATH-LIKE config markers of ANOTHER AI vendor's setup surfaced as
// authority inside a Claude-loaded file (PROVISIONAL). Only config PATHS are markers, never bare
// vendor names: a Claude-loaded rule that cites another tool's config path as authority is the real
// signal, whereas prose that merely names "Codex"/"Cursor" (e.g. a legitimate cross-tool
// compatibility doc) is not — so every marker here contains "/" or "." and matches as a
// case-insensitive substring. This is the Go-seed DEFAULT (referenced by defaultDetectorConfig);
// DetectorConfig.VendorSurfaceMarkers replaces it when set, and the engine threads the
// resolved set into matchVendorMarker.
var vendorSurfaceMarkers = []string{
	".cursorrules",
	".cursor/rules",
	".github/copilot-instructions",
	".kiro/steering",
	".kiro/hooks",
	".agents/agents",
	"gemini.md",
	".codex/",
}

// conflictFindings runs the deterministic Conflict lenses over the aggregate's committed artifacts,
// their fetched bodies, and the committed repo file-path set. It is pure over the aggregate (no
// I/O) so the detector logic is unit-testable. An artifact contributes only when its body was
// fetched (present in aggregate.bodies).
func conflictFindings(orgID, repoID, taskExecutionID string, aggregate harnessImproveAggregate, vendorMarkers, hardMarkers []string, ruleLedgerBasename string) []*model.PromptLibraryItem {
	bodied := bodiedConflictArtifacts(aggregate)

	var findings []*model.PromptLibraryItem
	findings = append(findings, deadGlobFindings(orgID, repoID, taskExecutionID, bodied, aggregate)...)
	findings = append(findings, brokenPathFindings(orgID, repoID, taskExecutionID, bodied, aggregate)...)
	findings = append(findings, dupIDFindings(orgID, repoID, taskExecutionID, bodied)...)
	findings = append(findings, rootRestatementFindings(orgID, repoID, taskExecutionID, bodied, hardMarkers, ruleLedgerBasename)...)
	findings = append(findings, vendorSurfaceFindings(orgID, repoID, taskExecutionID, bodied, vendorMarkers)...)
	return findings
}

// bodyRequiredForConflict reports whether an artifact's body is input to a wired Conflict lens. The
// broken-path lens must cover SKILL and COMMAND bodies too (they carry `.md` path references just as
// rules and CLAUDE.md do), so the Conflict set is the instruction set widened by those two types —
// broader than isInstructionArtifact. Dead-glob still filters itself to RULE (frontmatter globs are
// a rule concept), so a SKILL/COMMAND contributes only to the broken-path pass.
func bodyRequiredForConflict(a *model.HarnessArtifact) bool {
	if isInstructionArtifact(a) {
		return true
	}
	switch a.Type {
	case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL,
		commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_COMMAND:
		return true
	default:
		return false
	}
}

// bodiedConflictArtifacts collects the aggregate's Conflict-relevant artifacts that have a fetched
// body, in committed order — the input to the Conflict lenses (broken-path over all of them,
// dead-glob/dup-id/root-restatement/vendor-surface self-filtered to their own types).
func bodiedConflictArtifacts(aggregate harnessImproveAggregate) []bodiedInstructionArtifact {
	return bodiedArtifacts(aggregate, bodyRequiredForConflict)
}

// deadGlobFindings (HIGH): a RULE whose frontmatter scoping glob (Claude `paths:` /-Copilot
// `applyTo:`) matches no committed file. Skipped entirely on a truncated or empty tree — a clipped
// listing cannot prove non-existence.
func deadGlobFindings(orgID, repoID, taskExecutionID string, bodied []bodiedInstructionArtifact, aggregate harnessImproveAggregate) []*model.PromptLibraryItem {
	if aggregate.treeTruncated || len(aggregate.repoPaths) == 0 {
		return nil
	}
	pathsSlice := sortedPathKeys(aggregate.repoPaths)
	var findings []*model.PromptLibraryItem
	for _, bi := range bodied {
		if bi.artifact.Type != commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE {
			continue
		}
		for _, glob := range frontmatterGlobs(bi.body) {
			// A `!`-prefixed entry is an EXCLUSION within a paths list (`["**/*.ts", "!**/*.test.ts"]`),
			// not a scoping glob. Matched on its own it never matches, so treating it as a dead
			// glob is a false positive — skip it.
			if strings.HasPrefix(strings.TrimSpace(glob), "!") {
				continue
			}
			if globMatchesAny(pathsSlice, glob) {
				continue
			}
			findings = append(findings, newConflictFinding(orgID, repoID, taskExecutionID, bi.artifact,
				conflictFamilyDeadGlob, "Rule pattern matches no files in the repo", glob, priorityHigh))
		}
	}
	return findings
}

// brokenPathFindings (MEDIUM): a path reference in an instruction body that resolves to no committed
// file. Skipped on a truncated or empty tree (a clipped listing cannot prove non-existence).
func brokenPathFindings(orgID, repoID, taskExecutionID string, bodied []bodiedInstructionArtifact, aggregate harnessImproveAggregate) []*model.PromptLibraryItem {
	if aggregate.treeTruncated || len(aggregate.repoPaths) == 0 {
		return nil
	}
	tree := newCommittedTree(aggregate.repoPaths)
	var findings []*model.PromptLibraryItem
	for _, bi := range bodied {
		for _, ref := range brokenPathRefs(bi.body, tree, bi.artifact.SourcePath) {
			findings = append(findings, newConflictFinding(orgID, repoID, taskExecutionID, bi.artifact,
				conflictFamilyBrokenPath, "Instruction references a path that does not exist", ref, priorityMedium))
		}
	}
	return findings
}

// dupIDFindings (MEDIUM): a rule-ID DEFINED (not merely mentioned) in ≥2 distinct RULE or SKILL
// artifacts — the spec's canonical case is a rule-ID defined in both `rule-index.md` (a RULE) and a
// skill's rules-reference (a SKILL). Relational over the committed set, not per-artifact. One finding
// per duplicated id, anchored on the first defining artifact, naming EVERY other defining source path
// (not only the second) so the evidence is accurate when a rule is defined in three or more files —
// the finding is advisory (harnessFixLane routes dup-id to laneAdvisory), so its reference point IS
// what the admin acts on. CLAUDE_MD/AGENTS_MD are deliberately NOT scanned here: a root file
// re-carrying a ledger rule is the rootRestatement lens's job (LOW), so scanning them here too would
// double-flag the same restatement.
func dupIDFindings(orgID, repoID, taskExecutionID string, bodied []bodiedInstructionArtifact) []*model.PromptLibraryItem {
	// definers keeps committed order (bodied is committed order) so the primary/other choice is
	// deterministic across runs.
	definers := map[string][]*model.HarnessArtifact{}
	for _, bi := range bodied {
		if bi.artifact.Type != commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE &&
			bi.artifact.Type != commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_SKILL {
			continue
		}
		for ruleID := range definedRuleIDs(bi.body) {
			definers[ruleID] = append(definers[ruleID], bi.artifact)
		}
	}
	duplicated := make([]string, 0, len(definers))
	for ruleID, arts := range definers {
		if len(arts) >= 2 {
			duplicated = append(duplicated, ruleID)
		}
	}
	sort.Strings(duplicated)
	var findings []*model.PromptLibraryItem
	for _, ruleID := range duplicated {
		arts := definers[ruleID]
		primary := arts[0]
		otherPaths := make([]string, 0, len(arts)-1)
		for _, art := range arts[1:] {
			otherPaths = append(otherPaths, art.SourcePath)
		}
		findings = append(findings, newConflictFinding(orgID, repoID, taskExecutionID, primary,
			conflictFamilyDupID, "Rule ID defined in more than one file",
			fmt.Sprintf("%s also defined in %s", ruleID, strings.Join(otherPaths, ", ")), priorityMedium))
	}
	return findings
}

// rootRestatementFindings (LOW): a rule-ID the committed rule ledger (rule-index.md) marks
// hard-gated that is ALSO restated in a repo-root always-loaded instruction file (CLAUDE.md /
// AGENTS.md) — the root file re-carries a rule the ledger already enforces. Absent ledger → no
// findings (degrade).
func rootRestatementFindings(orgID, repoID, taskExecutionID string, bodied []bodiedInstructionArtifact, hardMarkers []string, ruleLedgerBasename string) []*model.PromptLibraryItem {
	var ledgerBody string
	haveLedger := false
	for _, bi := range bodied {
		if bi.artifact.Type == commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE &&
			strings.EqualFold(path.Base(bi.artifact.SourcePath), ruleLedgerBasename) {
			ledgerBody = bi.body
			haveLedger = true
			break
		}
	}
	if !haveLedger {
		return nil
	}
	hardSet := ledgerHardRuleIDs(ledgerBody, hardMarkerRegex(hardMarkers))
	if len(hardSet) == 0 {
		return nil
	}
	var findings []*model.PromptLibraryItem
	for _, bi := range bodied {
		if !isRootAlwaysLoadedInstruction(bi.artifact) {
			continue
		}
		restated := map[string]struct{}{}
		for _, id := range ruleIDAnywhere.FindAllString(bi.body, -1) {
			if _, hard := hardSet[id]; hard {
				restated[id] = struct{}{}
			}
		}
		for _, id := range sortedPathKeys(restated) {
			findings = append(findings, newConflictFinding(orgID, repoID, taskExecutionID, bi.artifact,
				conflictFamilyRootRestatement, "Always-loaded file repeats an already-enforced rule", id, priorityLow))
		}
	}
	return findings
}

// vendorSurfaceFindings (MEDIUM): a Claude-loaded instruction file (a Claude Code CLAUDE.md or
// rule) that surfaces another AI vendor's config as authority. One finding per file, on the first
// marker matched.
func vendorSurfaceFindings(orgID, repoID, taskExecutionID string, bodied []bodiedInstructionArtifact, vendorMarkers []string) []*model.PromptLibraryItem {
	var findings []*model.PromptLibraryItem
	for _, bi := range bodied {
		if bi.artifact.IdeType != commonv1.IdeType_IDE_TYPE_CLAUDE_CODE {
			continue
		}
		switch bi.artifact.Type {
		case commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_CLAUDE_MD,
			commonv1.HarnessArtifactType_HARNESS_ARTIFACT_TYPE_RULE:
		default:
			continue
		}
		// A cross-tool compatibility doc (e.g. codex-compatibility.md) exists precisely to translate
		// another vendor's config surface, so its vendor-config paths are descriptive, not an authority
		// claim — exclude it so the lens does not false-fire on a repo's own compat doc.
		if isCompatDocBasename(bi.artifact.SourcePath) {
			continue
		}
		if marker, ok := matchVendorMarker(bi.body, vendorMarkers); ok {
			findings = append(findings, newConflictFinding(orgID, repoID, taskExecutionID, bi.artifact,
				conflictFamilyVendorSurface, "File points Claude at another tool's config",
				marker, priorityMedium))
		}
	}
	return findings
}

// frontmatterGlobs reads the leading YAML/TOML frontmatter block's `paths:` (Claude) and `applyTo:`
// (Codex/Copilot) glob values — inline scalar, comma list, inline `[...]` array, or an indented
// block list. Returns nil when there is no (well-terminated) frontmatter.
func frontmatterGlobs(body string) []string {
	block, ok := leadingFrontmatter(body)
	if !ok {
		return nil
	}
	lines := strings.Split(block, "\n")
	var globs []string
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		key := ""
		switch {
		case strings.HasPrefix(trimmed, "paths:"):
			key = "paths:"
		case strings.HasPrefix(trimmed, "applyTo:"):
			key = "applyTo:"
		default:
			continue
		}
		rest := strings.TrimSpace(trimmed[len(key):])
		if rest != "" {
			globs = append(globs, parseGlobValue(rest)...)
			continue
		}
		// Value on following indented list lines: "- <glob>" until the first non-list line.
		for j := i + 1; j < len(lines); j++ {
			item := strings.TrimSpace(lines[j])
			if item != "-" && !strings.HasPrefix(item, "- ") {
				break
			}
			if g := cleanGlobToken(strings.TrimPrefix(item, "-")); g != "" {
				globs = append(globs, g)
			}
		}
	}
	return globs
}

// leadingFrontmatter returns the inner text of a leading `---`-fenced (YAML) or `+++`-fenced (TOML)
// frontmatter block and whether one is present and well-terminated. An unterminated fence yields
// no block (its content is untrusted), so a dead-glob is never inferred from a malformed header.
func leadingFrontmatter(body string) (string, bool) {
	lines := strings.Split(body, "\n")
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	if start >= len(lines) {
		return "", false
	}
	fence := strings.TrimSpace(lines[start])
	if fence != "---" && fence != "+++" {
		return "", false
	}
	for j := start + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == fence {
			return strings.Join(lines[start+1:j], "\n"), true
		}
	}
	return "", false
}

// stripLeadingFrontmatter returns body with its leading `---`-fenced (YAML) or `+++`-fenced (TOML)
// frontmatter block removed (fences and inner metadata), leaving the prose body. A body with no
// well-terminated leading fence is returned unchanged — the broken-path lens then scans all of it.
func stripLeadingFrontmatter(body string) string {
	lines := strings.Split(body, "\n")
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	if start >= len(lines) {
		return body
	}
	fence := strings.TrimSpace(lines[start])
	if fence != "---" && fence != "+++" {
		return body
	}
	for j := start + 1; j < len(lines); j++ {
		if strings.TrimSpace(lines[j]) == fence {
			return strings.Join(lines[j+1:], "\n")
		}
	}
	return body
}

// parseGlobValue splits a frontmatter glob VALUE (the text after `paths:`/`applyTo:`) into globs:
// an inline `[a, b]` array, or a comma-separated / single scalar. Each element is unquoted and
// trimmed.
func parseGlobValue(rest string) []string {
	rest = strings.TrimSpace(rest)
	// Strip a trailing inline comment before the array/scalar split so a `["a"] # note` still parses as
	// an array and a `"a" # note` scalar does not carry the comment into the token.
	rest = stripInlineComment(rest)
	if strings.HasPrefix(rest, "[") && strings.HasSuffix(rest, "]") {
		rest = rest[1 : len(rest)-1]
	}
	var globs []string
	for _, part := range strings.Split(rest, ",") {
		if g := cleanGlobToken(part); g != "" {
			globs = append(globs, g)
		}
	}
	return globs
}

// cleanGlobToken strips a trailing inline comment, then unquotes and trims one glob token — so a
// `"**/*.ts" # only ts` frontmatter value yields the clean glob `**/*.ts` rather than a junk token
// that would never match and false-fire as a dead glob.
func cleanGlobToken(token string) string {
	token = strings.TrimSpace(token)
	token = stripInlineComment(token)
	token = strings.Trim(token, `"'`)
	return strings.TrimSpace(token)
}

// stripInlineComment removes a trailing YAML inline comment (a ` #…` opened by whitespace) from a
// frontmatter value or list item. The leading-whitespace requirement keeps a `#` that is part of a
// token itself (an anchor fragment, a `#`-bearing pattern) from being mistaken for a comment opener.
func stripInlineComment(s string) string {
	if idx := strings.Index(s, " #"); idx >= 0 {
		return s[:idx]
	}
	return s
}

// maxGlobLen and maxGlobExpansion bound a frontmatter glob before the per-path doublestar match. The
// glob comes verbatim from a committed artifact, so it is attacker-influenceable; a long or
// combinatorially-expanding pattern could make doublestar's per-path match super-linear across the tree.
// maxGlobExpansion caps the MULTIPLICATIVE cost directly — doublestar re-expands one brace alternative at
// a time and recurses, so its work is the PRODUCT of per-group alternative counts (`{a,b}{c,d}…` ⇒ 2×2×…,
// NOT a linear operator sum) — which is the shape a bomb exploits. A real scoping glob expands well under
// 16 (e.g. `**/*.{ts,tsx,js,jsx,mjs,cjs,json,md}` = 8), so the bound never trips legitimate input.
// Over-limit is treated as MATCHING (precision-safe: no dead-glob finding), the same fail-open as an
// unparseable pattern.
const (
	maxGlobLen       = 256
	maxGlobExpansion = 64
)

// globExpansionFactor returns an upper bound on how many concrete patterns doublestar expands glob into —
// the PRODUCT over top-level `{…}` groups of that group's alternative count (its comma count + 1). This
// is doublestar's actual cost driver (it substitutes each alternative and recurses on the rest), so it
// bounds the real brace bomb, where a LINEAR operator sum does not (cost is ~k^n, not n). A comma at any
// depth counts toward its enclosing top-level group — a safe OVER-approximation (nested groups can only
// inflate the estimate, never hide cost). The bound is string-level, so it holds for both frontmatter
// forms — the parser comma-splits only the inline form, not the common block-list `paths:\n  - "…"` one,
// which reaches the matcher with commas intact. Returns early once the product passes maxGlobExpansion.
func globExpansionFactor(glob string) int {
	factor, depth, commasInGroup := 1, 0, 0
	for i := 0; i < len(glob); i++ {
		switch glob[i] {
		case '{':
			if depth == 0 {
				commasInGroup = 0
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 {
					factor *= commasInGroup + 1
					if factor > maxGlobExpansion {
						return factor
					}
				}
			}
		case ',':
			if depth > 0 {
				commasInGroup++
			}
		}
	}
	return factor
}

// globMatchesAny reports whether any repo path matches glob, using doublestar's `**` semantics —
// where `**` spans zero-or-more path segments. A frontmatter scoping glob is authored against a
// tool (Claude/Codex) that reads `**` as doublestar, so the detector MUST match it the same way:
// `docs/operations/**/*.md` covers `docs/operations/deployment-sop.md` (a file directly in the
// directory, `**` matching zero segments), and `**/.claude/rules/*.md` covers a root-level
// `.claude/rules/x.md`. A single-`*` matcher (or a doublestar-less engine) forces a leading segment
// after `**/` and false-fires those as dead. Conversely doublestar's `*` matches a SINGLE segment (it
// does not cross `/`) — the standard minimatch semantics the target tools (Claude paths /-Copilot
// applyTo) use — so `docs/*.md` covers `docs/x.md` but not `docs/sub/y.md`. doublestar.Match re-parses glob on every call (v4 exposes
// no compile-once API), so a glob that matches nothing scans the whole (maxRepoPathsEntries-capped)
// tree at O(paths) matches — acceptable for short repo paths. A malformed pattern, or one over the
// length/expansion bound, cannot be proven to match nothing, so it is treated as matching — a typo'd or
// pathological glob degrades to no finding rather than a false dead-glob (precision over recall). The
// pattern is validated ONCE up front (ValidatePattern → fail-open); the per-path loop then uses
// MatchUnvalidated, which returns a bare bool (no per-path error to handle).
func globMatchesAny(paths []string, glob string) bool {
	if len(glob) > maxGlobLen || globExpansionFactor(glob) > maxGlobExpansion || !doublestar.ValidatePattern(glob) {
		return true
	}
	for _, repoPath := range paths {
		if doublestar.MatchUnvalidated(glob, repoPath) {
			return true
		}
	}
	return false
}

// brokenPathRefs returns the DISTINCT path references in body that resolve to no committed file,
// in first-seen order. It fence-blanks the body (so a path inside a ``` example is not a live
// reference); an unbalanced fence leaves the scan untrustworthy (the rest of the body was swallowed
// or spuriously kept), so it emits nothing rather than risk a false positive. Per surviving line it
// skips rendered tree diagrams (Unicode and ASCII) and make-target invocations, extracts Markdown
// link hrefs as their own candidates, then applies classifyRef to each candidate before resolving
// survivors against the committed tree.
func brokenPathRefs(body string, tree committedTree, sourcePath string) []string {
	// A reference is grounded two ways before it is called broken, so a file the agent CAN follow is
	// never flagged: against the referencing artifact's OWN directory (a `./x`/`../../docs/x.md` link
	// via resolveRefPath) and by segment-aligned TAIL (a partial path or a href's basename via
	// suffixResolves). tree carries both the committed path set and its basename index, built once by
	// the caller (newCommittedTree).
	sourceDir := path.Dir(sourcePath)
	// Strip the leading YAML/TOML frontmatter block first, so a `paths:` scoping glob is owned only
	// by the dead-glob lens and never re-flagged here as a prose path reference (one declaration,
	// one finding).
	blanked, balanced := fenceBlank(stripLeadingFrontmatter(body))
	if !balanced {
		return nil
	}
	seen := map[string]struct{}{}
	var unresolved []string
	for _, line := range strings.Split(blanked, "\n") {
		if strings.ContainsAny(line, boxDrawingChars) || isASCIITreeLine(line) {
			continue // a rendered directory tree, not path references
		}
		// Candidate tokens: each Markdown-link href (pulled out of its `[label](href)` wrapper) plus
		// the whitespace fields of the line with those wrappers stripped, so the garbled wrapped form
		// (`label](href`) is never itself tested. A href is marked isLink — per Markdown semantics it is
		// unambiguously a path relative to the doc, so a bare href stays eligible while a bare prose word
		// (too ambiguous to be a followable path) is dropped below.
		var candidates []pathCandidate
		for _, match := range markdownLink.FindAllStringSubmatch(line, -1) {
			candidates = append(candidates, pathCandidate{token: match[1], isLink: true})
		}
		fields := strings.Fields(markdownLink.ReplaceAllString(line, " "))
		for idx, raw := range fields {
			if precededByMake(fields, idx) {
				continue // a make-target invocation, not a file path
			}
			candidates = append(candidates, pathCandidate{token: raw})
		}
		for _, cand := range candidates {
			token := stripFragmentQuery(trimPathToken(cand.token))
			if !classifyRef(token, line) {
				continue
			}
			// A bare token with no path separator is a NAME, not a followable path — a framework
			// proper-noun (`Next.js`) or an illustrative example (`mock.go`) — so a bare PROSE token is
			// dropped. A bare Markdown-link href is the exception: `[runbook](runbook.md)` is a real path
			// relative to the doc, so it stays eligible and is flagged when it resolves to nothing.
			if !cand.isLink && !strings.Contains(token, "/") {
				continue
			}
			// resolveRefPath grounds a `./`/`../` reference against the artifact dir and a root-relative
			// one against the root; suffixResolves then grounds a partial-path or href basename against
			// any committed path that ends on that tail (segment-aligned). Only a reference that resolves
			// NOWHERE is a genuine broken link (precision over recall).
			if _, ok := tree.paths[resolveRefPath(token, sourceDir)]; ok {
				continue
			}
			if suffixResolves(token, tree.byBasename) {
				continue
			}
			if _, dup := seen[token]; dup {
				continue
			}
			seen[token] = struct{}{}
			unresolved = append(unresolved, token)
		}
	}
	return unresolved
}

// pathCandidate is one broken-path candidate token plus whether it came from a Markdown-link href.
// A bare href resolves against the doc's directory per Markdown semantics, so it stays eligible where
// a bare prose word — genuinely ambiguous — is dropped.
type pathCandidate struct {
	token  string
	isLink bool
}

// committedTree is the committed repo file set in the two forms the broken-path resolver needs: the
// full path set (exact/relative lookups) and the same paths bucketed by basename (suffix resolution).
// Both are built by newCommittedTree from ONE source, so a caller cannot pass a byBasename that
// disagrees with paths — the cross-field invariant is enforced by construction, not by convention.
type committedTree struct {
	paths      map[string]struct{}
	byBasename map[string][]string
}

// newCommittedTree builds the resolver's view of the committed tree ONCE per repo scan (the scan fans
// over many bodied artifacts against the same tree), so the O(paths) basename index is not rebuilt per
// artifact.
func newCommittedTree(repoPaths map[string]struct{}) committedTree {
	byBasename := make(map[string][]string, len(repoPaths))
	for repoPath := range repoPaths {
		base := path.Base(repoPath)
		byBasename[base] = append(byBasename[base], repoPath)
	}
	return committedTree{paths: repoPaths, byBasename: byBasename}
}

// suffixResolves reports whether token is a segment-aligned TAIL of SOME committed path — the file it
// names exists, just cited by a partial path (`dao/x.go` for `a/b/dao/x.go`) or, for a bare href, a
// basename, rather than its full root-relative path. byBasename buckets committed paths by basename; a
// same-basename path resolves token when it ends with `/token`, so segments align (`dao/x.go` matches
// `a/b/dao/x.go` but not `a/mydao/x.go`). First match wins — ambiguity across multiple matches is
// intentionally tolerated, since any match proves the file is followable. An exact-path match is caught
// upstream by the paths lookup, so only the suffix case reaches here; a leading `/` is stripped so an
// absolute-style citation grounds the same way.
func suffixResolves(token string, byBasename map[string][]string) bool {
	token = strings.TrimPrefix(token, "/")
	if token == "" {
		return false
	}
	for _, repoPath := range byBasename[path.Base(token)] {
		if strings.HasSuffix(repoPath, "/"+token) {
			return true
		}
	}
	return false
}

// isASCIITreeLine reports whether a line (after leading whitespace) opens an ASCII directory-tree
// branch — `|--`, “ `-- “, `+--`, or `-- ` — the ASCII counterpart to boxDrawingChars. Such a line
// renders a tree, not path references, so the path scan skips it.
func isASCIITreeLine(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	return strings.HasPrefix(trimmed, "|--") ||
		strings.HasPrefix(trimmed, "`--") ||
		strings.HasPrefix(trimmed, "+--") ||
		strings.HasPrefix(trimmed, "-- ")
}

// precededByMake reports whether the field at idx is a make TARGET — the token walks left over only
// make's own flags (`-C <dir>`, `-f <file>`, `-jN`) and bottoms out at the `make` command itself
// (`make dist/bundle.js`, `make -C backend dist/bundle.js`). It stops at the first field that is
// neither `make` nor a make flag, so a path-shaped word that merely follows the ENGLISH verb "make"
// (`make sure to update docs/gone.md`) is NOT suppressed — only a genuine build target is.
// precededByMake reports whether any field left of idx on the line is the token `make`, in which
// case the candidate is treated as a make target and not a path reference. This deliberately
// OVER-suppresses: it also drops a genuine path reference that merely follows the word "make" in
// prose ("make sure to update docs/gone.md"). That is the precision-safe direction for this
// detector — a make invocation can carry multiple targets (`make proto gen/x.pb.go`,
// `make clean dist/a.js dist/b.js`) whose non-first targets are indistinguishable from a
// prose-following path, and a false NEGATIVE (a missed broken-path finding) is the acceptable
// failure mode here where a false POSITIVE (flagging a build target) is noise.
func precededByMake(fields []string, idx int) bool {
	for j := idx - 1; j >= 0; j-- {
		// Trim markdown/shell wrappers so a backticked command span (`` `make dist/bundle.js` ``,
		// which tokenizes the first field to `` `make``) is still recognized as a make invocation.
		if strings.Trim(fields[j], "`'\"()") == "make" {
			return true
		}
	}
	return false
}

// fenceBlank replaces the CONTENT of every ```-fenced block (and the fence delimiter lines
// themselves) with empty lines, preserving the total line count so per-line scanning is unaffected.
// balanced is false when a ``` fence was left open at end-of-scan — an unbalanced fence (a typo or a
// body truncated mid-fence) makes the blanked output untrustworthy, so callers must not derive
// findings from it.
func fenceBlank(body string) (blanked string, balanced bool) {
	lines := strings.Split(body, "\n")
	inFence := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			lines[i] = ""
			continue
		}
		if inFence {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n"), !inFence
}

// trimPathToken strips a token's surrounding markdown/quote wrappers and trailing sentence
// punctuation, WITHOUT touching a leading dot — ".claude/rules/x.md" is a real repo-relative path,
// not to be mistaken for a punctuation prefix.
func trimPathToken(raw string) string {
	token := strings.Trim(raw, "`*_\"'()[]{}<>")
	token = strings.TrimRight(token, ".,;:!?")
	return strings.TrimSpace(token)
}

// stripFragmentQuery cuts a token at its first `#` (anchor fragment) or `?` (query string), then
// cuts a trailing `:<line>` number suffix, so a decorated citation like
// "docs/ARCHITECTURE.md#deployment" or "backend/main.go:42" resolves to the plain committed file.
func stripFragmentQuery(token string) string {
	if idx := strings.IndexAny(token, "#?"); idx >= 0 {
		token = token[:idx]
	}
	return trailingLineSuffix.ReplaceAllString(token, "")
}

// resolveRepoPath maps a token to the form the committed git tree lists it under: git-tree paths are
// repo-relative with no leading slash, so both a `./` relative prefix and a leading `/` are stripped
// before the repoPaths lookup. Used by the mechanical patcher, which needs only the basename/stem.
func resolveRepoPath(token string) string {
	token = strings.TrimPrefix(token, "./")
	return strings.TrimPrefix(token, "/")
}

// resolveRefPath maps a reference token to the git-tree key form for the repoPaths lookup, resolving
// a file-relative reference against the referencing artifact's directory. A `./x` or `../x` token is
// joined onto sourceDir; any other token is repo-root-relative (a leading `/` stripped). path.Clean
// collapses `.`/`..`/`//` segments to the canonical form the committed tree lists — so a
// `../../docs/x.md` link from a nested file resolves to its real root-relative path and matches a
// committed file (not a false broken-path finding), a non-clean `a//b.md` normalizes to match, and a
// reference that escapes the repo root cleans to a leading `..` matching no tree key — the cross-repo
// guard (an out-of-repo link stays unresolved, hence flagged). Mirrors the gate's normalizeRepoPath
// (path.Clean) so the path lens and fix-grounding agree on what a token resolves to.
func resolveRefPath(token, sourceDir string) string {
	if strings.HasPrefix(token, "./") || strings.HasPrefix(token, "../") {
		return path.Clean(path.Join(sourceDir, token))
	}
	return path.Clean(strings.TrimPrefix(token, "/"))
}

// classifyRef reports whether a (pre-trimmed, fragment/query-stripped) token is a real repo-relative
// path reference. It is deliberately conservative — every ambiguous shape is rejected (precision over
// recall):
//   - a token carrying a glob metacharacter (`*`/`?`) is a pattern (a frontmatter scoping glob or a
//     prose wildcard), not a concrete file to resolve;
//   - a URL (contains "://") is a link, not a repo path;
//   - a token with embedded whitespace is not a single path;
//   - the token MUST carry a recognized source extension. A slash-only token with no extension is
//     slash-prose (`read/write`, `input/output`), a directory reference (`backend/app/`), or
//     an HTTP route (`/api/v1/users`) — none of which is a concrete file, so none is resolved. This
//     is the precision floor for a precision-critical detector; `/api/config.json` (a real
//     extension) still resolves, `docs/gone.md` (missing) still flags;
//   - a bare `foo.ts` reference is dropped only when its same-stem sibling `foo.tsx` also appears on
//     the line — extension-alternation noise, not a separate broken file — leaving an unrelated
//     `service.ts` on a `.tsx` line still flagged.
//
// The whole denylist is PROVISIONAL — an early-eyeball calibration set, not a settled contract. Each
// guard below is INDEPENDENT: a token is rejected if ANY clause matches, so the order is for reading,
// not correctness, and a new heuristic is added by dropping in one more guard (with its own rationale
// comment and a test case), never by reasoning about placement.
func classifyRef(token, line string) bool {
	if token == "" {
		return false
	}
	if strings.ContainsAny(token, "*?") {
		return false // a glob/pattern (e.g. a frontmatter scoping glob), not a concrete path
	}
	if strings.ContainsAny(token, "[]{}<>") {
		// A bracketed/angled segment reads as a template placeholder (`docs/prd/[feature-name]-spec.md`,
		// `<env>/config.yml`) — filled in at use time, never a committed file. This deliberately also
		// exempts a real Next.js dynamic-route path (`app/.../[tab]/page.tsx`), so a broken *route-shaped*
		// reference is a knowingly-accepted miss: the placeholder false-positive this prevents is the
		// costlier direction, and the two shapes are not deterministically separable here. trimPathToken
		// strips only SURROUNDING brackets, so an embedded one survives to this check.
		return false
	}
	if strings.ContainsAny(token, "`$()\"'") {
		// An embedded backtick is a multi-path citation the field-split mis-joined (`` `A.go`/`B.proto` ``
		// tokenizes to `A.go`/`B.proto`); `$` is a shell/template variable (`$UPSTREAM:...`); and an
		// embedded paren/quote is a shell-command fragment the split left behind
		// (`$(git rev-parse --show-toplevel)"/.claude/skills/deploy/deploy.sh`). A committed path carries none
		// of these — trimPathToken strips only the SURROUNDING wrappers, so an embedded one survives here.
		return false
	}
	if strings.Contains(token, "...") {
		return false // an elision the writer used to abbreviate a path (`src/.../consumer.go`), not a real path
	}
	if strings.Contains(token, "://") {
		return false // a URL, not a repo path
	}
	if strings.ContainsAny(token, " \t") {
		return false // not a single path token
	}
	if !hasSourceExtension(token) {
		return false // slash-prose, a directory reference, or an HTTP route — not a concrete file
	}
	if strings.TrimSuffix(path.Base(token), path.Ext(token)) == "" {
		// A bare extension with no filename stem (`.md`, `.go`) — hasSourceExtension is satisfied by
		// the extension alone, so a lone `.md` mentioned in prose would otherwise pass. A real file
		// always has a non-empty stem before its extension.
		return false
	}
	if hasPlaceholderRun(token, minPlaceholderRun) {
		// A run of minPlaceholderRun+ identical DATE/SERIAL-placeholder letters (Y/M/D/N/X, case-insensitive)
		// is a template segment (`…/YYYY-MM-DD.md`, `…/yyyy-mm-dd.md`, `NNNNNN_.up.sql`) filled in at use
		// time, never a committed path. An acronym built FROM that alphabet (`DDD`, `NNN`, `MMM`) is also
		// dropped — an accepted false-negative; a non-alphabet acronym (`IEEE754`, `AAA`) survives, since
		// I/E/A are not in the set.
		return false
	}
	if strings.HasSuffix(token, ".ts") && tsxSiblingOnLine(line, token) {
		return false
	}
	return true
}

// placeholderRunChars are the (uppercase) letters a date/serial template placeholder repeats — Y(ear),
// M(onth), D(ay), N(umber), X(generic). hasPlaceholderRun keys on this set, not any letter, so an
// incidental acronym outside it (`IEEE754`, `AAA`) is not mistaken for a placeholder and dropped.
// minPlaceholderRun is the run length that reads as a placeholder — 3 catches `YYY`/`NNN` while a
// 2-letter coincidence (`…MM…`) is left alone; the check is case-insensitive so `yyyy` folds to `YYYY`.
const (
	placeholderRunChars = "YMDNX"
	minPlaceholderRun   = 3
)

// hasPlaceholderRun reports whether token carries a run of n or more consecutive identical
// placeholder-alphabet letters (case-insensitively — `yyyy` counts like `YYYY`) — the shape of a
// `YYYY-MM-DD` / `NNNN` template segment a committed path never has. Go's RE2 regexp has no
// backreference to express "same char repeated", so this is a scan.
func hasPlaceholderRun(token string, n int) bool {
	run := 1
	for i := 1; i < len(token); i++ {
		if token[i] == token[i-1] && strings.IndexByte(placeholderRunChars, upperByte(token[i])) >= 0 {
			run++
			if run >= n {
				return true
			}
			continue
		}
		run = 1
	}
	return false
}

// upperByte folds an ASCII lowercase letter to uppercase, leaving every other byte unchanged.
func upperByte(b byte) byte {
	if b >= 'a' && b <= 'z' {
		return b - ('a' - 'A')
	}
	return b
}

// tsxSiblingOnLine reports whether the line carries a `.tsx` token whose stem (basename without
// extension) equals tsToken's `.ts` stem — the same-file `.ts`/`.tsx` alternation the classifier
// treats as noise. Basenames are compared so "a/foo.ts" and "b/foo.tsx" still count as siblings.
func tsxSiblingOnLine(line, tsToken string) bool {
	stem := strings.TrimSuffix(path.Base(tsToken), ".ts")
	for _, field := range strings.Fields(line) {
		cleaned := stripFragmentQuery(trimPathToken(field))
		if strings.HasSuffix(cleaned, ".tsx") && strings.TrimSuffix(path.Base(cleaned), ".tsx") == stem {
			return true
		}
	}
	return false
}

// hasSourceExtension reports whether a token ends in one of the recognized path-like extensions.
func hasSourceExtension(token string) bool {
	_, ok := pathLikeExtensions[strings.ToLower(path.Ext(token))]
	return ok
}

// definedRuleIDs returns the rule-IDs a body DEFINES — a rule-ID that leads a line carrying an actual
// definition marker: a markdown heading (`#`), a bold span (`**`), or a list item (`- ` / `* ` / `+ `)
// AND is immediately followed by a definition delimiter (`GO-018:` / `- **GO-018**:` / `GO-018 — …`).
// Both gates are required: the leading marker distinguishes a definition line from prose, and the
// delimiter distinguishes a real rule definition from a bare technical-term heading (`## SHA-256
// encoding`) whose token shares the rule-ID shape. A plain-prose mention ("GO-018 was replaced by
// GO-042") satisfies neither, so two files that merely reference the same id do not false-fire dup-id.
func definedRuleIDs(body string) map[string]struct{} {
	ids := map[string]struct{}{}
	for _, rawLine := range strings.Split(body, "\n") {
		lead := strings.TrimSpace(rawLine)
		if lead == "" {
			continue
		}
		hasMarker := false
		if strings.HasPrefix(lead, "#") {
			hasMarker = true
			lead = strings.TrimSpace(strings.TrimLeft(lead, "#"))
		} else if strings.HasPrefix(lead, "- ") || strings.HasPrefix(lead, "* ") || strings.HasPrefix(lead, "+ ") {
			hasMarker = true
			lead = strings.TrimSpace(lead[2:])
		}
		// A bold span is itself a definition marker (standalone or right after a list marker).
		if strings.HasPrefix(lead, "**") {
			hasMarker = true
			lead = strings.TrimSpace(strings.TrimPrefix(lead, "**"))
		}
		if !hasMarker {
			continue
		}
		if match := ruleIDDefinition.FindStringSubmatch(lead); match != nil {
			ids[match[1]] = struct{}{}
		}
	}
	return ids
}

// ledgerHardRuleIDs reads the ledger's hard-gated rule-ID set: rule-IDs on lines that also carry a
// configured hard-gate marker (hardMarker, case-insensitive). A ledger with no such marker yields
// the EMPTY set — the root-restatement lens then degrades to no finding, rather than treating every
// listed rule as hard and false-firing on a root mention of a soft rule.
func ledgerHardRuleIDs(ledgerBody string, hardMarker *regexp.Regexp) map[string]struct{} {
	hard := map[string]struct{}{}
	for _, line := range strings.Split(ledgerBody, "\n") {
		found := ruleIDAnywhere.FindAllString(line, -1)
		if len(found) == 0 {
			continue
		}
		if !hardMarker.MatchString(strings.ToLower(line)) {
			continue
		}
		for _, id := range found {
			hard[id] = struct{}{}
		}
	}
	return hard
}

// isCompatDocBasename reports whether a file's basename marks it a cross-tool compatibility doc — its
// basename contains "compat" (e.g. codex-compatibility.md). Such a doc's whole purpose is to translate
// another vendor's config surface, so the vendor-surface lens excludes it rather than reading its
// descriptive markers as an authority claim. Deliberately broad (any "*compat*" basename): a missed
// vendor-surface finding is the precision-safe direction for this provisional lens.
func isCompatDocBasename(sourcePath string) bool {
	return strings.Contains(strings.ToLower(path.Base(sourcePath)), "compat")
}

// matchVendorMarker returns the first passed-in marker hit in body and whether one matched. Every
// marker is a path-like config fragment, matched as a case-insensitive substring — so a `.codex/`
// path fires but a prose mention of the bare word "Codex" does not. The marker set is the resolved
// detector calibration (config or Go-seed default), threaded from the engine, not the package var.
func matchVendorMarker(body string, vendorMarkers []string) (string, bool) {
	lower := strings.ToLower(body)
	for _, marker := range vendorMarkers {
		if strings.Contains(lower, strings.ToLower(marker)) {
			return marker, true
		}
	}
	return "", false
}

// sortedPathKeys returns a set's keys sorted ascending — a deterministic slice for glob matching
// and stable finding order.
func sortedPathKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// newConflictFinding builds one deterministic Conflict finding via the shared newDeterministicFinding
// builder, stamping dimension CONFLICT and a per-family priority. referencePoint is the concrete
// detail (the glob, path, rule-ID, or marker) and also seeds the slug hash, so one artifact's several
// broken paths each get a distinct slug.
func newConflictFinding(orgID, repoID, taskExecutionID string, artifact *model.HarnessArtifact,
	family, title, referencePoint string, priority int32,
) *model.PromptLibraryItem {
	return newProjectFinding(orgID, repoID, taskExecutionID, artifact,
		promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_CONFLICT,
		priority, conflictSlug(repoID, artifact.ID, family, referencePoint), title, referencePoint)
}

// conflictSlug builds the stable finding slug "<repoID>:conflict:<family>:<hash>" where hash is the
// first 12 hex of sha256(artifactID + ":" + family + ":" + detail). Hashing the DETAIL (the
// path/rule-ID/glob), not just the artifact, keeps one artifact's several findings in the same
// family on distinct slugs so they don't collide.
func conflictSlug(repoID, artifactID, family, detail string) string {
	sum := sha256.Sum256([]byte(artifactID + ":" + family + ":" + detail))
	shortHash := hex.EncodeToString(sum[:])[:12]
	return fmt.Sprintf("%s:conflict:%s:%s", repoID, family, shortHash)
}
