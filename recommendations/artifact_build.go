// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"context"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.uber.org/zap"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
	promptlibraryv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/promptlibrary/v1"
	"github.com/tetriz-ai/tetriz-harness/recommendations/internal/logger"
	"github.com/tetriz-ai/tetriz-harness/recommendations/model"
)

// toModels converts the graded artifacts to prompt_library rows, writing each
// artifact's full ArtifactContent + ArtifactEvidence to the content store first
// and keeping the inline EvidenceProjection on the row. The artifact id is minted
// here so the content store content/evidence keys are stable before the row is inserted; a
// retry mints fresh ids (orphaning the prior attempt's the content store blobs — retention
// prunes them), while PersistArtifacts keeps the rows deterministic per run.
func (e *Engine) toModels(ctx context.Context, orgID, execID, scopeValue string, graded *GradedResult, aggCtx *promptlibraryv1.AggregatedContext, prCorrelation *promptlibraryv1.PRCorrelation) ([]*model.PromptLibraryItem, error) {
	items := make([]*model.PromptLibraryItem, 0, len(graded.Artifacts))
	for _, gradedArtifact := range graded.Artifacts {
		// A security_scope hide is a SCOPE filter (a securityOnly run suppressing
		// general output), not a quality-eval failure, so it is not persisted: the
		// recent-hidden dedup (ListRecentHiddenFailedSlugs) keys on
		// HIDDEN_FAILED_EVAL with no validator column, so persisting these would let
		// a general rule filtered during one underpowered securityOnly run suppress
		// its legitimate re-derivation in a later full run.
		// Genuine quality failures on the same run (body/dup/scope/paths) still
		// persist and feed dedup.
		//
		// A template_session_signal hide is the same shape: a TRANSIENT gate (the
		// repo's session cohort is below the floor THIS run), not a quality-eval
		// failure. Persisting it would poison the dedup identically — once the cohort
		// recovers and the model re-emits the same template slug, its own prior hidden
		// row would suppress it as not_dismissed indefinitely. So it is not persisted
		// either; a below-floor template simply does not surface this run and re-derives
		// cleanly when the signal recovers (the failure is still counted in the
		// quality-gate metric at grade time).
		//
		// Every other transient scope/context drop — the session-signal hides, the feature
		// type-drop, the per-run caps, and the tool-less and tool-scoped hook/agent hides — is
		// the same shape, so the one predicate decides them all (see isTransientDrop for why
		// each is dropped rather than persisted). HIDDEN_FAILED_EVAL from a genuine quality
		// validator (body/slug/dup/paths, empty hook_event/hook_body/agent_name) still persists
		// and feeds dedup — those are malformed regardless of context.
		if isTransientDrop(gradedArtifact.FailedValidator) {
			continue
		}
		artifactID := uuid.NewString()

		content := buildArtifactContent(ctx, gradedArtifact, firstRepoTree(aggCtx), aggCtx.GetResolvedTargetTool())
		contentKey, err := e.store.PutArtifactContent(ctx, orgID, artifactID, content)
		if err != nil {
			return nil, fmt.Errorf("persisting artifact content for %s: %w", artifactID, err)
		}
		evidence := buildArtifactEvidence(gradedArtifact, aggCtx, prCorrelation)
		evidenceKey, err := e.store.PutArtifactEvidence(ctx, orgID, artifactID, evidence)
		if err != nil {
			return nil, fmt.Errorf("persisting artifact evidence for %s: %w", artifactID, err)
		}

		items = append(items, &model.PromptLibraryItem{
			ID:             artifactID,
			OrganizationID: orgID,
			ArtifactType:   gradedArtifact.ArtifactType,
			Scope:          promptlibraryv1.ArtifactScope_ARTIFACT_SCOPE_PROJECT,
			ScopeValue:     scopeValue,
			TaskType:       taskTypeColumn(gradedArtifact),
			Title:          gradedArtifact.Artifact.Title,
			// normalizeSlug (validators.go) is the single source of truth — the gate
			// applied the same call when grading, so the persisted slug matches the
			// form suppression keys on. Keep these in lockstep.
			Slug:             normalizeSlug(gradedArtifact.Artifact.Slug, gradedArtifact.ArtifactType),
			ArtifactContent:  &promptlibraryv1.ContentRef{Key: contentKey},
			Evidence:         buildEvidenceProjection(gradedArtifact, evidence, evidenceKey),
			Priority:         priorityToInt(gradedArtifact.Artifact.Priority),
			ConfidenceScore:  float32(gradedArtifact.Artifact.Confidence),
			ClientStatus:     promptlibraryv1.ArtifactClientStatus_ARTIFACT_CLIENT_STATUS_UNSPECIFIED,
			SurfaceState:     gradedArtifact.SurfaceState,
			AdminInteraction: &promptlibraryv1.AdminInteraction{},
			TaskExecutionID:  execID,
			// IsLatest is derived by PersistArtifacts — never set here.
		})
	}
	return items, nil
}

// priorityToInt maps the LLM's semantic priority to the INT column. An
// unrecognized value defaults to medium (least urgent) rather than dropping the
// artifact — the value is advisory ordering, not a gate.
func priorityToInt(priority string) int32 {
	switch priority {
	case "critical":
		return priorityCritical
	case "high":
		return priorityHigh
	case "low":
		return priorityLow
	default:
		// "medium" and any unrecognized value both map to the medium rank (the LLM's
		// default tier); "low" is the one rank below it, reserved for the weakest evidence.
		return priorityMedium
	}
}

// taskTypeColumn returns the template task-type label (templates only); rules and
// skills carry none (the unified derive schema nulls task_type off-template).
func taskTypeColumn(gradedArtifact GradedArtifact) string {
	if gradedArtifact.ArtifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_TEMPLATE && gradedArtifact.Artifact.TaskType != nil {
		return *gradedArtifact.Artifact.TaskType
	}
	return ""
}

// buildArtifactContent assembles the full ArtifactContent stored in the content store. The body
// lives only in original/current; the type-specific oneof matches the artifact
// kind, carrying the one extra the unified derive schema supplies (a rule's
// addressed taxonomy issues). cited_paths are the LLM's structured grounding.
// resolvedTool is the run's resolved dominant tool; RULE/SKILL derive their tool
// from the resolved target FILE and ignore it, but HOOK/AGENT invert that — their
// destination is derived FROM this tool (see resolveToolScopedTarget).
func buildArtifactContent(ctx context.Context, gradedArtifact GradedArtifact, repoTree *promptlibraryv1.RepoTree, resolvedTool commonv1.IdeType) *promptlibraryv1.ArtifactContent {
	artifact := gradedArtifact.Artifact
	content := &promptlibraryv1.ArtifactContent{
		Original:   artifact.Body,
		Current:    artifact.Body,
		CitedPaths: artifact.CitedPaths,
	}
	switch gradedArtifact.ArtifactType {
	case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE:
		file, placement, referenceIn, tool := resolveArtifactTarget(artifact, gradedArtifact.ArtifactType, repoTree, resolvedTool)
		content.TypeSpecific = &promptlibraryv1.ArtifactContent_Rule{Rule: &promptlibraryv1.RuleContent{
			AddressesIssues: artifact.AddressesIssues,
			TargetFile:      file,
			TargetPlacement: placement,
			ReferenceInFile: referenceIn,
			TargetTool:      tool,
		}}
	case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_TEMPLATE:
		// Templates are tool-agnostic prompt scaffolds — no rules-file target. The LLM's
		// trigger_keywords are the template's tier-1 prompt-routing signals the install path
		// reads; read only in this branch, so keywords can never land on a non-template.
		content.TypeSpecific = &promptlibraryv1.ArtifactContent_Template{Template: &promptlibraryv1.TemplateContent{
			TriggerKeywords: artifact.TriggerKeywords,
		}}
	case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL:
		file, placement, referenceIn, tool := resolveArtifactTarget(artifact, gradedArtifact.ArtifactType, repoTree, resolvedTool)
		content.TypeSpecific = &promptlibraryv1.ArtifactContent_Skill{Skill: &promptlibraryv1.SkillContent{
			TargetFile:      file,
			TargetPlacement: placement,
			ReferenceInFile: referenceIn,
			TargetTool:      tool,
		}}
	case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK:
		// The event/matcher/command are the LLM's structured semantics (authored in the resolved
		// tool's vocabulary); the copy-paste snippet (current) is SYNTHESIZED by Go so its structure
		// always matches the destination tool — Kiro lands a standalone .kiro.hook
		// manifest, every other tool merges into its settings/hooks file (see resolveToolScopedTarget).
		// The gate hides a hook with no resolved tool, so synthesis succeeds here; on the impossible
		// error the LLM body (original) is a defensive fallback. reference_in_file is omitted (the
		// proto has no such field on a hook).
		file, placement, _, tool := resolveArtifactTarget(artifact, gradedArtifact.ArtifactType, repoTree, resolvedTool)
		// Canonicalize the structured event/matcher before BOTH synthesis and persistence. The gate
		// validates their trimmed values (hookBlockingFailure), so a stray-whitespace event like
		// " PostToolUse " passes the allowlist yet, untrimmed, would become a JSON event key the tool
		// never fires on. Trim once here so the synthesized body and the stored HookContent agree with
		// what the gate accepted. Command stays verbatim — it is the literal payload, not a matched key.
		event := strings.TrimSpace(artifact.Event)
		matcher := strings.TrimSpace(artifact.Matcher)
		// Copilot's hook format carries no matcher, so its synthesized body omits it. The stored matcher
		// must match — else EditArtifact's ValidateHookBody would require the (absent) matcher in the body
		// and reject even the unchanged synthesized body, locking the admin out of editing the command.
		if tool == commonv1.IdeType_IDE_TYPE_COPILOT {
			matcher = ""
		}
		body, err := synthesizeHookBody(tool, artifact.Slug, artifact.Title, event, matcher, artifact.Command)
		if err != nil {
			// Unreachable for a graded-VISIBLE hook — the gate hides a tool-less one and a drift-guard
			// test binds every resolvable tool to a serializer — so this is a should-never-happen
			// invariant breach; log it and keep the LLM body (original) as a defensive fallback.
			logger.Error(ctx, "prompt-library: hook body synthesis failed for a visible hook; keeping the LLM body",
				zap.String("slug", artifact.Slug), zap.String("tool", tool.String()), zap.Error(err))
		} else {
			// The synthesized body IS the deployable artifact, so it is BOTH the immutable baseline
			// (original — the edit-revert target + edit-size denominator) and the current text.
			content.Original = body
			content.Current = body
		}
		content.TypeSpecific = &promptlibraryv1.ArtifactContent_Hook{Hook: &promptlibraryv1.HookContent{
			Event:           event,
			Matcher:         matcher,
			Command:         artifact.Command,
			TargetFile:      file,
			TargetPlacement: placement,
			TargetTool:      tool,
		}}
	case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT:
		// The agent frontmatter (name/description/tools/model) are the LLM's structured extras and
		// its instruction prose is the body (original); Go SYNTHESIZES the tool-correct envelope
		// around that prose (current) — YAML-frontmatter Markdown for every tool except, whose
		// agent is a TOML file with a developer_instructions string. The agent file is
		// a new standalone file in the resolved tool's agent dir. reference_in_file is omitted (proto
		// has no such field on an agent — an agent file is auto-discovered, not imported).
		file, placement, _, tool := resolveArtifactTarget(artifact, gradedArtifact.ArtifactType, repoTree, resolvedTool)
		// The LLM emits a generic archetype — a tool scope and a SEMANTIC model tier ("a higher tier",
		// "mid"), not tool-specific names. Each tool's serializer maps those onto its own vocabulary
		// (Claude/Cursor model alias; model_reasoning_effort + a sandbox_mode derived from the tool
		// scope so a read-only reviewer stays read-only), so the deployable file honors the requested
		// scope instead of an unloadable value or a silently-broadened capability. The agent is keyed by
		// its validated kebab slug (matches the on-disk filename), not the display name.
		// AgentContent.Model holds the tool-resolved model (empty for, whose tier lives in the body
		// as reasoning_effort) so the panel never claims a model the file does not carry. AgentName stores
		// the same normalized identifier the serializer writes as the file's `name`, so the panel shows an
		// invocable specialist name (not a display-style "Code Reviewer" that cannot invoke the copied agent).
		agentModel := resolveAgentModel(tool, artifact.Model)
		agentID := normalizeAgentIdentifier(artifact.Slug)
		body, err := synthesizeAgentBody(tool, agentID, artifact.Description, artifact.Tools, artifact.Model, artifact.Body)
		if err != nil {
			// Unreachable for a graded-VISIBLE agent (gate hides a tool-less one; drift-guard covers
			// every tool) — log the should-never-happen breach and keep the LLM prose as a fallback.
			logger.Error(ctx, "prompt-library: agent body synthesis failed for a visible agent; keeping the LLM prose",
				zap.String("slug", artifact.Slug), zap.String("tool", tool.String()), zap.Error(err))
		} else {
			// The synthesized envelope IS the deployable agent file, so it is BOTH the immutable
			// baseline (original) and the current text; the raw prose lives inside it.
			content.Original = body
			content.Current = body
		}
		content.TypeSpecific = &promptlibraryv1.ArtifactContent_Agent{Agent: &promptlibraryv1.AgentContent{
			AgentName:       agentID,
			Description:     artifact.Description,
			Tools:           artifact.Tools,
			Model:           agentModel,
			TargetFile:      file,
			TargetPlacement: placement,
			TargetTool:      tool,
		}}
	default:
		// UNSPECIFIED never reaches here (flattenArtifacts always sets a real type);
		// leave the oneof unset rather than guess a bucket.
	}
	return content
}

// Fallback destinations used when a repo carries no detected rule file and the LLM
// named none. They encode Claude Code's conventions — the primary tool; a
// repo that already uses another tool overrides them via its detected rule files.
const (
	defaultRuleFile = "CLAUDE.md"
	defaultSkillDir = ".claude/skills"
	// maxTargetPathLen bounds a stored/rendered target path (runes). A real rule-file
	// path is short; the cap is an abuse backstop on the LLM-authored value.
	maxTargetPathLen = 256
)

// resolveArtifactTarget decides where a rule/skill lands. The LLM's target_file wins
// (it authored the body for that tool) once sanitized; an unsafe or empty value falls
// back to a type-appropriate destination — the nearest existing rule file for a rule,
// a skills-dir file for a skill. target_tool is always derived from the resolved file
// (RuleFileTool) so the label can't disagree with the body. reference_in_file is kept
// only for NEW_REFERENCED_FILE placement, and is itself sanitized.
func resolveArtifactTarget(artifact DerivedArtifact, artifactType promptlibraryv1.ArtifactType, repoTree *promptlibraryv1.RepoTree, resolvedTool commonv1.IdeType) (targetFile string, placement promptlibraryv1.ArtifactTargetPlacement, referenceIn string, tool commonv1.IdeType) {
	// HOOK and AGENT invert the rule/skill flow: the destination is derived FROM the run's
	// resolved dominant tool, not from an LLM target_file (a hook/agent path is fully
	// determined by tool + slug). resolveToolScopedTarget owns that; the rest of this
	// function is the rule/skill path.
	if artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK || artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT {
		return resolveToolScopedTarget(artifact, artifactType, resolvedTool)
	}
	targetFile = sanitizeTargetPath(artifact.TargetFile)
	// Path-safety is not enough — the target must also be a destination valid for THIS
	// artifact type (a rule into a rule file, a skill into a skill file). Otherwise an
	// LLM that echoes a cited source path (e.g. backend/main.go) would be rendered as
	// "copy this rule into backend/main.go". A wrong/invalid destination is dropped so
	// resolution falls back to a real one.
	if targetFile != "" && !validTargetForType(targetFile, artifactType, resolvedTool) {
		targetFile = ""
	}
	placement = parseTargetPlacement(artifact.TargetPlacement)
	referenceIn = sanitizeTargetPath(artifact.ReferenceInFile)
	// The importer must be a rules file OF THE SAME TOOL as target_file — it is the parent
	// that links the new file, and a cross-tool importer is incoherent: "create
	// .claude/rules/x.md and reference it from .cursorrules" would edit the Cursor config to
	// import a Claude rule Cursor never loads. A non-rule path (README.md, backend/main.go)
	// or a rule file for a different tool is dropped so the UI never renders that instruction.
	// A rejected target_file above leaves this comparing against "" (tool UNSPECIFIED), which
	// clears any stray reference; the fallback then recomputes both.
	if referenceIn != "" && (!isRuleDestination(referenceIn) || RuleFileTool(referenceIn) != RuleFileTool(targetFile)) {
		referenceIn = ""
	}
	if targetFile == "" {
		// The LLM named no usable destination (blank, or rejected as unsafe) — fall
		// back to a type-appropriate location, the placement that fits it, and (for a
		// new per-rule-dir file) any reference the fallback resolves.
		targetFile, placement, referenceIn = fallbackTarget(artifactType, artifact.Slug, artifact.CitedPaths, repoTree)
	}
	// A per-file Copilot instructions file (.github/instructions/*.instructions.md) requires an
	// applyTo frontmatter block the derivation does not author, so a rule body dropped there is
	// never applied. Route a derived RULE to the repo-wide .github/copilot-instructions.md, which
	// needs no frontmatter and always applies — whether the path came from the LLM or the
	// fallback. The reference is dropped: the repo-wide file imports nothing. (Copilot skills →
	// .github/prompts are a different kind and are left as-is.)
	if artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_RULE && isCopilotInstructionsDirFile(targetFile) {
		targetFile = copilotRepoWideInstructions
		referenceIn = ""
	}
	// The LLM's placement is coerced to what the destination structurally allows. A
	// new_referenced_file is impossible for a shared rules file (CLAUDE.md, .cursorrules — it
	// cannot be "created and referenced from itself") or for a file the repo ALREADY has (you
	// cannot create an existing file), so both become append. Conversely a plain append to a
	// NEW file in a directory-based convention (.cursor/rules, .claude/rules, .claude/skills, …)
	// — including the schema's append default when the LLM omits placement — is a standalone
	// file to CREATE, so it becomes new_referenced_file; else the UI says "Copy this into <file>"
	// for a file that does not exist yet.
	switch {
	case placement == promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE && (isSharedRuleFile(targetFile) || repoHasRuleFile(repoTree, targetFile)):
		placement = promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_APPEND_TO_FILE
	case placement == promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_APPEND_TO_FILE && isStandaloneArtifactFile(targetFile) && !repoHasRuleFile(repoTree, targetFile):
		placement = promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE
	}
	if placement != promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE {
		referenceIn = "" // a reference target is meaningful only for a newly-created file
	}
	tool = RuleFileTool(targetFile)
	return targetFile, placement, referenceIn, tool
}

// resolveToolScopedTarget resolves a HOOK or AGENT destination from the run's resolved
// dominant tool, inverting the rule/skill flow (which derives the tool from an
// LLM target_file). An LLM-supplied target_file is honored only when it names the resolved
// tool's own hook/agent destination; otherwise the canonical per-tool path (tool + slug)
// is used. Placement is per-type: an AGENT is always a new standalone file
// (NEW_REFERENCED_FILE); a HOOK is COPY_MERGE into the tool's settings/hooks file, except a
// Kiro hook — a standalone .kiro.hook manifest, created (NEW_REFERENCED_FILE). Both omit
// reference_in_file (the proto has none for these types). When no tool resolved
// (UNSPECIFIED), the canonical path is "" and the serving layer hides the artifact — the
// gate already fails a tool-less hook/agent, so this is only a defensive floor.
func resolveToolScopedTarget(artifact DerivedArtifact, artifactType promptlibraryv1.ArtifactType, resolvedTool commonv1.IdeType) (targetFile string, placement promptlibraryv1.ArtifactTargetPlacement, referenceIn string, tool commonv1.IdeType) {
	slug := artifactFileBasename(normalizeSlug(artifact.Slug, artifactType))
	targetFile = sanitizeTargetPath(artifact.TargetFile)
	if targetFile == "" || !validTargetForType(targetFile, artifactType, resolvedTool) {
		if artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT {
			targetFile = destAgentPath(resolvedTool, slug)
		} else {
			targetFile = destHookPath(resolvedTool, slug)
		}
	}
	if artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT {
		return targetFile, promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE, "", resolvedTool
	}
	return targetFile, hookTargetPlacement(resolvedTool), "", resolvedTool
}

// sanitizeTargetPath accepts an LLM-authored destination path only if it is a safe
// repo-relative path, else returns "" (the caller then falls back). It rejects an
// absolute path, a "~" home reference, any ".." traversal (post-Clean), a backslash
// separator, a control character, or a path longer than maxTargetPathLen — none of
// which is a legitimate in-repo rule-file destination, and any of which, rendered as
// a "copy this into X" instruction to a human or an AI agent, could point outside the
// repo. Grounding parity with cited_paths (validators.go), which are tree-checked.
func sanitizeTargetPath(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if utf8.RuneCountInString(trimmed) > maxTargetPathLen {
		return ""
	}
	if strings.ContainsAny(trimmed, "\\") || strings.HasPrefix(trimmed, "~") {
		return ""
	}
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	if path.IsAbs(trimmed) {
		return ""
	}
	cleaned := path.Clean(trimmed)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return ""
	}
	return cleaned
}

// fallbackTarget picks a destination when the LLM supplied none, naming a NEW file the
// tool actually loads. A skill lands in the repo's (or its dominant tool's) skill dir,
// created in that tool's layout (a package "<dir>/<slug>/SKILL.md" for Claude/Codex, a
// "<slug>.prompt.md" for Copilot) — EXCEPT for a tool that authors skills as blocks in its
// rule file (Cursor), where the skill follows the rule fallback instead. A rule appends to
// the nearest existing shared rules file, EXCEPT when the only rule destination is a
// per-rule directory (.cursor/rules, .claude/rules) — there each rule is its own file, so a
// new slug-named file is created rather than corrupting an existing one. Slugs are
// normalized to the gate's stored form.
func fallbackTarget(artifactType promptlibraryv1.ArtifactType, slug string, citedPaths []string, repoTree *promptlibraryv1.RepoTree) (targetFile string, placement promptlibraryv1.ArtifactTargetPlacement, referenceIn string) {
	// An existing skill dir always wins (a mixed repo that has one keeps using it); only a repo
	// with no skill dir whose tool merges skills into its rule file (Cursor) falls through to the
	// rule fallback below, per the func doc.
	if artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL {
		entry, dir, foundExisting := skillFallbackDir(repoTree)
		if foundExisting || !skillsMergeIntoRuleFile(dominantRuleTool(repoTree)) {
			return newArtifactPath(entry, dir, artifactFileBasename(normalizeSlug(slug, promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL))),
				promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE, ""
		}
	}
	nearest := nearestRuleFile(citedPaths, repoTree)
	if entry, ok := dirEntryFor(nearest); ok && entry.conv.kind == destRule {
		// A per-rule directory (.cursor/rules, .claude/rules, .github/instructions,
		// .kiro/steering) holds one standalone file per rule, each auto-discovered and
		// loaded by its tool — so a new slug-named file here needs no importer and
		// referenceIn stays empty.
		newFile := newArtifactPath(entry, path.Dir(nearest), artifactFileBasename(normalizeSlug(slug, artifactType)))
		return newFile, promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE, ""
	}
	return nearest, promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_APPEND_TO_FILE, ""
}

// artifactFileBasename derives a filesystem-safe basename from a normalizeSlug output
// (e.g. "/spec-driven-dev" for a skill, "go-error-wrapping" for a rule). The fallback path
// interpolates this into "<dir>/<base>...", so base must be a single safe filename. A
// well-formed slug is kebab, which by construction has no path separator ("/" or "\"), no
// ".." traversal, and no control char — so any input failing the kebab shape yields
// "skill", and the fallback path can never escape its directory. This is the slug-side
// twin of sanitizeTargetPath (which guards the LLM-supplied target_file).
func artifactFileBasename(slug string) string {
	base := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(slug), "/"))
	if !kebabSlugPattern.MatchString(base) {
		return "skill"
	}
	return base
}

// validTargetForType reports whether targetFile is a destination the given artifact
// type may legitimately land in: a rule in a rule destination, a skill in a skill
// destination (with the layout/extension the tool actually loads — targetHasValidExt),
// a hook in the resolved tool's hook-config path, an agent in its agent-file path.
// resolvedTool is used only for HOOK/AGENT (whose destination is tool-keyed); RULE/SKILL
// derive their tool from the path and ignore it. Templates carry no target.
func validTargetForType(targetFile string, artifactType promptlibraryv1.ArtifactType, resolvedTool commonv1.IdeType) bool {
	switch artifactType {
	case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_HOOK:
		return isToolHookTarget(targetFile, resolvedTool)
	case promptlibraryv1.ArtifactType_ARTIFACT_TYPE_AGENT:
		return isToolAgentTarget(targetFile, resolvedTool)
	default:
		// RULE / SKILL / TEMPLATE / UNSPECIFIED derive their tool from the path, not the
		// resolved tool — fall through to the rule/skill destination checks below.
	}
	if !targetHasValidExt(targetFile) {
		return false
	}
	if artifactType == promptlibraryv1.ArtifactType_ARTIFACT_TYPE_SKILL {
		if isSkillDestination(targetFile) {
			return true
		}
		// Cursor keeps skills in its rule file (a conditional block merged into
		// .cursorrules / a .cursor/rules/*.mdc — the derive prompt authors them that
		// way), so its rule destination doubles as a skill target rather than dropping
		// to a Claude-shaped .claude/skills path the repo's tool never reads. Scoped to
		// Cursor: other rule-only tools author skills as standalone frontmatter files
		// (see skillsMergeIntoRuleFile), so their rule file is NOT a skill target.
		return isRuleDestination(targetFile) && skillsMergeIntoRuleFile(RuleFileTool(targetFile))
	}
	return isRuleDestination(targetFile)
}

// parseTargetPlacement maps the LLM's placement string to the enum; an empty or
// unrecognized value defaults to APPEND_TO_FILE (the safe, tool-agnostic action).
func parseTargetPlacement(placement string) promptlibraryv1.ArtifactTargetPlacement {
	switch strings.ToLower(strings.TrimSpace(placement)) {
	case "new_referenced_file":
		return promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_NEW_REFERENCED_FILE
	default:
		return promptlibraryv1.ArtifactTargetPlacement_ARTIFACT_TARGET_PLACEMENT_APPEND_TO_FILE
	}
}

// nearestRuleFile picks the repo's existing rule file that best fits an artifact: the
// one whose directory is the deepest ancestor (by path-segment nesting) of any cited
// path (so a rule about web/ code targets web/CLAUDE.md when present), else the
// repo-root-most rule file, else defaultRuleFile when the repo carries none.
func nearestRuleFile(citedPaths []string, repoTree *promptlibraryv1.RepoTree) string {
	// Only rule destinations are appendable rule targets — a skill/command/prompt file
	// in the tree is NOT somewhere a rule may be appended, so filter them out before
	// picking. When the repo has only skill files (no rule file), fall to the default.
	ruleFiles := make([]string, 0, len(repoTree.GetRuleFiles()))
	for _, rf := range repoTree.GetRuleFiles() {
		if isRuleDestination(rf) {
			ruleFiles = append(ruleFiles, rf)
		}
	}
	if len(ruleFiles) == 0 {
		return defaultRuleFile
	}
	best, bestDepth := "", -1
	for _, ruleFile := range ruleFiles {
		dir := path.Dir(ruleFile)
		for _, cited := range citedPaths {
			cleaned := normalizeRepoPath(cited)
			if dir == "." || cleaned == dir || strings.HasPrefix(cleaned, dir+"/") {
				if depth := dirDepth(dir); depth > bestDepth {
					best, bestDepth = ruleFile, depth
				}
			}
		}
	}
	if best != "" {
		return best
	}
	// No cited path sits under any rule file — prefer the shallowest (repo-root) one.
	shallowest := ruleFiles[0]
	for _, ruleFile := range ruleFiles[1:] {
		if dirDepth(path.Dir(ruleFile)) < dirDepth(path.Dir(shallowest)) {
			shallowest = ruleFile
		}
	}
	return shallowest
}

// dirDepth is a directory's path-segment nesting depth: "." (repo root) is 0, "client"
// is 1, "a/b" is 2 — so a deeper-nested rules file outranks a shallow one with a longer
// name (which a raw string length would get backwards).
func dirDepth(dir string) int {
	if dir == "." || dir == "" {
		return 0
	}
	return strings.Count(dir, "/") + 1
}
