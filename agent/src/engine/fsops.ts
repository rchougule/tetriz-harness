import { lstatSync, realpathSync } from "node:fs";
import { anchoredOperation, trustedParent } from "./anchoredFs.js";
import { basename, dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import { IDE_CODEX, TYPE_HOOK } from "../contract.js";

export interface InstallRoots {
  home: string;
  codexHome: string;
}

/**
 * The root a user-global artifact's install path resolves against: home, except a Codex hook script,
 * whose path is relative to the Codex home. Keyed on type, not path, to match the server's derivation.
 */
export function installRootForIde(ideType: string, type: string, roots: InstallRoots): string {
  return ideType === IDE_CODEX && type === TYPE_HOOK ? roots.codexHome : roots.home;
}

/**
 * `installPath` resolved under `root`, or null if it would land outside it: absolute, a `..` escape, or
 * a symlink anywhere on the way (a parent directory or the file itself) that leads outside the root's
 * real path. A symlink that stays inside the root is followed. The server derives paths from validated
 * names; this is the agent's own last defence.
 */
export function resolveInstallPath(root: string, installPath: string): string | null {
  if (isAbsolute(installPath)) return null;
  const rootAbs = resolve(root);
  const abs = resolve(rootAbs, installPath);
  const rel = relative(rootAbs, abs);
  if (rel === "" || rel.startsWith("..") || isAbsolute(rel)) return null;
  if (!physicallyContained(rootAbs, abs)) return null;
  return abs;
}

function lexists(path: string): boolean {
  try {
    lstatSync(path);
    return true;
  } catch {
    return false;
  }
}

/**
 * `path` with symlinks resolved in every component that exists, and the missing rest appended as named.
 * Null when a component exists but cannot be resolved (a dangling or looping symlink, or no permission):
 * where it leads cannot be checked, so it is refused.
 */
function realPathOf(path: string): string | null {
  const tail: string[] = [];
  let cur = path;
  for (;;) {
    try {
      return join(realpathSync(cur), ...tail);
    } catch {
      if (lexists(cur)) return null;
      const parent = dirname(cur);
      if (parent === cur) return path;
      tail.unshift(basename(cur));
      cur = parent;
    }
  }
}

/** True when `abs`, followed through every symlink, still lands strictly inside `root` followed the same way. */
export function physicallyContained(root: string, abs: string): boolean {
  const rootReal = realPathOf(resolve(root));
  const real = realPathOf(abs);
  if (rootReal === null || real === null) return false;
  const rel = relative(rootReal, real);
  return rel !== "" && !rel.startsWith("..") && !isAbsolute(rel);
}

/**
 * Stage and rename through one anchored parent directory. The helper confines every operation even
 * when an untrusted descendant directory is swapped for a symlink during the write. Configured roots
 * and their ancestors are trusted; absolute symlinks below a root are refused by the helper.
 */
export function writeAtomic(path: string, content: Uint8Array, opts: { executable?: boolean; root?: string } = {}): string | undefined {
  const root = opts.root ?? trustedParent(path);
  const result = anchoredOperation("write", root, path, { data: content, executable: opts.executable });
  return result.created === undefined ? undefined : resolve(root, result.created);
}

/** Root-relative directories from `firstCreated` down to the parent of `filePath`. */
export function createdDirsFor(root: string, firstCreated: string | undefined, filePath: string): string[] {
  if (firstCreated === undefined) return [];
  const parent = relative(resolve(root), dirname(filePath));
  if (parent === "" || parent.startsWith("..") || isAbsolute(parent)) return [];
  const segments = parent.split(sep);
  const first = relative(resolve(root), firstCreated);
  // mkdir may have created the root itself (a fresh Codex home): then every level below is ours.
  const depth = first === "" || first.startsWith("..") ? 1 : first.split(sep).length;
  const out: string[] = [];
  for (let n = depth; n <= segments.length; n++) out.push(segments.slice(0, n).join("/"));
  return out;
}

/**
 * Remove whichever of `dirs` (root-relative, recorded as created by this agent) are now empty, deepest
 * first. A directory at or above `keepDepth` segments is the tool's own root and is never removed.
 */
export function removeEmptyCreatedDirs(root: string, dirs: readonly string[], keepDepth: number): void {
  const ordered = [...new Set(dirs)]
    .filter((d) => d.split("/").length > keepDepth)
    .sort((a, b) => b.split("/").length - a.split("/").length);
  for (const d of ordered) {
    const abs = resolveInstallPath(root, d);
    if (abs === null) continue;
    try {
      anchoredOperation("rmdir", root, abs);
    } catch {
      // Not empty (someone else's file), or already gone: either way it is not ours to remove.
    }
  }
}

/**
 * Delete one file this agent placed, by its root-relative path; true iff a file was removed. The
 * containment check refuses an escaping path, so a prune can only touch the tool's config root.
 */
export function removePlacedFile(root: string, installPath: string): boolean {
  const abs = resolveInstallPath(root, installPath);
  if (abs === null) return false;
  try {
    return anchoredOperation("remove", root, abs).removed === true;
  } catch {
    return false;
  }
}
