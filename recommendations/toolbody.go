// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
)

// This file owns the per-tool BODY SHAPE for HOOK and AGENT artifacts — the deterministic
// serializers that turn the LLM's structured semantics (event/matcher/command for a hook;
// name/description/tools/model + instruction prose for an agent) into the exact bytes the
// resolved tool can load. The LLM authors WHICH event/matcher/command (in the resolved tool's
// vocabulary, told to it via the derive prompt); Go authors the STRUCTURE, so a body can never
// diverge from its destination tool. Destination paths + placement stay in
// repotree.go (toolArtifactTargets / destHookPath / hookTargetPlacement); this file owns shape only.
//
// Two hook families: NESTED matcher-groups (Claude/Codex/Antigravity — the command lives inside
// an inner `hooks[]`) vs FLAT entries (Cursor/Copilot/Kiro — the command sits on the entry). The
// synthesizers emit indented JSON (a human copy-pastes the snippet); for a merge-placement hook
// the output is the top-level object to merge at the config file's root, for a standalone hook it
// is the whole file.

// hookIndent is the JSON indent for a copy-pasted hook snippet (readable for the admin).
const hookIndent = "  "

// synthesizeHookBody returns the resolved tool's exact hook-config body for the structured
// event/matcher/command. matcher may be empty (a non-tool event — Claude SessionStart etc. — omits
// it, matching "absent = every tool"). Returns an error for an unspecified/unknown tool so the gate
// hides the hook rather than persisting an unloadable body.
func synthesizeHookBody(tool commonv1.IdeType, slug, description, event, matcher, command string) (string, error) {
	var v any
	switch tool {
	case commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, commonv1.IdeType_IDE_TYPE_CODEX:
		// Nested matcher-groups: {"hooks":{"<event>":[{"matcher":..,"hooks":[{"type":"command","command":..}]}]}}.
		// Same shape for both; they differ only by destination file + edit-matcher vocabulary (authored upstream).
		v = nestedHookFile{Hooks: map[string][]nestedMatcherGroup{
			event: {{Matcher: matcher, Hooks: []commandHook{{Type: "command", Command: command}}}},
		}}
	case commonv1.IdeType_IDE_TYPE_CURSOR:
		// Flat entries, no `type`, no inner hooks[]: {"version":1,"hooks":{"<event>":[{"command":..,"matcher"?:..}]}}.
		v = cursorHookFile{Version: 1, Hooks: map[string][]flatEntry{
			event: {{Command: command, Matcher: matcher}},
		}}
	case commonv1.IdeType_IDE_TYPE_COPILOT:
		// Flat standalone file (per-slug, created not merged). Copilot command entries carry the
		// executable in the OS-specific `bash`/`powershell` fields — NOT a `command` field, and NOT a
		// Claude-style `matcher` (its per-event example carries neither). We hold one LLM-authored
		// command, so it goes in `bash` (dev-tooling commands are bash-idiomatic); a Windows admin adds
		// the `powershell` variant rather than us duplicating a possibly-incompatible script into it.
		v = copilotHookFile{Version: 1, DisableAllHooks: false, Hooks: map[string][]copilotEntry{
			event: {{Type: "command", Bash: command}},
		}}
	case commonv1.IdeType_IDE_TYPE_ANTIGRAVITY:
		// Merge, but keyed at the top level by HOOK-NAME (the slug), each holding {enabled + event arrays}.
		// The named-hook object mixes a fixed `enabled` with the dynamic event key, so it is built as a map
		// (json.Marshal sorts the two keys — order is immaterial for a JSON object).
		v = map[string]any{slug: map[string]any{
			"enabled": true,
			event:     []nestedMatcherGroup{{Matcher: matcher, Hooks: []commandHook{{Type: "command", Command: command}}}},
		}}
	case commonv1.IdeType_IDE_TYPE_KIRO:
		// Standalone .kiro.hook manifest: {"version":"v1","hooks":[{name,description,trigger,matcher?,action:{type,command},enabled}]}.
		// The lifecycle event is the `trigger`; the command is nested under `action` (NOT an inner hooks[]).
		v = kiroHookFile{Version: "v1", Hooks: []kiroHookEntry{{
			Name: slug, Description: description, Trigger: event, Matcher: matcher,
			Action:  commandHook{Type: "command", Command: command},
			Enabled: true,
		}}}
	default:
		return "", fmt.Errorf("no hook body serializer for tool %s", tool)
	}
	body, err := marshalHookBody(v)
	if err != nil {
		return "", fmt.Errorf("marshalling %s hook body: %w", tool, err)
	}
	return body, nil
}

// marshalHookBody renders v as indented JSON with HTML escaping OFF, so a command's shell operators
// (&, <, >) stay literal in the copy-paste snippet instead of Go's default HTML-escaped unicode form,
// which is valid JSON but reads as garbage when a human pastes the snippet. This also matches the
// literal-operator convention the hook-body validators already assume (see validators.go
// jsonStringEscaper). encoding/json's Encoder appends a trailing newline — trim it.
func marshalHookBody(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", hookIndent)
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("encoding hook body to JSON: %w", err)
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// synthesizeAgentBody returns the resolved tool's exact agent-definition body: a Markdown file with
// YAML frontmatter for every tool except, which uses a TOML file whose `developer_instructions`
// carries the system prompt. The agent is keyed by its IDENTIFIER (the validated kebab slug, matching
// the on-disk filename) — a display-style name like "Bug Fixer" would leave the file undiscoverable.
// rawModelTier is the LLM's semantic tier, mapped to each tool's own model vocabulary here. prose is the
// LLM-authored instruction body. Returns an error for an unspecified/unknown tool.
func synthesizeAgentBody(tool commonv1.IdeType, identifier, description string, tools []string, rawModelTier, prose string) (string, error) {
	id := normalizeAgentIdentifier(identifier)
	model := resolveAgentModel(tool, rawModelTier)
	switch tool {
	case commonv1.IdeType_IDE_TYPE_CODEX:
		return synthesizeCodexAgentTOML(id, description, tools, rawModelTier, prose), nil
	case commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, commonv1.IdeType_IDE_TYPE_CURSOR:
		// Claude/Cursor: name/description, tools as a COMMA-separated string, optional model. The joined
		// scalar is QUOTED — an LLM tool value carrying YAML-significant text (a colon, '#', a newline)
		// would otherwise produce malformed or field-injecting frontmatter (e.g. `tools: Read: files`).
		// The `tools` field is ALWAYS emitted, FAIL-CLOSED: Claude/Cursor inherit the ambient (all) tool
		// set when `tools` is omitted, so a scope-less agent would silently gain edit/execute — a
		// read-only reviewer must not. An empty scope falls back to a minimal read-only set.
		fm := []yamlField{{"name", yamlQuote(id)}, {"description", yamlQuote(description)}}
		agentTools := tools
		if len(agentTools) == 0 {
			agentTools = agentReadOnlyFallback(tool)
		}
		fm = append(fm, yamlField{"tools", yamlQuote(strings.Join(agentTools, ", "))})
		if model != "" {
			fm = append(fm, yamlField{"model", yamlQuote(model)})
		}
		return frontmatterMarkdown(fm, prose), nil
	case commonv1.IdeType_IDE_TYPE_COPILOT, commonv1.IdeType_IDE_TYPE_KIRO:
		// Copilot / Kiro: description required, tools as a JSON-style array (Kiro's are capability names).
		// The `tools` field is ALWAYS emitted, FAIL-CLOSED: both tools treat an absent `tools` as access
		// to every tool, so a scope-less agent would silently gain edit/execute; an empty scope falls
		// back to the tool's minimal read-only set instead.
		fm := []yamlField{{"name", yamlQuote(id)}, {"description", yamlQuote(description)}}
		agentTools := tools
		if len(agentTools) == 0 {
			agentTools = agentReadOnlyFallback(tool)
		}
		fm = append(fm, yamlField{"tools", yamlInlineArray(agentTools)})
		if model != "" {
			fm = append(fm, yamlField{"model", yamlQuote(model)})
		}
		return frontmatterMarkdown(fm, prose), nil
	case commonv1.IdeType_IDE_TYPE_ANTIGRAVITY:
		// Antigravity: name/description, model (inherit/flash/pro — resolveAgentModel defaults inherit),
		// tools as a YAML block array (fail-closed to a read-only set when empty, as for the other
		// tools-field tools), subagent:true. Built directly for the block-array tools.
		agentTools := tools
		if len(agentTools) == 0 {
			agentTools = agentReadOnlyFallback(tool)
		}
		var b strings.Builder
		b.WriteString("---\n")
		b.WriteString("name: " + yamlQuote(id) + "\n")
		b.WriteString("description: " + yamlQuote(description) + "\n")
		b.WriteString("model: " + yamlQuote(model) + "\n")
		b.WriteString("tools:\n")
		for _, t := range agentTools {
			b.WriteString("  - " + yamlQuote(t) + "\n")
		}
		b.WriteString("subagent: true\n---\n\n")
		b.WriteString(prose)
		return b.String(), nil
	default:
		return "", fmt.Errorf("no agent body serializer for tool %s", tool)
	}
}

// normalizeAgentIdentifier coerces an agent identity to the lowercase-hyphenated form tools key
// invocation/file-discovery on. The derive pipeline already emits a kebab slug for agents; this is a
// defensive backstop (lowercase, strip a leading '/', collapse spaces/underscores to '-').
func normalizeAgentIdentifier(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "/")
	var b strings.Builder
	prevHyphen := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevHyphen = false
		case r == ' ' || r == '_' || r == '-':
			if !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// --- hook shape structs (json tags fix field order; a single-key event map is deterministic) ---

type commandHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

type nestedMatcherGroup struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []commandHook `json:"hooks"`
}

type nestedHookFile struct {
	Hooks map[string][]nestedMatcherGroup `json:"hooks"`
}

type flatEntry struct {
	Command string `json:"command"`
	Matcher string `json:"matcher,omitempty"`
}

type cursorHookFile struct {
	Version int                    `json:"version"`
	Hooks   map[string][]flatEntry `json:"hooks"`
}

type copilotEntry struct {
	Type       string `json:"type"`
	Bash       string `json:"bash,omitempty"`
	Powershell string `json:"powershell,omitempty"`
}

type copilotHookFile struct {
	Version         int                       `json:"version"`
	DisableAllHooks bool                      `json:"disableAllHooks"`
	Hooks           map[string][]copilotEntry `json:"hooks"`
}

type kiroHookEntry struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Trigger     string      `json:"trigger"`
	Matcher     string      `json:"matcher,omitempty"`
	Action      commandHook `json:"action"`
	Enabled     bool        `json:"enabled"`
}

type kiroHookFile struct {
	Version string          `json:"version"`
	Hooks   []kiroHookEntry `json:"hooks"`
}

// --- agent frontmatter helpers ---

type yamlField struct{ key, value string }

// frontmatterMarkdown builds a `---\n<fields>\n---\n\n<prose>` Markdown agent file.
func frontmatterMarkdown(fields []yamlField, prose string) string {
	var b strings.Builder
	b.WriteString("---\n")
	for _, f := range fields {
		b.WriteString(f.key + ": " + f.value + "\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(prose)
	return b.String()
}

// yamlQuote returns a double-quoted, escaped YAML scalar — safe for a name/description that may
// carry a colon, quote, or backslash. Always quoting avoids per-value plain-vs-quoted heuristics.
func yamlQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
}

// yamlInlineArray renders a JSON-style inline string array, e.g. ["read", "write"].
func yamlInlineArray(items []string) string {
	quoted := make([]string, len(items))
	for i, it := range items {
		quoted[i] = yamlQuote(it)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// synthesizeCodexAgentTOML builds's TOML agent file: `name`/`description` scalars, the system
// prompt as `developer_instructions`, and — from the LLM's generic archetype fields —'s own
// capability equivalents: the model tier becomes `model_reasoning_effort` (Codex has no model-NAME
// field, but it does take a reasoning-effort level) and the tool scope becomes `sandbox_mode`, so a
// read-only reviewer stays read-only instead of silently inheriting a write-capable sandbox. Never
// YAML/markdown (Codex ignores those).
func synthesizeCodexAgentTOML(name, description string, tools []string, rawModelTier, prose string) string {
	var b strings.Builder
	b.WriteString("name = " + tomlBasicString(name) + "\n")
	b.WriteString("description = " + tomlBasicString(description) + "\n")
	if effort := codexReasoningEffort(rawModelTier); effort != "" {
		b.WriteString("model_reasoning_effort = " + tomlBasicString(effort) + "\n")
	}
	if sandbox := codexSandboxMode(tools); sandbox != "" {
		b.WriteString("sandbox_mode = " + tomlBasicString(sandbox) + "\n")
	}
	// The prose goes in a multiline BASIC string (""") with every backslash and quote escaped, so it
	// round-trips byte-for-byte: a Windows path or regex survives (backslashes are escaped, not rejected)
	// AND a triple-single-quote run (a Python docstring in the system prompt) is preserved verbatim (a
	// literal ''' string could not contain one and would have to mutate it). Newlines stay literal.
	b.WriteString("developer_instructions = " + tomlMultilineBasicString(prose) + "\n")
	return b.String()
}

// codexReasoningEffort maps the LLM's semantic model tier onto Codex's model_reasoning_effort vocabulary
// (high/medium/low), or "" to omit — so a high-tier reviewer and a low-tier helper don't both collapse
// to the ambient default.
func codexReasoningEffort(rawModelTier string) string {
	// The effort vocabulary (high/medium/low) coincides with the tier constants for high/low; only the
	// mid tier renames to "medium".
	switch agentModelTier(rawModelTier) {
	case tierHigh:
		return tierHigh
	case tierMid:
		return "medium"
	case tierLow:
		return tierLow
	}
	return ""
}

// codexSandboxMode maps a generic tool scope onto Codex's sandbox_mode, FAIL-CLOSED: a derived agent
// gets read-only unless it EXPLICITLY requests a recognized write/exec tool, in which case it widens to
// workspace-write. An omitted, empty, unrecognized, or comma-scalar tool scope therefore stays read-only
// — a reviewer must never silently inherit write access from an ambiguous scope (least privilege; the
// admin widens deliberately). Never danger-full-access — a derived agent does not grant itself network/
// host access.
func codexSandboxMode(tools []string) string {
	for _, tool := range tools {
		if isWriteOrExecTool(tool) {
			return "workspace-write"
		}
	}
	return "read-only"
}

// isWriteOrExecTool reports whether a tool name denotes filesystem-write or command-execution capability.
// Only a RECOGNIZED write/exec tool widens the sandbox; every other name (read-only tools, unknown names,
// a combined "Read, Grep" scalar) leaves the agent read-only — the fail-closed direction.
func isWriteOrExecTool(tool string) bool {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "edit", "write", "multiedit", "notebookedit", "create", "createfile", "delete", "remove",
		"bash", "shell", "exec", "execute", "run", "runcommand", "apply", "applypatch", "patch":
		return true
	}
	return false
}

// tomlBasicString returns a single-line TOML basic (double-quoted) string with the minimal required escapes.
func tomlBasicString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
}

// tomlMultilineBasicString returns a multiline TOML basic string ("""...""") that preserves arbitrary
// prose verbatim. Backslashes are escaped first (\\), then every quote (\"), so any run of 3+ quotes
// that would otherwise close the delimiter is neutralized; newlines are kept literal. The newline right
// after the opening delimiter is trimmed by TOML, so it is cosmetic.
func tomlMultilineBasicString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return "\"\"\"\n" + s + "\n\"\"\""
}

// Semantic model tiers agentModelTier buckets the LLM's free-form value into, before resolveAgentModel
// maps them onto each tool's real model vocabulary.
const (
	tierHigh = "high"
	tierMid  = "mid"
	tierLow  = "low"
)

// defaultReadOnlyAgentTools is the fail-closed scope for a tool-name-based agent whose LLM-authored
// tool list is empty — a minimal read-only set, so an under-specified agent inspects but never
// edits/executes. Even if a name is not this tool's exact spelling, the set grants no write/exec, so
// the security property (never silently inherit the ambient/all tools) holds regardless.
var defaultReadOnlyAgentTools = []string{"Read", "Grep", "Glob"}

// agentReadOnlyFallback is the fail-closed tool scope to emit when the LLM supplies none, per tool's
// vocabulary. Kiro keys on lowercase capability names; every other tools-field tool uses tool names.
func agentReadOnlyFallback(tool commonv1.IdeType) []string {
	if tool == commonv1.IdeType_IDE_TYPE_KIRO {
		return []string{"read"}
	}
	return defaultReadOnlyAgentTools
}

// agentModelTier normalizes the LLM's free-form model tier to high/mid/low (or "" when unrecognizable).
// The derive prompt asks for a SEMANTIC tier ("a higher tier", "a mid tier"), so the raw value carries
// words like "mid_tier"/"high"/"sonnet" that must be bucketed before mapping to a tool's model vocabulary.
// Aliases are matched as WHOLE TOKENS (split on non-alphanumeric), never substrings — otherwise a phrase
// like "provider default" would match "pro" and silently promote the agent to the high tier.
func agentModelTier(raw string) string {
	tokens := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(raw)), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	for _, token := range tokens {
		switch token {
		case "high", "higher", "highest", "senior", "advanced", "opus", "pro", "max":
			return tierHigh
		case "low", "lower", "lowest", "cheap", "cheaper", "fast", "fastest",
			"lightweight", "haiku", "flash", "mini", "lite", "nano", "small":
			return tierLow
		case "mid", "middle", "medium", "balanced", "standard", "sonnet":
			return tierMid
		}
	}
	return ""
}

// resolveAgentModel maps the LLM's semantic model tier onto the resolved tool's real model vocabulary.
// It returns "" when the tool takes no model NAME in its agent file (Codex — reasoning-effort/provider
// model, not a tier) or the tier is unrecognizable for a tool with no safe default; the caller then omits
// the field so the tool applies its own default rather than an unloadable value. Antigravity has a defined
// "inherit" default so it always yields a valid value. Only mapped where the vocabulary is well-established
// (Claude/Cursor aliases, Antigravity tiers); Copilot/Kiro model names are not a generic-tier match, so omit.
func resolveAgentModel(tool commonv1.IdeType, rawTier string) string {
	tier := agentModelTier(rawTier)
	switch tool {
	case commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, commonv1.IdeType_IDE_TYPE_CURSOR:
		switch tier {
		case tierHigh:
			return "opus"
		case tierMid:
			return "sonnet"
		case tierLow:
			return "haiku"
		}
		return ""
	case commonv1.IdeType_IDE_TYPE_ANTIGRAVITY:
		switch tier {
		case tierHigh:
			return "pro"
		case tierMid, tierLow:
			return "flash"
		}
		return "inherit"
	default:
		return ""
	}
}
