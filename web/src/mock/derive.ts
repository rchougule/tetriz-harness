import { ArtifactScope, ArtifactType, IdeType } from "@harness-control-plane/gen/harnessctl/v1/types_pb";

// A trimmed copy of the server's install-path derivation, enough to make the mock's previews realistic.
export function installPath(ide: IdeType, type: ArtifactType, slug: string, scope: ArtifactScope): string | null {
  const repo = scope === ArtifactScope.REPO_LOCAL;
  switch (type) {
    case ArtifactType.CLAUDE_MD:
      return repo ? "CLAUDE.md" : ".claude/CLAUDE.md";
    case ArtifactType.AGENTS_MD:
      return repo ? "AGENTS.md" : null;
    case ArtifactType.HOOK:
      if (ide === IdeType.KIRO) return repo ? `.kiro/hooks/${slug}.kiro.hook` : null;
      if (ide === IdeType.CODEX && !repo) return `hooks/${slug}.sh`;
      return `${dirFor(ide)}/hooks/${slug}.sh`;
    default:
      break;
  }
  const table: Partial<Record<IdeType, Partial<Record<ArtifactType, string>>>> = {
    [IdeType.CLAUDE_CODE]: { [ArtifactType.SKILL]: ".claude/skills/%/SKILL.md", [ArtifactType.AGENT]: ".claude/agents/%.md", [ArtifactType.RULE]: ".claude/rules/%.md" },
    [IdeType.CURSOR]: { [ArtifactType.SKILL]: ".cursor/skills/%/SKILL.md", [ArtifactType.AGENT]: ".cursor/agents/%.md", [ArtifactType.RULE]: ".cursor/rules/%.mdc" },
    [IdeType.CODEX]: repo
      ? { [ArtifactType.SKILL]: ".agents/skills/%/SKILL.md", [ArtifactType.AGENT]: ".codex/agents/%.md", [ArtifactType.RULE]: ".codex/rules/%.md" }
      : { [ArtifactType.SKILL]: ".agents/skills/%/SKILL.md" },
    [IdeType.KIRO]: { [ArtifactType.SKILL]: ".kiro/skills/%/SKILL.md", [ArtifactType.AGENT]: ".kiro/agents/%.md", [ArtifactType.RULE]: ".kiro/steering/%.md" },
  };
  const f = table[ide]?.[type];
  return f ? f.replace("%", slug) : null;
}

function dirFor(ide: IdeType): string {
  return ide === IdeType.CURSOR ? ".cursor" : ide === IdeType.CODEX ? ".codex" : ".claude";
}

export function fileInstallPath(root: string, rel: string): string {
  const dir = root.includes("/") ? root.slice(0, root.lastIndexOf("/")) : "";
  return dir ? `${dir}/${rel}` : rel;
}
