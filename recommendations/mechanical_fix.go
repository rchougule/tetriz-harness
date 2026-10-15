// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"path"
	"strings"

	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// fixLane is the fix-producer routing verdict for one finding: a Conflict broken-path finding is
// resolved deterministically in Go, and the content-fix families are drafted by the LLM — the
// Instruction-Intent rewrites (token-floor / tok-per-line / directive-share / behavioral-deviation) and
// the single-file Conflict remove-line families (dead-glob / vendor-surface / root-restatement). Every
// other finding is advisory (no fix) — including dup-id, whose duplicate spans multiple files a per-repo
// fix cannot resolve. See harnessFixLane.
type fixLane int

const (
	// laneAdvisory produces no fix — the finding ships with its evidence only. The default for every
	// family not mapped to a fix lane: dup-id (its duplicate spans multiple files, so no per-repo body
	// rewrite resolves it — which copy to drop is the admin's call), every SECURITY family in this
	// per-repo lane (a permission finding's settings.json body is deliberately not egressed to the LLM;
	// the ORG coverage-gap/mcp fixes are generated in the org path, not here), and any malformed/unmapped
	// family.
	laneAdvisory fixLane = iota
	// laneDeterministic resolves a broken-path rename in Go via PatchBrokenPath — no LLM.
	laneDeterministic
	// laneLLM drafts a body via tetriz-py (GenerateHarnessFix) for a content-fix finding — an
	// Instruction-Intent rewrite, a Conflict remove-line, or a behavioral-deviation realign.
	laneLLM
)

// harnessFixLane routes a finding to its fix lane from its dimension + slug family: a CONFLICT
// broken-path finding is deterministic (Go); the single-file CONFLICT remove-line families (dead-glob /
// vendor-surface / root-restatement) and the INSTRUCTION_INTENT rewrites (token-floor / tok-per-line /
// directive-share, plus behavioral-deviation, which carries dimension INSTRUCTION_INTENT) are LLM; and
// everything else — dup-id (a multi-file duplicate no per-repo fix can resolve), every SECURITY family
// in this per-repo lane, and a nil dimension — is advisory, never routed to a paid/deterministic fix.
// This couples the producer to the slug shape
// (<subject>:<dimension-label>:<family>:<hash>), so a routing guard test asserts each detector's real
// slug maps to the intended lane.
func harnessFixLane(finding *model.PromptLibraryItem) fixLane {
	if finding.Dimension == nil {
		return laneAdvisory
	}
	family := slugFamily(finding.Slug)
	switch *finding.Dimension {
	case promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_CONFLICT:
		switch family {
		case conflictFamilyBrokenPath:
			// A rename resolves to exactly one existing path — deterministic, no LLM.
			return laneDeterministic
		case conflictFamilyDeadGlob, conflictFamilyVendorSurface, conflictFamilyRootRestatement:
			// Remove-the-offending-line fixes: the LLM drops exactly the dead glob / vendor
			// citation / restated rule from the committed body — each is confined to ONE file.
			return laneLLM
		case conflictFamilyDupID:
			// ADVISORY, not LLM: a duplicated rule ID spans ≥2 files, but the finding anchors on the
			// FIRST definer and a per-repo fix can only rewrite that one body — removing the definition
			// there does not resolve a 3-plus-file duplicate, and even the 2-file case would delete the
			// wrong copy if the anchor is the one to keep. Which duplicate to drop is the admin's call,
			// so the finding surfaces with its recommended-action copy (naming every other definer) and
			// no auto-generated fix.
			return laneAdvisory
		case personalFamilyShadowsCommitted:
			// ADVISORY: the subject is one engineer's own (desktop) file. No enforce path writes to an
			// engineer's machine, and a personal body is never egressed to the LLM — the remedy (adopt
			// the team's copy or upstream the personal one) is the engineer's call.
			return laneAdvisory
		default:
			return laneAdvisory
		}
	case promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_INSTRUCTION_INTENT:
		switch family {
		case instructionIntentFamilyTokenFloor, instructionIntentFamilyTokPerLine, instructionIntentFamilyDirectiveShare,
			behavioralFamilyDeviation:
			// Content rewrites: trim/split an oversized file, or align a skill/subagent to its intent.
			return laneLLM
		case personalFamilyNeverUsed:
			// ADVISORY: there is no body to rewrite — the remedy is to use or remove the engineer's own
			// artifact, which only they can do (see personalFamilyShadowsCommitted above).
			return laneAdvisory
		default:
			return laneAdvisory
		}
	case promptlibraryv1.HarnessFindingDimension_HARNESS_FINDING_DIMENSION_SECURITY:
		// SECURITY findings are ADVISORY in this per-repo fix producer — no LLM fix lane. The permission
		// families (permission-grant / permission-default-mode) anchor on the PERMISSIONS artifact, whose
		// committed body is the WHOLE .claude/settings.json (permission rules AND env values / MCP server
		// configs — the highest-secret-density harness file). Routing them to generate_harness_fix would
		// egress that body verbatim to the external LLM and its trace with no redaction, for a fix the
		// serving surface can only Copy anyway (PERMISSIONS is not an enforceable type). So the finding
		// stays advisory and surfaces with its recommended-action copy instead. The ORG-scoped Security
		// families (coverage-gap, mcp) are produced from scratch in RunOrgScan's org path, not this lane.
		return laneAdvisory
	default:
		return laneAdvisory
	}
}

// slugFamily returns a finding slug's family — the 3rd ':'-segment of the shared slug shape
// <subject>:<dimension-label>:<family>:<hash> (every builder emits it). "" when the slug is malformed
// (too few segments), which routes to advisory.
func slugFamily(slug string) string {
	parts := strings.Split(slug, ":")
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

// harnessFixDimensionLabel is the fix input's dimension descriptor — the finding's dimension label
// joined to its slug family (e.g. "instruction_intent:token-floor"), the "dimension + detector family"
// the generate_harness_fix system prompt reads.
func harnessFixDimensionLabel(finding *model.PromptLibraryItem) string {
	return dimensionLabel(finding.Dimension) + ":" + slugFamily(finding.Slug)
}

// The deterministic mechanical-fix lane (the detector spec): a broken-path
// Conflict finding whose rename target is UNAMBIGUOUS is resolved in Go — no LLM. It is the
// counterpart to the generate_harness_fix leg, which handles the content rewrites that are
// not a straight rename. Ambiguity is resolved conservatively: zero or two-plus candidates
// mean the caller falls back to an advisory finding, never a guessed rename.

// PatchBrokenPath resolves a broken path reference to an unambiguous rename against the
// committed repo path set and, when it can, rewrites the reference in body.
//
// brokenRef is the finding's reference_point — the already-cleaned path token the
// broken-path lens extracted (fragment/query and line-suffix stripped; see conflict.go's
// stripFragmentQuery). repoPaths is the committed git-tree path set (the same set the
// broken-path lens resolves against). A candidate is found by either rule:
//
//   - unique basename: exactly one repo path whose basename equals brokenRef's basename
//     (the file moved directories);
//   - extension drift: exactly one repo path with the same basename stem but a different
//     extension (e.g. foo.ts → foo.tsx).
//
// Across BOTH rules combined: exactly one distinct candidate → autoFixable=true, with
// patched = body with every WHOLE-TOKEN occurrence of brokenRef replaced by resolvedPath.
// Zero or two-plus candidates → autoFixable=false with empty patched/resolvedPath, and the
// caller falls back to advisory. The two rules are disjoint per repo path (equal basename ⇒
// equal extension, so a path can satisfy at most one), so a single candidate is genuinely
// singular. A candidate that resolves but whose brokenRef appears only *inside* longer paths
// in body (no whole-token occurrence) also falls back to advisory rather than a no-op patch.
func PatchBrokenPath(brokenRef, body string, repoPaths map[string]struct{}) (patched, resolvedPath string, autoFixable bool) {
	// resolveRepoPath (conflict.go) normalizes a `./` or leading `/` prefix to the
	// git-tree form the repo paths and the basename/stem comparison use.
	normalizedRef := resolveRepoPath(brokenRef)
	brokenBase := path.Base(normalizedRef)
	if brokenBase == "" || brokenBase == "." || brokenBase == "/" {
		return "", "", false
	}
	brokenExt := path.Ext(brokenBase)
	brokenStem := strings.TrimSuffix(brokenBase, brokenExt)

	// A set (not a slice) so a repo path that could match under more than one reading is
	// counted once — the "distinct candidate" the ambiguity check needs.
	candidates := map[string]struct{}{}
	for repoPath := range repoPaths {
		candidateBase := path.Base(repoPath)
		if candidateBase == brokenBase {
			candidates[repoPath] = struct{}{}
			continue
		}
		candidateExt := path.Ext(candidateBase)
		candidateStem := strings.TrimSuffix(candidateBase, candidateExt)
		// Extension drift needs a non-empty stem so two extensionless files (stem "") do
		// not read as same-file variants, and a genuinely different extension.
		if brokenStem != "" && candidateStem == brokenStem && candidateExt != brokenExt {
			candidates[repoPath] = struct{}{}
		}
	}

	if len(candidates) != 1 {
		return "", "", false
	}
	for candidate := range candidates {
		resolvedPath = candidate
	}
	// Replace only WHOLE-TOKEN occurrences of brokenRef (as it appears in body, not the
	// normalized form, so a `./`-prefixed or bare occurrence is rewritten in place). A raw
	// substring replace would corrupt a longer path that merely contains brokenRef — e.g.
	// rewrite "Foo.ts" inside "Foo.tsx", or "config.json" inside "backend/config.json". If no
	// whole-token occurrence exists, nothing is genuinely auto-fixable: fall back to advisory.
	patched, replaced := replacePathToken(body, brokenRef, resolvedPath)
	if !replaced {
		return "", "", false
	}
	return patched, resolvedPath, true
}

// replacePathToken replaces every WHOLE-TOKEN occurrence of oldRef in body with newRef and
// reports whether any replacement happened. An occurrence counts only when it is not abutted
// by a path-continuation byte (alphanumeric, '.', '/', '_', '-') on either side, so a
// reference embedded in a longer filename or path is left intact — this is what lets the
// deterministic lane trust its own output and never rewrite a path it was not asked to. The
// replacement text is not re-scanned, so newRef containing oldRef cannot cascade.
func replacePathToken(body, oldRef, newRef string) (result string, replaced bool) {
	if oldRef == "" {
		return body, false
	}
	var builder strings.Builder
	for index := 0; index < len(body); {
		if strings.HasPrefix(body[index:], oldRef) && isWholePathToken(body, index, index+len(oldRef)) {
			builder.WriteString(newRef)
			index += len(oldRef)
			replaced = true
			continue
		}
		builder.WriteByte(body[index])
		index++
	}
	return builder.String(), replaced
}

// isWholePathToken reports whether body[start:end] is a whole path token — not abutted by a
// path-continuation byte on either side, so a substring inside a longer path or filename is
// not mistaken for the token.
func isWholePathToken(body string, start, end int) bool {
	if start > 0 && isPathByte(body[start-1]) {
		return false
	}
	if end < len(body) && isPathByte(body[end]) {
		return false
	}
	return true
}

func isPathByte(char byte) bool {
	return char >= 'A' && char <= 'Z' ||
		char >= 'a' && char <= 'z' ||
		char >= '0' && char <= '9' ||
		char == '.' || char == '/' || char == '_' || char == '-'
}
