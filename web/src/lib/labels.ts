import { ArtifactScope, ArtifactType, IdeType } from "@harness-control-plane/gen/harnessctl/v1/types_pb";

export const IDE_OPTIONS: { value: IdeType; label: string }[] = [
  { value: IdeType.CLAUDE_CODE, label: "Claude Code" },
  { value: IdeType.CURSOR, label: "Cursor" },
  { value: IdeType.CODEX, label: "Codex" },
  { value: IdeType.KIRO, label: "Kiro" },
];

export const TYPE_OPTIONS: { value: ArtifactType; label: string }[] = [
  { value: ArtifactType.SKILL, label: "Skill" },
  { value: ArtifactType.RULE, label: "Rule" },
  { value: ArtifactType.AGENT, label: "Agent" },
  { value: ArtifactType.CLAUDE_MD, label: "CLAUDE.md" },
  { value: ArtifactType.AGENTS_MD, label: "AGENTS.md" },
  { value: ArtifactType.HOOK, label: "Hook" },
];

export const SCOPE_OPTIONS: { value: ArtifactScope; label: string; hint: string }[] = [
  { value: ArtifactScope.USER_GLOBAL, label: "User (machine-wide)", hint: "The tool's config root in each person's home directory." },
  { value: ArtifactScope.REPO_LOCAL, label: "Repository", hint: "Every known checkout of one repository. Placed, never committed." },
];

export function ideLabel(v: IdeType): string {
  return IDE_OPTIONS.find((o) => o.value === v)?.label ?? "Unknown tool";
}

export function typeLabel(v: ArtifactType): string {
  return TYPE_OPTIONS.find((o) => o.value === v)?.label ?? "Unknown type";
}

export function scopeLabel(v: ArtifactScope, repository = ""): string {
  if (v === ArtifactScope.REPO_LOCAL) return repository ? repository : "Repository";
  if (v === ArtifactScope.USER_GLOBAL) return "User (machine-wide)";
  return "Unknown scope";
}

// Mirrors the server's hook vocabulary so the composer can offer suggestions; the server remains the validator.
export const HOOK_EVENTS: Partial<Record<IdeType, string[]>> = {
  [IdeType.CLAUDE_CODE]: ["PreToolUse", "PostToolUse", "SessionStart", "Notification", "PermissionRequest", "Stop", "UserPromptSubmit"],
  [IdeType.CURSOR]: ["sessionStart", "beforeSubmitPrompt", "beforeReadFile", "subagentStart", "stop"],
  [IdeType.CODEX]: ["SessionStart", "SubagentStart", "PreToolUse", "PostToolUse", "Stop"],
  [IdeType.KIRO]: [
    "SessionStart", "Stop", "PreToolUse", "PostToolUse", "PreTaskExec", "PostTaskExec",
    "UserPromptSubmit", "PostFileCreate", "PostFileSave", "PostFileDelete",
  ],
};
