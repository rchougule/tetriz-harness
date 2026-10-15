// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

package recommendations

import (
	"encoding/json"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	commonv1 "github.com/tetriz-ai/tetriz-harness/gen/go/recommendations/common/v1"
)

// TestSynthesizeHookBody_PerTool asserts each tool's hook body re-parses as loadable JSON AND
// carries the tool's distinguishing structure — the shape a hand-written serializer is most likely
// to get wrong (nested vs flat family, Kiro's array manifest, Antigravity's hook-name key).
func TestSynthesizeHookBody_PerTool(t *testing.T) {
	const (
		slug    = "run-tests-on-edit"
		title   = "Run tests after an edit"
		event   = "PostToolUse"
		matcher = "Edit|Write|MultiEdit"
		command = "go test ./..."
	)
	tests := []struct {
		name   string
		tool   commonv1.IdeType
		assert func(t *testing.T, body string, parsed map[string]any)
	}{
		{
			name: "claude nested matcher-groups",
			tool: commonv1.IdeType_IDE_TYPE_CLAUDE_CODE,
			assert: func(t *testing.T, body string, parsed map[string]any) {
				group := parsed["hooks"].(map[string]any)[event].([]any)[0].(map[string]any)
				assert.Equal(t, matcher, group["matcher"])
				inner := group["hooks"].([]any)[0].(map[string]any)
				assert.Equal(t, "command", inner["type"])
				assert.Equal(t, command, inner["command"])
			},
		},
		{
			name: "codex nested matcher-groups (own file)",
			tool: commonv1.IdeType_IDE_TYPE_CODEX,
			assert: func(t *testing.T, body string, parsed map[string]any) {
				group := parsed["hooks"].(map[string]any)[event].([]any)[0].(map[string]any)
				assert.NotNil(t, group["hooks"], "codex is nested (inner hooks[]), not flat")
			},
		},
		{
			name: "cursor flat entries (no type, no inner hooks[])",
			tool: commonv1.IdeType_IDE_TYPE_CURSOR,
			assert: func(t *testing.T, body string, parsed map[string]any) {
				assert.EqualValues(t, 1, parsed["version"])
				entry := parsed["hooks"].(map[string]any)[event].([]any)[0].(map[string]any)
				assert.Equal(t, command, entry["command"])
				assert.NotContains(t, entry, "type", "cursor entries have no type field")
				assert.NotContains(t, entry, "hooks", "cursor is flat, no inner hooks[]")
			},
		},
		{
			name: "copilot flat standalone file (bash field, no command/matcher)",
			tool: commonv1.IdeType_IDE_TYPE_COPILOT,
			assert: func(t *testing.T, body string, parsed map[string]any) {
				assert.EqualValues(t, 1, parsed["version"])
				assert.Equal(t, false, parsed["disableAllHooks"])
				entry := parsed["hooks"].(map[string]any)[event].([]any)[0].(map[string]any)
				assert.Equal(t, "command", entry["type"])
				assert.Equal(t, command, entry["bash"], "copilot carries the command in the OS-specific bash field")
				assert.NotContains(t, entry, "command", "copilot has no cross-platform command field")
				assert.NotContains(t, entry, "matcher", "copilot entries carry no Claude-style matcher")
				assert.NotContains(t, entry, "hooks", "copilot is flat")
			},
		},
		{
			name: "antigravity keyed by hook-name",
			tool: commonv1.IdeType_IDE_TYPE_ANTIGRAVITY,
			assert: func(t *testing.T, body string, parsed map[string]any) {
				named := parsed[slug].(map[string]any) // top-level key is the slug (hook-name), not "hooks"
				assert.Equal(t, true, named["enabled"])
				group := named[event].([]any)[0].(map[string]any)
				assert.NotNil(t, group["hooks"], "antigravity is nested inside the named hook")
			},
		},
		{
			name: "kiro standalone .kiro.hook manifest (array, trigger, action)",
			tool: commonv1.IdeType_IDE_TYPE_KIRO,
			assert: func(t *testing.T, body string, parsed map[string]any) {
				assert.Equal(t, "v1", parsed["version"])
				entry := parsed["hooks"].([]any)[0].(map[string]any) // hooks is an ARRAY, not a map
				assert.Equal(t, slug, entry["name"])
				assert.Equal(t, event, entry["trigger"], "kiro uses trigger, not an event map key")
				action := entry["action"].(map[string]any)
				assert.Equal(t, command, action["command"], "kiro command is under action, not an inner hooks[]")
				assert.Equal(t, true, entry["enabled"])
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, err := synthesizeHookBody(tc.tool, slug, title, event, matcher, command)
			require.NoError(t, err)
			var parsed map[string]any
			require.NoError(t, json.Unmarshal([]byte(body), &parsed), "hook body must be loadable JSON:\n%s", body)
			tc.assert(t, body, parsed)
		})
	}
}

// TestSynthesizeHookBody_ShellOperatorsLiteral guards the copy-paste snippet against Go's default
// JSON HTML-escaping: a command's &, <, > must appear LITERALLY in the body (as "&&"), never Go's default
// HTML-escaped unicode form, which reads as garbage when the admin pastes the snippet yet is still
// valid JSON. Regression guard for the escaped-operator display bug.
func TestSynthesizeHookBody_ShellOperatorsLiteral(t *testing.T) {
	const command = `grep -rE '(API_KEY|SECRET)' . && echo '<secret> & flagged' || exit 0`
	body, err := synthesizeHookBody(
		commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, "secret-scan", "Secret scan",
		"UserPromptSubmit", "Edit|Write|MultiEdit", command,
	)
	require.NoError(t, err)

	// Raw body carries the operators verbatim — literal "&&" and "<secret>", never Go's default
	// HTML-escaped unicode form (the failure mode this guards). The positive substring check is
	// non-vacuous: before the fix the body held the escaped form, so these literals were absent.
	assert.Contains(t, body, "&&", "shell && must stay literal in the body")
	assert.Contains(t, body, "<secret>", "angle brackets must stay literal")

	// Escaping-off changes bytes, not semantics: the body still parses back to the exact command.
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &parsed))
	group := parsed["hooks"].(map[string]any)["UserPromptSubmit"].([]any)[0].(map[string]any)
	inner := group["hooks"].([]any)[0].(map[string]any)
	assert.Equal(t, command, inner["command"])
}

// TestSynthesizeAgentBody_PerTool asserts each tool's agent body is its correct format: TOML
// (developer_instructions), every other tool Markdown+YAML frontmatter wrapping the prose.
func TestSynthesizeAgentBody_PerTool(t *testing.T) {
	const (
		name  = "Bug Fixer"
		desc  = "Fixes bugs found in review"
		model = "sonnet"
		prose = "# System Prompt\nYou fix bugs.\n"
	)
	tools := []string{"Read", "Bash"}

	t.Run("codex is valid TOML: identifier, effort+sandbox from archetype, never YAML", func(t *testing.T) {
		body, err := synthesizeAgentBody(commonv1.IdeType_IDE_TYPE_CODEX, name, desc, tools, model, prose)
		require.NoError(t, err)
		assert.NotContains(t, body, "---", "codex agent must not be YAML frontmatter")
		// The whole file must parse as TOML and every field must round-trip.
		tree, err := toml.Load(body)
		require.NoError(t, err, "codex agent body must be loadable TOML:\n%s", body)
		assert.Equal(t, "bug-fixer", tree.Get("name"), "identifier is the normalized kebab slug, not the display name")
		assert.Equal(t, desc, tree.Get("description"))
		// model "sonnet" → mid tier → medium effort; tools include Bash (write/exec) → workspace-write.
		assert.Equal(t, "medium", tree.Get("model_reasoning_effort"), "model tier maps to reasoning effort")
		assert.Equal(t, "workspace-write", tree.Get("sandbox_mode"), "a write/exec tool widens the sandbox")
		assert.Equal(t, prose, strings.TrimSuffix(tree.Get("developer_instructions").(string), "\n"),
			"prose round-trips through the developer_instructions string")
	})

	t.Run("codex read-only tools stay read-only, high tier → high effort", func(t *testing.T) {
		body, err := synthesizeAgentBody(commonv1.IdeType_IDE_TYPE_CODEX, "Code Reviewer", desc, []string{"Read", "Grep"}, "high", prose)
		require.NoError(t, err)
		tree, err := toml.Load(body)
		require.NoError(t, err, "codex agent body must be loadable TOML:\n%s", body)
		assert.Equal(t, "read-only", tree.Get("sandbox_mode"), "a read-only reviewer must not inherit a write sandbox")
		assert.Equal(t, "high", tree.Get("model_reasoning_effort"))
	})

	// The prose is a multiline BASIC string, so it must survive a real TOML decode byte-for-byte —
	// both a backslash payload (a Windows path / regex, which a naive literal-vs-basic choice gets wrong)
	// and a triple-single-quote run (a Python docstring in the system prompt, which a ''' literal string
	// could not hold and would silently mutate). Both are the exact Codex serializer findings.
	for _, tc := range []struct {
		name  string
		prose string
	}{
		{"backslashes (windows path / regex)", `Run on C:\src\repo and match \d+ digits.`},
		{"triple-single-quote docstring", "Example:\n```python\ndef f():\n    '''docstring'''\n    return 1\n```"},
		{"embedded triple-double-quote", `Prompt: """do the thing""" carefully.`},
	} {
		t.Run("codex prose round-trips: "+tc.name, func(t *testing.T) {
			body, err := synthesizeAgentBody(commonv1.IdeType_IDE_TYPE_CODEX, name, desc, nil, "", tc.prose)
			require.NoError(t, err)
			tree, err := toml.Load(body)
			require.NoError(t, err, "codex agent body must be loadable TOML:\n%s", body)
			assert.Equal(t, tc.prose, strings.TrimSuffix(tree.Get("developer_instructions").(string), "\n"),
				"prose is preserved byte-for-byte after a real TOML decode")
		})
	}

	for _, tool := range []commonv1.IdeType{
		commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, commonv1.IdeType_IDE_TYPE_CURSOR,
		commonv1.IdeType_IDE_TYPE_COPILOT, commonv1.IdeType_IDE_TYPE_KIRO,
		commonv1.IdeType_IDE_TYPE_ANTIGRAVITY,
	} {
		t.Run("markdown+frontmatter: "+tool.String(), func(t *testing.T) {
			body, err := synthesizeAgentBody(tool, name, desc, tools, model, prose)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(body, "---\n"), "agent file opens with YAML frontmatter")
			fm, rest, found := strings.Cut(strings.TrimPrefix(body, "---\n"), "\n---\n")
			require.True(t, found, "frontmatter must close with ---")
			assert.Contains(t, fm, `name: "bug-fixer"`, "identifier is the normalized kebab slug, not the display name")
			assert.Contains(t, fm, `description: "Fixes bugs found in review"`)
			assert.Contains(t, strings.TrimSpace(rest), "You fix bugs", "prose preserved after frontmatter")
		})
	}

	// tools rendering differs: Claude/Cursor comma-string vs the array tools.
	claude, _ := synthesizeAgentBody(commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, name, desc, tools, model, prose)
	assert.Contains(t, claude, `tools: "Read, Bash"`, "claude tools is a QUOTED comma-separated scalar (injection-safe)")
	// A YAML-significant tool value must stay inside the quoted scalar, not break/inject the frontmatter.
	injected, _ := synthesizeAgentBody(commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, name, desc, []string{"Read: files", "Bash"}, model, prose)
	assert.Contains(t, injected, `tools: "Read: files, Bash"`, "a colon-bearing tool value is contained by the quoted scalar")
	assert.NotContains(t, injected, "\ntools: Read: files", "the raw colon must not produce invalid/injecting frontmatter")
	// Fail-closed: an agent with an EMPTY tool scope must NOT omit `tools` (every tools-field tool treats
	// an absent list as access to ALL tools) — it falls back to that tool's minimal read-only set, so a
	// scope-less agent can never silently edit/execute. Each tool renders the scope in its own format.
	scopelessChecks := []struct {
		tool commonv1.IdeType
		want string
	}{
		{commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, `tools: "Read, Grep, Glob"`},
		{commonv1.IdeType_IDE_TYPE_CURSOR, `tools: "Read, Grep, Glob"`},
		{commonv1.IdeType_IDE_TYPE_COPILOT, `tools: ["Read", "Grep", "Glob"]`},
		{commonv1.IdeType_IDE_TYPE_KIRO, `tools: ["read"]`}, // Kiro keys on lowercase capability names
		{commonv1.IdeType_IDE_TYPE_ANTIGRAVITY, "tools:\n  - \"Read\"\n  - \"Grep\"\n  - \"Glob\""},
	}
	for _, tc := range scopelessChecks {
		scopeless, err := synthesizeAgentBody(tc.tool, name, desc, nil, model, prose)
		require.NoError(t, err)
		assert.Contains(t, scopeless, tc.want, "%s empty tool scope → read-only fallback, never omitted", tc.tool)
	}
	copilot, _ := synthesizeAgentBody(commonv1.IdeType_IDE_TYPE_COPILOT, name, desc, tools, model, prose)
	assert.Contains(t, copilot, `tools: ["Read", "Bash"]`, "copilot tools is an inline array")
	anti, _ := synthesizeAgentBody(commonv1.IdeType_IDE_TYPE_ANTIGRAVITY, name, desc, tools, "", prose)
	assert.Contains(t, anti, "model: \"inherit\"", "antigravity model defaults to inherit")
	assert.Contains(t, anti, "subagent: true")
}

// TestResolveAgentModel proves the LLM's SEMANTIC model tier maps onto each tool's real model
// vocabulary — never leaking a raw tier like "mid_tier" into a deployable agent file. A tool
// with no generic-tier→name mapping (Codex/Copilot/Kiro) yields "" so the caller omits the field.
func TestResolveAgentModel(t *testing.T) {
	tests := []struct {
		name string
		tool commonv1.IdeType
		tier string
		want string
	}{
		{"claude mid_tier → sonnet", commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, "mid_tier", "sonnet"},
		{"claude a higher tier → opus", commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, "a higher tier", "opus"},
		{"claude low/fast → haiku", commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, "fast", "haiku"},
		{"claude unknown → omitted", commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, "gpt-9", ""},
		{"claude empty → omitted", commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, "", ""},
		{"claude 'provider default' does NOT match pro → omitted", commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, "provider default", ""},
		{"claude 'appropriate model' does NOT match pro → omitted", commonv1.IdeType_IDE_TYPE_CLAUDE_CODE, "appropriate model", ""},
		{"antigravity 'provider default' → inherit (no false pro-match)", commonv1.IdeType_IDE_TYPE_ANTIGRAVITY, "provider default", "inherit"},
		{"cursor mid → sonnet (claude-compatible)", commonv1.IdeType_IDE_TYPE_CURSOR, "medium", "sonnet"},
		{"antigravity high → pro", commonv1.IdeType_IDE_TYPE_ANTIGRAVITY, "high", "pro"},
		{"antigravity mid → flash", commonv1.IdeType_IDE_TYPE_ANTIGRAVITY, "mid_tier", "flash"},
		{"antigravity sonnet (mid) → flash", commonv1.IdeType_IDE_TYPE_ANTIGRAVITY, "sonnet", "flash"},
		{"antigravity unknown → inherit default", commonv1.IdeType_IDE_TYPE_ANTIGRAVITY, "gpt-9", "inherit"},
		{"codex any tier → omitted (no model-name field)", commonv1.IdeType_IDE_TYPE_CODEX, "high", ""},
		{"copilot any tier → omitted", commonv1.IdeType_IDE_TYPE_COPILOT, "mid_tier", ""},
		{"kiro any tier → omitted", commonv1.IdeType_IDE_TYPE_KIRO, "high", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveAgentModel(tc.tool, tc.tier))
		})
	}
}

// TestNormalizeAgentIdentifier proves a display-style agent name is coerced to the kebab identifier
// tools discover the file by — never left as "Bug Fixer".
func TestNormalizeAgentIdentifier(t *testing.T) {
	tests := []struct{ in, want string }{
		{"bug-fixer", "bug-fixer"},
		{"Bug Fixer", "bug-fixer"},
		{"/code-reviewer", "code-reviewer"},
		{"Test  Writer_v2", "test-writer-v2"},
		{"  Trailing- ", "trailing"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, normalizeAgentIdentifier(tc.in), "normalize %q", tc.in)
	}
}

// TestCodexCapabilityMapping proves the generic archetype maps onto Codex's own capability vocabulary:
// tool scope → sandbox_mode (read-only preserved), model tier → reasoning effort.
func TestCodexCapabilityMapping(t *testing.T) {
	assert.Equal(t, "read-only", codexSandboxMode([]string{"Read", "Grep", "Glob"}))
	assert.Equal(t, "workspace-write", codexSandboxMode([]string{"Read", "Edit"}), "a recognized write/exec tool widens the sandbox")
	// Fail-closed: an unknown tool, an empty scope, or a combined comma-scalar must NOT broaden to write.
	assert.Equal(t, "read-only", codexSandboxMode([]string{"MysteryTool"}), "an unknown tool stays read-only, never widens")
	assert.Equal(t, "read-only", codexSandboxMode(nil), "no tools → fail-closed read-only, not the ambient default")
	assert.Equal(t, "read-only", codexSandboxMode([]string{"Read, Grep"}), "a combined scalar is unrecognized → read-only")

	assert.Equal(t, "high", codexReasoningEffort("a higher tier"))
	assert.Equal(t, "medium", codexReasoningEffort("mid_tier"))
	assert.Equal(t, "low", codexReasoningEffort("fast/cheap"))
	assert.Empty(t, codexReasoningEffort(""), "no tier → omit")
	assert.Empty(t, codexReasoningEffort("gpt-9"), "unrecognized tier → omit")
}

// TestSynthesize_UnknownToolErrors proves an unresolved tool cannot yield a body — the gate relies
// on this to hide (never persist) a hook/agent it cannot serialize.
func TestSynthesize_UnknownToolErrors(t *testing.T) {
	_, hookErr := synthesizeHookBody(commonv1.IdeType_IDE_TYPE_UNSPECIFIED, "s", "t", "e", "m", "c")
	require.Error(t, hookErr)
	_, agentErr := synthesizeAgentBody(commonv1.IdeType_IDE_TYPE_UNSPECIFIED, "n", "d", nil, "", "p")
	require.Error(t, agentErr)
}

// TestSynthesize_AllTargetToolsCovered is the drift guard: every tool with a hook/agent destination
// in toolArtifactTargets MUST have a working serializer, so a VISIBLE hook/agent never hits the
// buildArtifactContent error-fallback. Binds the destination table to this file.
func TestSynthesize_AllTargetToolsCovered(t *testing.T) {
	for tool := range toolArtifactTargets {
		_, hookErr := synthesizeHookBody(tool, "s", "t", "PostToolUse", "", "cmd")
		require.NoErrorf(t, hookErr, "tool %s has a hook destination but no hook serializer", tool)
		_, agentErr := synthesizeAgentBody(tool, "n", "d", []string{"Read"}, "", "prose")
		require.NoErrorf(t, agentErr, "tool %s has an agent destination but no agent serializer", tool)
	}
}
