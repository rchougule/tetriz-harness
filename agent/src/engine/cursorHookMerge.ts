import { anchoredPathExists, readAnchoredFile, trustedParent } from "./anchoredFs.js";
import { writeAtomic } from "./fsops.js";
import type { HookMergeResult, HookMergeSpec } from "./hookMerge.js";

// Cursor's hooks.json is flat — `{ version: 1, hooks: { <event>: [{ command, matcher? }] } }` — with
// identity (event, command). Same discipline as hookMerge: structured merge, atomic write, and a
// malformed file is reported and left byte-for-byte untouched.

interface CursorEntry {
  command: string;
  matcher?: string;
}

type CursorHooks = Record<string, CursorEntry[]>;

interface CursorHooksFile {
  version?: number;
  hooks?: CursorHooks;
  [key: string]: unknown;
}

function isCursorEntry(v: unknown): v is CursorEntry {
  return typeof v === "object" && v !== null && typeof (v as { command?: unknown }).command === "string";
}

function readHooksFile(
  path: string,
  root: string,
): { ok: true; file: CursorHooksFile; existed: boolean } | { ok: false; reason: string } {
  if (!anchoredPathExists(root, path)) return { ok: true, file: {}, existed: false };
  let raw: string;
  try {
    raw = readAnchoredFile(root, path).toString("utf8");
  } catch (err) {
    return { ok: false, reason: `read hooks.json: ${err instanceof Error ? err.message : String(err)}` };
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return { ok: false, reason: "hooks.json is not valid JSON" };
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    return { ok: false, reason: "hooks.json does not contain a JSON object" };
  }
  return { ok: true, file: parsed as CursorHooksFile, existed: true };
}

function writeHooksAtomic(path: string, file: CursorHooksFile, root: string): void {
  writeAtomic(path, Buffer.from(`${JSON.stringify(file, null, 2)}\n`), { root });
}

/** Upsert matched by command, so re-pointing a matcher updates the entry rather than duplicating it. */
function mergeFlatHook(file: CursorHooksFile, spec: HookMergeSpec): { changed: boolean; added: boolean } {
  const hooks: CursorHooks =
    file.hooks && typeof file.hooks === "object" && !Array.isArray(file.hooks) ? file.hooks : {};
  file.hooks = hooks;

  const entries = Array.isArray(hooks[spec.event]) ? hooks[spec.event]! : [];
  hooks[spec.event] = entries;

  const desiredMatcher = spec.matcher !== "" ? spec.matcher : undefined;
  const existing = entries.find((e) => isCursorEntry(e) && e.command === spec.command);
  if (existing) {
    if (existing.matcher === desiredMatcher) return { changed: false, added: false };
    if (desiredMatcher === undefined) delete existing.matcher;
    else existing.matcher = desiredMatcher;
    return { changed: true, added: false };
  }

  const entry: CursorEntry = { command: spec.command };
  if (desiredMatcher !== undefined) entry.matcher = desiredMatcher;
  entries.push(entry);
  return { changed: true, added: true };
}

function removeFlatHook(file: CursorHooksFile, spec: HookMergeSpec): boolean {
  const hooks = file.hooks;
  if (!hooks || typeof hooks !== "object" || Array.isArray(hooks)) return false;
  const entries = hooks[spec.event];
  if (!Array.isArray(entries)) return false;

  const remaining = entries.filter((e) => !(isCursorEntry(e) && e.command === spec.command));
  if (remaining.length === entries.length) return false;
  if (remaining.length > 0) hooks[spec.event] = remaining;
  else delete hooks[spec.event];
  return true;
}

export function applyCursorHookMerge(hooksJsonPath: string, spec: HookMergeSpec, root: string = trustedParent(hooksJsonPath)): HookMergeResult {
  const read = readHooksFile(hooksJsonPath, root);
  if (!read.ok) return { ok: false, reason: read.reason };

  // Set the schema version only when creating the file — never overwrite a co-owner's.
  if (!read.existed) read.file.version = 1;

  const { changed, added } = mergeFlatHook(read.file, spec);
  if (changed) {
    try {
      writeHooksAtomic(hooksJsonPath, read.file, root);
    } catch (err) {
      return { ok: false, reason: `write hooks.json: ${err instanceof Error ? err.message : String(err)}` };
    }
  }
  return { ok: true, changed, added };
}

export function applyCursorHookRemoval(hooksJsonPath: string, spec: HookMergeSpec, root: string = trustedParent(hooksJsonPath)): HookMergeResult {
  if (!anchoredPathExists(root, hooksJsonPath)) return { ok: true, changed: false };
  const read = readHooksFile(hooksJsonPath, root);
  if (!read.ok) return { ok: false, reason: read.reason };

  const changed = removeFlatHook(read.file, spec);
  if (changed) {
    try {
      writeHooksAtomic(hooksJsonPath, read.file, root);
    } catch (err) {
      return { ok: false, reason: `write hooks.json: ${err instanceof Error ? err.message : String(err)}` };
    }
  }
  return { ok: true, changed };
}
