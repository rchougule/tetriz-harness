import { anchoredPathExists, readAnchoredFile, trustedParent } from "./anchoredFs.js";
import { writeAtomic } from "./fsops.js";

// Structured merge of one (event, matcher, command) hook entry into a Claude Code settings.json.
// The file is co-owned by the engineer, so every other key and every undeclared hook survives.

interface HookEntry {
  type: "command";
  command: string;
  timeout?: number;
}

interface MatcherGroup {
  // Matcher-less events omit it.
  matcher?: string;
  hooks: HookEntry[];
}

type SettingsHooks = Record<string, MatcherGroup[]>;

interface Settings {
  hooks?: SettingsHooks;
  [key: string]: unknown;
}

export interface HookMergeSpec {
  event: string;
  // Absent or "" for matcher-less events.
  matcher?: string;
  command: string;
  /** 0 / undefined = unset. */
  timeoutSeconds?: number;
}

/** `added`: the merge inserted the entry, rather than finding one already there. */
export type HookMergeResult = { ok: true; changed: boolean; added?: boolean } | { ok: false; reason: string };

/** A missing matcher and "" are one identity, so matcher-less groups dedupe. */
export const normalizeMatcher = (matcher: unknown): string => (typeof matcher === "string" ? matcher : "");

function isMatcherGroup(v: unknown): v is MatcherGroup {
  if (typeof v !== "object" || v === null) return false;
  const g = v as { matcher?: unknown; hooks?: unknown };
  if (!Array.isArray(g.hooks)) return false;
  return g.matcher === undefined || typeof g.matcher === "string";
}

/** Absent file → `{}`; malformed JSON or a non-object body → an error, and the file is never touched. */
function readSettings(settingsPath: string, root: string): { ok: true; settings: Settings } | { ok: false; reason: string } {
  if (!anchoredPathExists(root, settingsPath)) return { ok: true, settings: {} };
  let raw: string;
  try {
    raw = readAnchoredFile(root, settingsPath).toString("utf8");
  } catch (err) {
    return { ok: false, reason: `read settings.json: ${err instanceof Error ? err.message : String(err)}` };
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return { ok: false, reason: "settings.json is not valid JSON" };
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    return { ok: false, reason: "settings.json does not contain a JSON object" };
  }
  return { ok: true, settings: parsed as Settings };
}

function writeSettingsAtomic(path: string, settings: Settings, root: string): void {
  writeAtomic(path, Buffer.from(`${JSON.stringify(settings, null, 2)}\n`), { root });
}

/**
 * Upsert keyed on (matcher, command). Idempotent, and self-healing: duplicate groups sharing the
 * matcher fold back into one, so a machine that accumulated duplicates converges to a single entry.
 */
function mergeHookIntoSettings(settings: Settings, spec: HookMergeSpec): { changed: boolean; added: boolean } {
  const hooks: SettingsHooks =
    settings.hooks && typeof settings.hooks === "object" && !Array.isArray(settings.hooks)
      ? (settings.hooks as SettingsHooks)
      : {};
  settings.hooks = hooks;

  const groups = Array.isArray(hooks[spec.event]) ? hooks[spec.event]! : [];
  const specMatcher = normalizeMatcher(spec.matcher);
  const desiredTimeout = spec.timeoutSeconds ? spec.timeoutSeconds : undefined;

  // From later duplicates strip only OUR command, leaving the engineer's hooks in them untouched.
  let canonical: MatcherGroup | undefined;
  let changed = false;
  const nextGroups: MatcherGroup[] = [];
  for (const g of groups) {
    if (!(isMatcherGroup(g) && normalizeMatcher(g.matcher) === specMatcher)) {
      nextGroups.push(g);
      continue;
    }
    if (!canonical) {
      canonical = g;
      nextGroups.push(g);
      continue;
    }
    const remaining = g.hooks.filter((h) => h.command !== spec.command);
    if (remaining.length !== g.hooks.length) changed = true;
    if (remaining.length > 0) nextGroups.push({ ...g, hooks: remaining });
  }

  if (!canonical) {
    canonical = specMatcher === "" ? { hooks: [] } : { matcher: specMatcher, hooks: [] };
    nextGroups.push(canonical);
    changed = true;
  }
  hooks[spec.event] = nextGroups;

  const existing = canonical.hooks.find((h) => h.command === spec.command);
  if (existing) {
    if (existing.timeout !== desiredTimeout) {
      existing.timeout = desiredTimeout;
      changed = true;
    }
  } else {
    const entry: HookEntry = { type: "command", command: spec.command };
    if (desiredTimeout !== undefined) entry.timeout = desiredTimeout;
    canonical.hooks.push(entry);
    changed = true;
  }

  return { changed, added: existing === undefined };
}

/** Remove exactly the matching entry, dropping an emptied group or event array. */
function removeHookFromSettings(settings: Settings, spec: HookMergeSpec): boolean {
  const hooks = settings.hooks;
  if (!hooks || typeof hooks !== "object" || Array.isArray(hooks)) return false;
  const groups = hooks[spec.event];
  if (!Array.isArray(groups)) return false;

  let changed = false;
  const nextGroups: MatcherGroup[] = [];
  for (const group of groups) {
    if (!isMatcherGroup(group) || normalizeMatcher(group.matcher) !== normalizeMatcher(spec.matcher)) {
      nextGroups.push(group);
      continue;
    }
    const remaining = group.hooks.filter((h) => h.command !== spec.command);
    if (remaining.length !== group.hooks.length) changed = true;
    if (remaining.length > 0) nextGroups.push({ ...group, hooks: remaining });
  }

  if (!changed) return false;
  if (nextGroups.length > 0) hooks[spec.event] = nextGroups;
  else delete hooks[spec.event];
  return true;
}

/** Upsert `spec`; a no-op re-run reports `changed: false` and writes nothing. */
export function applyHookMerge(settingsPath: string, spec: HookMergeSpec, root: string = trustedParent(settingsPath)): HookMergeResult {
  const read = readSettings(settingsPath, root);
  if (!read.ok) return { ok: false, reason: read.reason };

  const { changed, added } = mergeHookIntoSettings(read.settings, spec);
  if (changed) {
    try {
      writeSettingsAtomic(settingsPath, read.settings, root);
    } catch (err) {
      return { ok: false, reason: `write settings.json: ${err instanceof Error ? err.message : String(err)}` };
    }
  }
  return { ok: true, changed, added };
}

/** Remove `spec`'s entry; a missing file or an already-absent entry is a no-op success. */
export function applyHookRemoval(settingsPath: string, spec: HookMergeSpec, root: string = trustedParent(settingsPath)): HookMergeResult {
  if (!anchoredPathExists(root, settingsPath)) return { ok: true, changed: false };
  const read = readSettings(settingsPath, root);
  if (!read.ok) return { ok: false, reason: read.reason };

  const changed = removeHookFromSettings(read.settings, spec);
  if (changed) {
    try {
      writeSettingsAtomic(settingsPath, read.settings, root);
    } catch (err) {
      return { ok: false, reason: `write settings.json: ${err instanceof Error ? err.message : String(err)}` };
    }
  }
  return { ok: true, changed };
}
