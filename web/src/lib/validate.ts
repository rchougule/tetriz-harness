import { ArtifactScope, ArtifactType, IdeType } from "@harness-control-plane/gen/harnessctl/v1/types_pb";
import { HOOK_EVENTS } from "./labels";

export const SLUG_RE = /^[a-z0-9][a-z0-9-]*$/;
export const REPO_RE = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const REL_PATH_RE = /^[A-Za-z0-9.][A-Za-z0-9._-]*(\/[A-Za-z0-9.][A-Za-z0-9._-]*)*$/;

export type AudienceMode = "org" | "persons" | "teams";

export interface AudienceDraft {
  mode: AudienceMode;
  personIds: string[];
  teamIds: string[];
}

export interface FileDraft {
  relativePath: string;
  content: string;
}

export interface ComposerDraft {
  artifactId: string;
  ideType: IdeType;
  type: ArtifactType;
  slug: string;
  name: string;
  multiFile: boolean;
  inlineContent: string;
  files: FileDraft[];
  hookEvent: string;
  hookMatcher: string;
  hookTimeout: string;
  scope: ArtifactScope;
  repository: string;
  audience: AudienceDraft;
  // enforceUntil is a local "YYYY-MM-DD"; empty takes the server's default window.
  enforceUntil: string;
}

// WindowLimits are the server's bounds on a chosen enforcement end.
export interface WindowLimits {
  now: number;
  maxDays: number;
}

// enforceUntilDate reads a chosen day as its local end, capped at the server's maximum so the last allowed day is accepted.
export function enforceUntilDate(day: string, limits: WindowLimits): Date | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(day);
  if (!m) return null;
  const end = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]), 23, 59, 59);
  const max = limits.now + limits.maxDays * 86_400_000 - 60_000;
  return new Date(Math.min(end.getTime(), max));
}

export function validateEnforceUntil(day: string, limits: WindowLimits | undefined): string | undefined {
  if (day === "" || !limits) return undefined;
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(day);
  if (!m) return "Pick a date.";
  const end = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]), 23, 59, 59).getTime();
  if (end <= limits.now) return "Pick a date in the future.";
  const lastDay = new Date(limits.now + limits.maxDays * 86_400_000);
  lastDay.setHours(23, 59, 59, 0);
  if (end > lastDay.getTime()) return `Pick a date at most ${limits.maxDays} days ahead.`;
  return undefined;
}

export type Errors = Partial<Record<string, string>>;

export function validateSlug(slug: string): string | undefined {
  if (slug === "") return "Slug is required.";
  if (slug.length > 100) return "Slug must be at most 100 characters.";
  if (!SLUG_RE.test(slug)) return "Use lower-case letters, digits and dashes; start with a letter or digit.";
  return undefined;
}

export function validateRelativePath(p: string): string | undefined {
  if (p === "") return "File path is required.";
  if (p.length > 255) return "File path must be at most 255 characters.";
  if (!REL_PATH_RE.test(p) || p.split("/").some((s) => s === "." || s === ".."))
    return "Use a relative path like scripts/run.sh (no leading dash, no . or .. segments).";
  return undefined;
}

export function validateRepository(repo: string): string | undefined {
  if (repo === "") return "Repository is required for a repository placement.";
  if (!REPO_RE.test(repo)) return "Use the owner/repo form, for example example-org/example-repo.";
  return undefined;
}

// Mirrors the server's install-path derivation for combinations with no path, so the form fails before the save does.
export function placementUnsupported(ide: IdeType, type: ArtifactType, scope: ArtifactScope): string | undefined {
  const user = scope === ArtifactScope.USER_GLOBAL;
  if (type === ArtifactType.AGENTS_MD && user) return "AGENTS.md can only be placed in a repository.";
  if (ide === IdeType.CODEX && user && (type === ArtifactType.AGENT || type === ArtifactType.RULE))
    return "Codex agents and rules can only be placed in a repository.";
  if (ide === IdeType.KIRO && user && type === ArtifactType.HOOK) return "Kiro hooks can only be placed in a repository.";
  return undefined;
}

export function validateAudience(a: AudienceDraft): string | undefined {
  if (a.mode === "persons" && a.personIds.length === 0) return "Pick at least one person.";
  if (a.mode === "teams" && a.teamIds.length === 0) return "Pick at least one team.";
  return undefined;
}

export function validateDraft(d: ComposerDraft, limits?: WindowLimits): Errors {
  const e: Errors = {};
  if (d.ideType === IdeType.UNSPECIFIED) e.ideType = "Choose a tool.";
  if (d.type === ArtifactType.UNSPECIFIED) e.type = "Choose a type.";
  const slug = validateSlug(d.slug);
  if (slug) e.slug = slug;
  if (d.name.trim() === "") e.name = "Name is required.";
  else if (d.name.length > 200) e.name = "Name must be at most 200 characters.";

  if (d.multiFile) {
    if (d.type !== ArtifactType.SKILL) e.files = "Only a skill can carry several files.";
    else {
      const seen = new Set<string>();
      d.files.forEach((f, i) => {
        const pe = validateRelativePath(f.relativePath);
        if (pe) e[`file.${i}`] = pe;
        else if (seen.has(f.relativePath)) e[`file.${i}`] = "Each file path must be unique.";
        seen.add(f.relativePath);
      });
      if (!d.files.some((f) => f.relativePath === "SKILL.md")) e.files = "A skill needs exactly one SKILL.md file.";
      else if (d.files.find((f) => f.relativePath === "SKILL.md")?.content.trim() === "")
        e.files = "SKILL.md must not be empty.";
    }
  } else if (d.inlineContent.trim() === "") {
    e.inlineContent = d.type === ArtifactType.HOOK ? "The hook script is required." : "Content is required.";
  }

  if (d.type === ArtifactType.HOOK) {
    const events = HOOK_EVENTS[d.ideType] ?? [];
    if (d.hookEvent === "") e.hookEvent = "Event is required.";
    else if (!events.includes(d.hookEvent)) e.hookEvent = "This event is not in the tool's hook vocabulary.";
    if (d.hookTimeout !== "" && !/^\d+$/.test(d.hookTimeout)) e.hookTimeout = "Timeout must be a whole number of seconds.";
  }

  const until = validateEnforceUntil(d.enforceUntil, limits);
  if (until) e.enforceUntil = until;

  if (d.artifactId === "") {
    if (d.scope === ArtifactScope.REPO_LOCAL) {
      const r = validateRepository(d.repository);
      if (r) e.repository = r;
    }
    const unsupported = placementUnsupported(d.ideType, d.type, d.scope);
    if (unsupported) e.scope = unsupported;
    const a = validateAudience(d.audience);
    if (a) e.audience = a;
  }
  return e;
}
