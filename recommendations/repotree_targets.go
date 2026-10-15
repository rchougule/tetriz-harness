// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"strings"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
)

// isRuleFile reports whether a path is a LOADABLE AI artifact-config file — a rule OR a
// skill/command/prompt/agent/hook the tool actually loads. Used to populate RepoTree.rule_files
// so the derivation LLM (and skillFallbackDir) see the conventions the repo really uses; callers
// split by kind with isRuleDestination / isSkillDestination. It applies the SAME extension/shape
// gate as EnumerateArtifactFiles so a non-loadable file (a wrong-extension .cursor/commands/util.sh,
// a nested rule) does not count as a convention the repo uses — the two detection paths must not
// diverge, or a bogus rule_file would mislead the fallback-destination picker.
func isRuleFile(filePath string) bool {
	conv, ok := conventionFor(filePath)
	if !ok {
		return false
	}
	dirEntry, isDir := dirEntryFor(filePath)
	if !isDir {
		return true // a basename / exact-path convention (CLAUDE.md, .claude/settings.json) — no dir shape
	}
	switch {
	case dirEntry.skillPackage:
		return true // a file under a skill dir; the enumerator groups it (root detection needs the whole tree)
	case conv.kind == destAgent || conv.kind == destHook:
		_, valid := perSlugSlug(conv, dirEntry, filePath)
		return valid
	default: // rule / command / prompt — a direct, non-hidden child carrying the convention's extension
		rest, found := afterDirPrefix(filePath, dirEntry.prefix)
		return found && !strings.Contains(rest, "/") && !strings.HasPrefix(rest, ".") && dirEntry.matchesExt(filePath)
	}
}

// isRuleDestination reports whether a path is a place a derived RULE can live (an agent
// rules-instruction file or a rules directory), NOT a skill/command/prompt file.
func isRuleDestination(filePath string) bool {
	conv, ok := conventionFor(filePath)
	return ok && conv.kind == destRule
}

// isSkillDestination reports whether a path is a place a derived SKILL can live (a
// skills/commands/prompts directory).
func isSkillDestination(filePath string) bool {
	conv, ok := conventionFor(filePath)
	return ok && conv.kind == destSkill
}

// matchesRuleDir reports whether a path lives in a per-rule directory convention — a
// rules directory where each rule is its own file (.cursor/rules/, .claude/rules/) —
// as opposed to a shared single rules file.
func matchesRuleDir(filePath string) bool {
	entry, ok := dirEntryFor(filePath)
	return ok && entry.conv.kind == destRule
}

// isSharedRuleFile reports whether a path is a single shared rules file that rules are
// APPENDED to (CLAUDE.md, AGENTS.md, .cursorrules, …) rather than a per-rule directory
// file. A new_referenced_file placement is incoherent for a shared file — it cannot be
// "created and referenced from itself" — so the resolver coerces such a placement to
// append_to_file.
func isSharedRuleFile(filePath string) bool {
	return isRuleDestination(filePath) && !matchesRuleDir(filePath)
}

// copilotRepoWideInstructions is GitHub Copilot's repository-wide custom-instructions file. It
// applies to every request and needs no frontmatter, unlike a per-file
// .github/instructions/*.instructions.md (which requires an applyTo frontmatter block). Derived
// RULES are routed here so an admin never gets a Copilot instruction file that Copilot won't
// apply for lack of the frontmatter the derivation does not author.
const copilotRepoWideInstructions = ".github/copilot-instructions.md"

// isCopilotInstructionsDirFile reports whether a path is a per-file Copilot instructions file
// under .github/instructions/ — a RULE destination that requires an applyTo frontmatter block
// (GitHub docs) the derivation does not emit. Copilot skills (.github/prompts) are a different
// kind and do not match.
func isCopilotInstructionsDirFile(filePath string) bool {
	entry, ok := dirEntryFor(filePath)
	return ok && entry.conv.tool == commonv1.IdeType_IDE_TYPE_COPILOT && entry.conv.kind == destRule
}

// isStandaloneArtifactFile reports whether a path lives in a directory-based convention — a
// per-rule dir (.cursor/rules, .claude/rules, .github/instructions, .kiro/steering) or a
// skill/command/prompt dir (.claude/skills, .agents/skills, .github/prompts, …) — where each
// artifact is its OWN file, as opposed to a shared rules file that is appended to. It is the
// complement of isSharedRuleFile over valid targets.
func isStandaloneArtifactFile(filePath string) bool {
	_, ok := dirEntryFor(filePath)
	return ok
}

// repoHasRuleFile reports whether filePath already exists among the repo's detected artifact
// files (RepoTree.rule_files), compared case-insensitively as elsewhere in this package. It lets
// target resolution tell an existing file (append to it) from a to-be-created one (create it), so
// a placement is never rewritten to "create" a file the repo already has.
func repoHasRuleFile(repoTree *promptlibraryv1.RepoTree, filePath string) bool {
	target := strings.ToLower(filePath)
	for _, ruleFile := range repoTree.GetRuleFiles() {
		if strings.ToLower(ruleFile) == target {
			return true
		}
	}
	return false
}

// skillsMergeIntoRuleFile reports whether a tool authors skills as blocks merged INTO
// its rule file rather than as standalone skill files. Only Cursor does — the derive
// prompt emits a Cursor skill as a conditional block for .cursorrules / .cursor/rules,
// prose written to merge into that file. Every other rule-only tool (Codex, Kiro,
// Antigravity) authors skills as standalone frontmatter files, so its rule file is NOT
// a skill target — splicing a frontmatter block into it would malform the rules file.
// Keyed on the tool the prompt contract actually verifies, not merely "has no skill dir".
func skillsMergeIntoRuleFile(tool commonv1.IdeType) bool {
	return tool == commonv1.IdeType_IDE_TYPE_CURSOR
}

// newArtifactPath builds the repo-relative path for a NEW artifact with the given slug in
// directory dir (which matched entry). A skillPackage dir yields "<dir>/<slug>/SKILL.md"
// (the Claude Code / skill layout); any other yields "<dir>/<slug>" + the canonical
// extension (the dir's first accepted extension). dir may carry a trailing slash.
func newArtifactPath(entry artifactDir, dir, slug string) string {
	base := strings.TrimSuffix(dir, "/")
	if entry.skillPackage {
		return base + "/" + slug + "/SKILL.md"
	}
	return base + "/" + slug + entry.canonicalExt()
}

// targetHasValidExt reports whether a target path inside a directory-based convention
// carries the layout that tool actually loads: a skillPackage dir requires a
// "<name>/SKILL.md" file, an extension-constrained dir requires that extension (Copilot
// instructions .instructions.md, prompts .prompt.md, Cursor .mdc). A file in such a dir
// with the wrong layout/extension is one the tool never surfaces, so it is not a valid
// output target. A path outside any dir convention (a shared rule file, an unconstrained
// dir) has no such constraint and is accepted.
func targetHasValidExt(filePath string) bool {
	entry, ok := dirEntryFor(filePath)
	if !ok {
		return true
	}
	if entry.skillPackage {
		// The package layout is exactly "<dir>/<skill-name>/SKILL.md": one non-empty name
		// segment then the file SKILL.md, matched CASE-SENSITIVELY (Claude Code / load
		// only that exact filename on a case-sensitive FS). A bare "<dir>/SKILL.md" (no name
		// folder), a deeper path, or a wrong-case "skill.md" is not a loadable skill.
		rest, found := afterDirPrefix(filePath, entry.prefix)
		if !found {
			return false
		}
		name, file, hasSlash := strings.Cut(rest, "/")
		return hasSlash && name != "" && file == "SKILL.md"
	}
	return entry.matchesExt(filePath)
}

// afterDirPrefix returns the portion of filePath following the anchored occurrence of
// dirPrefix (repo root or after a "/"), preserving original case, and whether the prefix
// was present. Mirrors pathHasDirPrefix's anchoring so the two never disagree.
func afterDirPrefix(filePath, dirPrefix string) (string, bool) {
	lower := strings.ToLower(filePath)
	if strings.HasPrefix(lower, dirPrefix) {
		return filePath[len(dirPrefix):], true
	}
	if idx := strings.Index(lower, "/"+dirPrefix); idx >= 0 {
		return filePath[idx+1+len(dirPrefix):], true
	}
	return "", false
}

// artifactDirRoot returns the convention directory a matched file sits directly under:
// the anchored dirPrefix occurrence with any parent path preserved and the trailing slash
// trimmed (".claude/skills/onboard/SKILL.md" → ".claude/skills"; "client/.agents/skills/x/SKILL.md"
// → "client/.agents/skills"). A NEW artifact is placed beside the existing one under this
// root; path.Dir would instead point inside the sibling package, nesting a new skill in
// another skill's package.
func artifactDirRoot(filePath, dirPrefix string) string {
	rest, ok := afterDirPrefix(filePath, dirPrefix)
	if !ok {
		return strings.TrimSuffix(dirPrefix, "/")
	}
	return strings.TrimSuffix(filePath[:len(filePath)-len(rest)], "/")
}

// hookEventAllowlist maps each AI tool to its known hook lifecycle events (lowercased
// for case-insensitive membership). It is ADVISORY, not authoritative: an
// event outside a tool's list does not hide the hook — the gate meters it on the drift
// counter and keeps it — because in-repo extractors parse hook configs as open maps and
// upstream tools add events over time, so a strict allowlist would silently suppress
// valid hooks as those vocabularies drift. Representative, not exhaustive;
// extend as tool vocabularies are confirmed. Kiro is the one exception whose list is
// treated as CLOSED (blocking): its entry holds only the file-event automations it runs,
// and hookBlockingFailure suppresses any Kiro event outside it — the Kiro IDE executes
// no lifecycle hooks.
var hookEventAllowlist = map[commonv1.IdeType]map[string]struct{}{
	commonv1.IdeType_IDE_TYPE_CLAUDE_CODE: setOfLower(
		"PreToolUse", "PostToolUse", "UserPromptSubmit", "Notification",
		"Stop", "SubagentStop", "PreCompact", "SessionStart", "SessionEnd",
	),
	commonv1.IdeType_IDE_TYPE_CURSOR: setOfLower(
		"beforeShellExecution", "afterShellExecution", "beforeSubmitPrompt",
		"beforeReadFile", "afterFileEdit", "beforeMCPExecution", "stop",
		// Cursor also natively runs these lifecycle steps (see the desktop extractor's
		// cursorCliHooks.ts + the real .cursor/hooks.json fixture) — they share a token with
		// Claude/Codex/Copilot, so they MUST be listed here or the cross-tool block would
		// suppress a valid Cursor hook that uses them.
		"sessionStart", "preToolUse", "postToolUse",
	),
	// Copilot has no documented Claude-style hook lifecycle, so this vocabulary is
	// best-effort. The allowlist is advisory, so an unmatched event only bumps the drift
	// counter and never hides the hook — the drift counter is the signal to correct it.
	commonv1.IdeType_IDE_TYPE_COPILOT: setOfLower(
		"preToolUse", "postToolUse", "userPromptSubmit", "sessionStart", "sessionEnd",
	),
	commonv1.IdeType_IDE_TYPE_CODEX: setOfLower(
		"PreToolUse", "PostToolUse", "UserPromptSubmit", "SessionStart", "SessionEnd", "Stop",
	),
	commonv1.IdeType_IDE_TYPE_KIRO: setOfLower(
		"PostFileSave", "PostFileCreate", "PostFileDelete",
	),
}

// setOfLower builds a lowercased string set for case-insensitive event membership.
func setOfLower(events ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(events))
	for _, event := range events {
		set[strings.ToLower(event)] = struct{}{}
	}
	return set
}

// isKnownHookEvent reports whether event is on the resolved tool's advisory allowlist. A
// tool with no allowlist entry (or an unmapped tool) treats every event as unknown, so
// the drift counter fires — the safe direction for a tool we have not vocabulary-mapped.
func isKnownHookEvent(tool commonv1.IdeType, event string) bool {
	allowed, ok := hookEventAllowlist[tool]
	if !ok {
		return false
	}
	_, known := allowed[strings.ToLower(strings.TrimSpace(event))]
	return known
}

// isKnownForAnySupportedTool reports whether event is on ANY mapped OPEN-VOCABULARY tool's
// advisory allowlist (case-insensitive), separating a cross-tool event (belongs to a
// different open-vocab tool) from genuine drift (unknown everywhere). Kiro is excluded — its
// closed file-event vocabulary has its own blocking path, so a Kiro file event on an open-vocab
// tool is left to the advisory path, not treated as a cross-tool mismatch. Membership is
// lowercased, so casing aliases across tools (Copilot's preToolUse vs Claude's PreToolUse)
// unify to one token; this is a best-effort signal, not proof — the allowlists are non-exhaustive
// (see hookEventAllowlist), so callers must keep the suppression it drives transient and observable.
func isKnownForAnySupportedTool(event string) bool {
	normalized := strings.ToLower(strings.TrimSpace(event))
	for tool, allowed := range hookEventAllowlist {
		if tool == commonv1.IdeType_IDE_TYPE_KIRO {
			continue
		}
		if _, ok := allowed[normalized]; ok {
			return true
		}
	}
	return false
}

// artifactSlugPlaceholder marks where a per-artifact slug is substituted in a
// toolArtifactTargets path template.
const artifactSlugPlaceholder = "<slug>"

// toolArtifactTargets is the SINGLE SOURCE for each resolved tool's agent-file and
// hook-config destinations. agentPath and hookPath carry an
// artifactSlugPlaceholder where the destination is per-artifact; a template with no
// placeholder is a single shared config file the snippet merges into. Only tools in
// common.v1.IdeType appear. This table owns the destination PATHS; the per-tool BODY
// shape (Claude/Codex nested matcher-groups vs Cursor/Copilot flat vs Kiro's array
// manifest; agent YAML-frontmatter vs TOML) is Go-synthesized from the structured
// event/matcher/command and agent fields — see synthesizeHookBody / synthesizeAgentBody
// in toolbody.go, the source of truth for shape.
var toolArtifactTargets = map[commonv1.IdeType]struct {
	agentPath string
	hookPath  string
}{
	commonv1.IdeType_IDE_TYPE_CLAUDE_CODE: {".claude/agents/" + artifactSlugPlaceholder + ".md", ".claude/settings.json"},
	commonv1.IdeType_IDE_TYPE_CODEX:       {".codex/agents/" + artifactSlugPlaceholder + ".toml", ".codex/hooks.json"},
	commonv1.IdeType_IDE_TYPE_COPILOT:     {".github/agents/" + artifactSlugPlaceholder + ".agent.md", ".github/hooks/" + artifactSlugPlaceholder + ".json"},
	commonv1.IdeType_IDE_TYPE_ANTIGRAVITY: {".agents/agents/" + artifactSlugPlaceholder + "/agent.md", ".agents/hooks.json"},
	// Kiro IDE agent format is Markdown+YAML at .kiro/agents/<slug>.md (the .json schema
	// is CLI-only); a Kiro hook is a standalone .kiro/hooks/<slug>.kiro.hook manifest.
	commonv1.IdeType_IDE_TYPE_KIRO:   {".kiro/agents/" + artifactSlugPlaceholder + ".md", ".kiro/hooks/" + artifactSlugPlaceholder + ".kiro.hook"},
	commonv1.IdeType_IDE_TYPE_CURSOR: {".cursor/agents/" + artifactSlugPlaceholder + ".md", ".cursor/hooks.json"},
}

// destAgentPath returns tool's agent-definition destination with slug substituted, or ""
// when tool has no known agent convention (UNSPECIFIED — the gate hides a tool-less agent).
func destAgentPath(tool commonv1.IdeType, slug string) string {
	target, ok := toolArtifactTargets[tool]
	if !ok {
		return ""
	}
	return strings.ReplaceAll(target.agentPath, artifactSlugPlaceholder, slug)
}

// destHookPath returns tool's hook-config destination with slug substituted, or "" when
// tool has no known hook convention (UNSPECIFIED — the gate hides a tool-less hook).
func destHookPath(tool commonv1.IdeType, slug string) string {
	target, ok := toolArtifactTargets[tool]
	if !ok {
		return ""
	}
	return strings.ReplaceAll(target.hookPath, artifactSlugPlaceholder, slug)
}

// hookTargetPlacement is determined by the tool's hookPath SHAPE, not its identity: a
// per-slug path (carrying the slug placeholder — Kiro's .kiro.hook manifest, a per-hook
// file) is a standalone file to CREATE (NEW_REFERENCED_FILE); a placeholder-less path (a
// single shared settings/hooks file — Claude/Codex/Cursor/Antigravity) is one the snippet
// is MERGED into (COPY_MERGE). Keying on the path keeps placement consistent with the
// destination — a per-slug file has nothing to merge into. The proto omits reference_in_file
// for a hook, so a created file carries no back-reference — fine, a standalone hook manifest
// is auto-discovered, not imported.
func hookTargetPlacement(tool commonv1.IdeType) promptlibraryv1.ArtifactTargetPlacement {
	target, ok := toolArtifactTargets[tool]
	if ok && strings.Contains(target.hookPath, artifactSlugPlaceholder) {
		return promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE
	}
	return promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_COPY_MERGE
}

// isToolAgentTarget reports whether filePath is the agent-definition destination for tool
// (its per-tool path shape with a non-empty slug segment). Lets target resolution honor an
// LLM-supplied target_file only when it names the resolved tool's real agent path.
func isToolAgentTarget(filePath string, tool commonv1.IdeType) bool {
	target, ok := toolArtifactTargets[tool]
	return ok && matchesTargetShape(filePath, target.agentPath)
}

// perSlugSlug validates that filePath is the tool's one-file-per-slug path for a destAgent or a
// per-file destHook, and returns the slug. These dirs are NOT recursively loaded: every tool
// places a single file per slug — flat <dir>/<slug><ext> (Claude/Codex/Copilot/Kiro/Cursor
// agents, Copilot/Kiro hooks) or Antigravity's <dir>/<slug>/agent.md — so a nested or extra file
// (a README, a helper, a <slug>/<other>.md) is not an artifact the tool loads and must not be
// enumerated. Mirrors the toolArtifactTargets template (agentPath for an agent, hookPath for a
// hook), accepting any of the convention's extensions (a agent is .toml or .md).
func perSlugSlug(conv artifactConvention, dirEntry artifactDir, filePath string) (string, bool) {
	target, ok := toolArtifactTargets[conv.tool]
	if !ok {
		return "", false
	}
	template := target.agentPath
	if conv.kind == destHook {
		template = target.hookPath
	}
	idx := strings.Index(template, artifactSlugPlaceholder)
	if idx < 0 {
		return "", false // a shared (non-per-slug) target has no slug to extract
	}
	rest, found := afterDirPrefix(filePath, dirEntry.prefix)
	if !found {
		return "", false
	}
	// slugSuffix is the template text after the slug: "/agent.md" for the nested Antigravity
	// layout, or the bare extension (".md", ".toml", ".kiro.hook", …) for the flat layouts.
	slugSuffix := template[idx+len(artifactSlugPlaceholder):]
	if strings.HasPrefix(slugSuffix, "/") {
		slug, tail, hasTail := strings.Cut(rest, "/")
		if hasTail && slug != "" && !strings.HasPrefix(slug, ".") && "/"+tail == slugSuffix {
			return slug, true // <slug>/<fixed file>, e.g. "reviewer/agent.md"
		}
		return "", false
	}
	if strings.Contains(rest, "/") || strings.HasPrefix(rest, ".") || !dirEntry.matchesExt(filePath) {
		return "", false // nested, hidden, or wrong-extension file — not a flat <slug><ext> artifact
	}
	return dirEntry.trimExt(rest), true
}

// isToolHookTarget reports whether filePath is the hook-config destination for tool.
func isToolHookTarget(filePath string, tool commonv1.IdeType) bool {
	target, ok := toolArtifactTargets[tool]
	return ok && matchesTargetShape(filePath, target.hookPath)
}

// matchesTargetShape reports whether filePath conforms to a toolArtifactTargets template.
// A template with no artifactSlugPlaceholder (a shared config file) requires exact
// equality; one with a placeholder requires the surrounding prefix/suffix plus a single
// non-empty slug segment (no "/"), so a nested or empty middle is rejected.
func matchesTargetShape(filePath, shape string) bool {
	prefix, suffix, hasSlug := strings.Cut(shape, artifactSlugPlaceholder)
	if !hasSlug {
		return filePath == shape
	}
	if len(filePath) < len(prefix)+len(suffix) || !strings.HasPrefix(filePath, prefix) || !strings.HasSuffix(filePath, suffix) {
		return false
	}
	middle := filePath[len(prefix) : len(filePath)-len(suffix)]
	return middle != "" && !strings.Contains(middle, "/")
}

// resolveDominantTool picks the single AI tool a derived hook/agent targets, resolved once
// up front at BuildContext and threaded to the gate + target resolver. Ranking:
// checked-in rule file > checked-in hook/agent config > ide_breakdown. A tool whose config a
// repo commits standardizes on that tool and OVERRIDES the session mix. When a repo commits
// configs for MORE THAN ONE tool at a tier (e.g. AGENTS.md + CLAUDE.md), the candidate set is
// disambiguated by the session IDE mix — the dominant logged tool that is among the candidates
// — rather than by path sort order, which would otherwise arbitrarily target e.g. .codex on a
// Claude-Code-dominant repo. A single committed tool wins outright. ide_breakdown (rows sorted
// by session count desc) decides only when no config is committed. Returns UNSPECIFIED when
// none resolve (the gate then hides tool-targeted hooks/agents). A securityOnly run passes a
// nil ide_breakdown; a multi-tool repo then falls back to the sorted-first candidate.
func resolveDominantTool(ideBreakdown []*promptlibraryv1.IDESummary, repoTree *promptlibraryv1.RepoTree) commonv1.IdeType {
	if tools := ruleTools(repoTree); len(tools) > 0 {
		return disambiguate(tools, ideBreakdown)
	}
	if tools := configTools(repoTree); len(tools) > 0 {
		return disambiguate(tools, ideBreakdown)
	}
	for _, ide := range ideBreakdown {
		if tool := ideTypeFromRaw(ide.GetIde()); tool != commonv1.IdeType_IDE_TYPE_UNSPECIFIED {
			return tool
		}
	}
	return commonv1.IdeType_IDE_TYPE_UNSPECIFIED
}

// ideTypeFromRaw resolves a stored ide_breakdown label to its canonical IdeType.
// AggregateByIDE returns usage_metadata.ideType verbatim (a raw deref kept for its
// expression index), so newer rows carry the enum form ("IDE_TYPE_CURSOR") while legacy
// rows carry a lowercase alias ("cursor") — the DAO tests accept both. Consulting
// IdeType_value alone maps a legacy alias to 0 (UNSPECIFIED), so a dominant legacy row
// would be silently skipped and the tool mis-resolved. So look up the canonical form first,
// then fall back to the alias set. A tool with no enum (vscode, windsurf, codeium) resolves
// to UNSPECIFIED — it is not a hook/agent target, so that is correct.
func ideTypeFromRaw(raw string) commonv1.IdeType {
	if tool := commonv1.IdeType(commonv1.IdeType_value[raw]); tool != commonv1.IdeType_IDE_TYPE_UNSPECIFIED {
		return tool
	}
	switch strings.ToLower(raw) {
	case "claude code", "claude-code", "ide_type_claude_code":
		return commonv1.IdeType_IDE_TYPE_CLAUDE_CODE
	case "cursor", "ide_type_cursor":
		return commonv1.IdeType_IDE_TYPE_CURSOR
	case "copilot", "github copilot", "github-copilot", "ide_type_copilot":
		return commonv1.IdeType_IDE_TYPE_COPILOT
	case "antigravity", "ide_type_antigravity":
		return commonv1.IdeType_IDE_TYPE_ANTIGRAVITY
	case "codex", "openai codex", "ide_type_codex":
		return commonv1.IdeType_IDE_TYPE_CODEX
	case "kiro", "ide_type_kiro":
		return commonv1.IdeType_IDE_TYPE_KIRO
	}
	return commonv1.IdeType_IDE_TYPE_UNSPECIFIED
}

// dominantRuleTool returns the tool of the repo's rule files (the tool whose config the
// repo actually uses) — used to pick a tool-appropriate default when the repo has no
// existing skill directory of its own. rule_files is sorted, so the choice is
// deterministic; returns UNSPECIFIED when the repo carries no rule file.
func dominantRuleTool(repoTree *promptlibraryv1.RepoTree) commonv1.IdeType {
	for _, ruleFile := range repoTree.GetRuleFiles() {
		if conv, ok := conventionFor(ruleFile); ok && conv.kind == destRule {
			return conv.tool
		}
	}
	return commonv1.IdeType_IDE_TYPE_UNSPECIFIED
}

// ruleTools / configTools return the DISTINCT tools whose checked-in rule_files are rule
// (resp. hook/agent) configs, in rule_files (sorted) order — the candidate set
// resolveDominantTool disambiguates by the session IDE mix when a repo commits configs for
// more than one tool. dominantRuleTool stays a separate first-match helper because
// skillFallbackDir depends on its rule-only, session-independent semantics for skill placement.
func ruleTools(repoTree *promptlibraryv1.RepoTree) []commonv1.IdeType {
	return detectedTools(repoTree, func(conv artifactConvention) bool { return conv.kind == destRule })
}

func configTools(repoTree *promptlibraryv1.RepoTree) []commonv1.IdeType {
	return detectedTools(repoTree, func(conv artifactConvention) bool { return conv.kind == destHook || conv.kind == destAgent })
}

func detectedTools(repoTree *promptlibraryv1.RepoTree, match func(artifactConvention) bool) []commonv1.IdeType {
	var tools []commonv1.IdeType
	seen := map[commonv1.IdeType]bool{}
	for _, ruleFile := range repoTree.GetRuleFiles() {
		if conv, ok := conventionFor(ruleFile); ok && match(conv) && !seen[conv.tool] {
			seen[conv.tool] = true
			tools = append(tools, conv.tool)
		}
	}
	return tools
}

// disambiguate picks one tool from a set of checked-in candidate tools. A single committed
// tool wins outright (a checked-in config overrides the session mix). When more than one tool
// is committed, the session IDE mix breaks the tie — the first ide_breakdown tool that is
// among the candidates — falling back to the sorted-first candidate when the dominant IDE is
// not one of them, or there is no session signal (a securityOnly run). Empty set → UNSPECIFIED.
func disambiguate(tools []commonv1.IdeType, ideBreakdown []*promptlibraryv1.IDESummary) commonv1.IdeType {
	switch len(tools) {
	case 0:
		return commonv1.IdeType_IDE_TYPE_UNSPECIFIED
	case 1:
		return tools[0]
	}
	candidate := make(map[commonv1.IdeType]bool, len(tools))
	for _, tool := range tools {
		candidate[tool] = true
	}
	for _, ide := range ideBreakdown {
		if tool := ideTypeFromRaw(ide.GetIde()); candidate[tool] {
			return tool
		}
	}
	return tools[0]
}

// skillFallbackDir returns the directory + its convention where a NEW skill should land
// when the LLM named no target, and whether that directory is one the repo ALREADY uses:
// an existing skill dir the repo uses (foundExisting=true, so the derived skill matches the
// repo's tool), else the default skill dir for the repo's dominant rule tool (a repo →
// .agents/skills, a Copilot repo → .github/prompts), else the Claude Code default. The
// foundExisting flag lets the caller prefer an existing skill dir over a rule-file fallback
// for a skill-in-rule-file tool (Cursor). Never returns an empty dir — there is always a default.
func skillFallbackDir(repoTree *promptlibraryv1.RepoTree) (entry artifactDir, dir string, foundExisting bool) {
	for _, ruleFile := range repoTree.GetRuleFiles() {
		if existing, ok := dirEntryFor(ruleFile); ok && existing.conv.kind == destSkill {
			// The skill-dir ROOT (.claude/skills), not the existing package dir
			// (.claude/skills/onboard) — else newArtifactPath would nest the new skill
			// inside the sibling package (.claude/skills/onboard/<slug>/SKILL.md), a layout
			// no tool loads.
			return existing, artifactDirRoot(ruleFile, existing.prefix), true
		}
	}
	tool := dominantRuleTool(repoTree)
	for _, candidate := range artifactDirPrefixes {
		if candidate.conv.kind == destSkill && candidate.conv.tool == tool {
			return candidate, strings.TrimSuffix(candidate.prefix, "/"), false
		}
	}
	for _, candidate := range artifactDirPrefixes {
		if candidate.prefix == defaultSkillDir+"/" {
			return candidate, defaultSkillDir, false
		}
	}
	return artifactDir{}, defaultSkillDir, false
}

// pathHasDirPrefix reports whether dirPrefix (a "foo/bar/"-form directory) appears in
// the lowercased path as a real path-segment boundary — at the repo root or after a
// "/". Anchored, not a bare substring: without this, ".cursor/rules/" would match
// inside "vendor/my.cursor/rules/x", misclassifying an unrelated directory as a rule
// file. Callers pass an already-lowercased path.
func pathHasDirPrefix(lowerPath, dirPrefix string) bool {
	return strings.HasPrefix(lowerPath, dirPrefix) || strings.Contains(lowerPath, "/"+dirPrefix)
}

// RuleFileTool maps a detected rule-file path to the AI coding tool it belongs to,
// best-effort. It powers only the artifact's target_tool label/icon — target_file is
// the authoritative destination. AGENTS.md is cross-tool (several tools read it); it is
// labelled CODEX as the tool that established the convention. Returns IDE_TYPE_UNSPECIFIED
// for a path that matches no known convention.
func RuleFileTool(filePath string) commonv1.IdeType {
	if conv, ok := conventionFor(filePath); ok {
		return conv.tool
	}
	return commonv1.IdeType_IDE_TYPE_UNSPECIFIED
}
