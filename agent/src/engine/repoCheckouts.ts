import { execFile } from "node:child_process";
import { existsSync, readFileSync, realpathSync } from "node:fs";
import { open, readdir, stat } from "node:fs/promises";
import type { Dirent } from "node:fs";
import { join } from "node:path";
import { promisify } from "node:util";
import type { Logger } from "../ports.js";
import { codexSessionsPath, noopLogger, type AgentPaths } from "./context.js";

// Repo-local targets: every local checkout (worktrees included) of the artifact's repo. Unlike an
// inventory scan, enforcement must land in every checkout, so nothing reduces a repo to one winner.

const execFileAsync = promisify(execFile);

const MAX_PROJECTS = 100;
const FIRST_CHUNK_BYTES = 64 * 1024;

// ── Git identity ────────────────────────────────────────────────────────────

/**
 * stdout on success; null when git ran and exited non-zero; undefined on a spawn-level failure
 * (binary missing, timeout). Injectable so tests can force the filesystem fallback.
 */
export type GitRunner = (cwd: string, args: string[]) => Promise<string | null | undefined>;

export const defaultGitRunner: GitRunner = async (cwd, args) => {
  try {
    const { stdout } = await execFileAsync("git", args, { cwd, timeout: 10_000, maxBuffer: 8 * 1024 * 1024 });
    return stdout;
  } catch (err) {
    return typeof (err as { code?: unknown }).code === "number" ? null : undefined;
  }
};

/** "org/repo" from a git remote URL (scp-style or URL form); the last two path segments. */
export function parseRepoName(remoteUrl: string): string | undefined {
  if (!remoteUrl) return undefined;
  let path: string | undefined;
  const sshColonMatch = remoteUrl.match(/^[^@]+@[^:]+:(.+)$/);
  if (sshColonMatch) path = sshColonMatch[1];
  if (!path) {
    try {
      path = new URL(remoteUrl).pathname.replace(/^\//, "");
    } catch {
      return undefined;
    }
  }
  if (!path) return undefined;
  path = path.replace(/\.git$/, "");
  const segments = path.split("/").filter(Boolean);
  if (segments.length < 2) return undefined;
  // Last two segments, so an enterprise host's extra path prefix still yields org/repo.
  return segments.slice(-2).join("/");
}

/** The cross-source comparison key: last two segments, lowercased. */
export function canonicalRepoKey(repo: string | undefined): string | undefined {
  if (!repo) return undefined;
  const segments = repo.split("/").filter(Boolean);
  if (segments.length < 2) return undefined;
  return segments.slice(-2).join("/").toLowerCase();
}

/** The "owner/repo" as reports carry it: lowercased, "" for none. The server folds case the same way. */
export function reportedRepository(repo: string | undefined): string {
  return canonicalRepoKey(repo) ?? "";
}

/** Nearest ancestor with a `.git` entry, as `git rev-parse --show-toplevel` would answer it. */
export function findGitToplevel(startPath: string): string | undefined {
  const segments = startPath.split("/").filter(Boolean);
  for (let depth = segments.length; depth > 0; depth--) {
    const candidate = "/" + segments.slice(0, depth).join("/");
    try {
      if (!existsSync(join(candidate, ".git"))) continue;
      try {
        return realpathSync(candidate);
      } catch {
        return candidate;
      }
    } catch {
      // Permission error: keep walking up.
    }
  }
  return undefined;
}

export function parseGitConfigRemoteUrl(configContent: string): string | undefined {
  let inOriginSection = false;
  for (const rawLine of configContent.split("\n")) {
    const line = rawLine.trim();
    if (line.startsWith("[")) {
      inOriginSection = line === '[remote "origin"]';
      continue;
    }
    if (inOriginSection && line.startsWith("url")) {
      const eqIdx = line.indexOf("=");
      if (eqIdx !== -1) return line.slice(eqIdx + 1).trim();
    }
  }
  return undefined;
}

function readGitRemoteOrigin(gitRoot: string): string | undefined {
  try {
    return parseGitConfigRemoteUrl(readFileSync(join(gitRoot, ".git", "config"), "utf-8"));
  } catch {
    return undefined;
  }
}

/** Drop userinfo so an `https://token@host/...` remote never travels further. */
export function stripRemoteCredentials(remote: string): string {
  try {
    const url = new URL(remote);
    url.username = "";
    url.password = "";
    return url.toString();
  } catch {
    return remote;
  }
}

export interface RepoIdentity {
  topLevel: string;
  remoteUrl: string;
}

/** Top-level and origin of the checkout containing `path`; falls back to the filesystem when git is unavailable. */
export async function readRepoIdentity(path: string, git: GitRunner = defaultGitRunner): Promise<RepoIdentity | null> {
  const top = await git(path, ["rev-parse", "--show-toplevel"]);
  const topLevel = top?.trim() || (findGitToplevel(path) ?? "");
  if (!topLevel) return null;
  const remoteGit = await git(topLevel, ["config", "--get", "remote.origin.url"]);
  const remoteRaw = remoteGit ?? readGitRemoteOrigin(topLevel) ?? "";
  return { topLevel, remoteUrl: stripRemoteCredentials(remoteRaw.trim()) };
}

// ── Project discovery (where the engineer actually runs agents) ────────────

async function safeReaddir(dirPath: string): Promise<Dirent[]> {
  try {
    return await readdir(dirPath, { withFileTypes: true });
  } catch {
    return [];
  }
}

async function isDirectory(dirPath: string): Promise<boolean> {
  try {
    return (await stat(dirPath)).isDirectory();
  } catch {
    return false;
  }
}

async function readHead(filePath: string): Promise<string | null> {
  let handle;
  try {
    handle = await open(filePath, "r");
    const buf = Buffer.alloc(FIRST_CHUNK_BYTES);
    const { bytesRead } = await handle.read(buf, 0, FIRST_CHUNK_BYTES, 0);
    return buf.subarray(0, bytesRead).toString("utf-8");
  } catch {
    return null;
  } finally {
    await handle?.close().catch(() => {});
  }
}

/** Distinct cwds from Claude Code session logs (`<projects>/<dir>/<session>.jsonl` carries `cwd`). */
export async function claudeProjectRoots(projectsDir: string, limit: number): Promise<string[]> {
  const roots = new Set<string>();
  for (const entry of await safeReaddir(projectsDir)) {
    if (roots.size >= limit) break;
    if (!entry.isDirectory() || entry.name.startsWith(".")) continue;
    const dir = join(projectsDir, entry.name);
    const files = (await safeReaddir(dir)).filter(
      (f) => f.isFile() && f.name.endsWith(".jsonl") && !f.name.startsWith("agent-"),
    );
    // Every session of a project carries the same cwd; take the first that parses.
    for (const f of files.slice(0, 3)) {
      const head = await readHead(join(dir, f.name));
      const cwd = head === null ? null : firstCwd(head);
      if (cwd && (await isDirectory(cwd))) {
        roots.add(cwd);
        break;
      }
    }
  }
  return Array.from(roots);
}

function firstCwd(head: string): string | null {
  for (const line of head.split("\n")) {
    if (!line.trim()) continue;
    try {
      const parsed = JSON.parse(line) as { cwd?: unknown };
      if (typeof parsed.cwd === "string" && parsed.cwd.startsWith("/")) return parsed.cwd;
    } catch {
      // A partial last line of the chunk.
    }
  }
  return null;
}

/** Distinct cwds from Codex rollout heads (`sessions/YYYY/MM/DD/rollout-*.jsonl`, first line). */
export async function codexProjectRoots(sessionsPath: string, limit: number): Promise<string[]> {
  const roots = new Set<string>();
  // Descending names keep the most recent sessions under the cap: dates and rollout names sort by time.
  const desc = (a: { name: string }, b: { name: string }): number => (a.name < b.name ? 1 : a.name > b.name ? -1 : 0);
  for (const year of (await safeReaddir(sessionsPath)).sort(desc)) {
    if (roots.size >= limit) break;
    if (!year.isDirectory() || !/^\d{4}$/.test(year.name)) continue;
    const yearPath = join(sessionsPath, year.name);
    for (const month of (await safeReaddir(yearPath)).sort(desc)) {
      if (roots.size >= limit) break;
      if (!month.isDirectory() || !/^\d{2}$/.test(month.name)) continue;
      const monthPath = join(yearPath, month.name);
      for (const day of (await safeReaddir(monthPath)).sort(desc)) {
        if (roots.size >= limit) break;
        if (!day.isDirectory() || !/^\d{2}$/.test(day.name)) continue;
        const dayPath = join(monthPath, day.name);
        for (const f of (await safeReaddir(dayPath)).sort(desc)) {
          if (roots.size >= limit) break;
          if (!f.isFile() || !f.name.startsWith("rollout-") || !f.name.endsWith(".jsonl")) continue;
          const cwd = rolloutCwd(await readHead(join(dayPath, f.name)));
          if (cwd && (await isDirectory(cwd))) roots.add(cwd);
        }
      }
    }
  }
  return Array.from(roots);
}

function rolloutCwd(head: string | null): string | null {
  const firstLine = head?.split("\n").find((l) => l.trim());
  if (!firstLine) return null;
  try {
    const parsed = JSON.parse(firstLine) as { type?: unknown; payload?: { cwd?: unknown } };
    if (parsed.type !== "session_meta") return null;
    const cwd = parsed.payload?.cwd;
    return typeof cwd === "string" && cwd.startsWith("/") ? cwd : null;
  } catch {
    return null;
  }
}

/**
 * Candidate project dirs: `extra` first, then session-derived roots interleaved round-robin so one
 * tool's long history cannot starve another's under the cap. A failing source contributes nothing.
 */
export async function discoverProjectRoots(
  paths: AgentPaths,
  extra: string[] = [],
  opts: { sessions?: boolean; log?: Logger } = {},
): Promise<string[]> {
  const log = opts.log ?? noopLogger;
  const sources: Array<{ id: string; read: () => Promise<string[]> }> =
    opts.sessions === false
      ? []
      : [
          { id: "claude-code", read: () => claudeProjectRoots(paths.claudeProjects, MAX_PROJECTS) },
          { id: "codex", read: () => codexProjectRoots(codexSessionsPath(paths), MAX_PROJECTS) },
        ];
  const lists = await Promise.all(
    sources.map(async (src) => {
      try {
        return await src.read();
      } catch (err) {
        log.warn("project_source_failed", { source: src.id, error: err instanceof Error ? err.message : String(err) });
        return [];
      }
    }),
  );

  const roots = new Set<string>(extra);
  const maxLen = lists.reduce((m, l) => Math.max(m, l.length), 0);
  for (let i = 0; i < maxLen && roots.size < MAX_PROJECTS + extra.length; i++) {
    for (const list of lists) {
      if (roots.size >= MAX_PROJECTS + extra.length) break;
      const p = list[i];
      if (p !== undefined) roots.add(p);
    }
  }
  return Array.from(roots).sort();
}

// ── Repo → checkouts ────────────────────────────────────────────────────────

/**
 * canonicalRepoKey(org/repo) → sorted, de-duplicated checkout top-levels. A candidate with no remote
 * or no top-level is skipped; two candidates under one checkout collapse to one entry. Built once per
 * pass (it spawns git per candidate), then read per artifact.
 */
export async function resolveRepoCheckouts(
  candidates: string[],
  opts: { git?: GitRunner } = {},
): Promise<Map<string, string[]>> {
  const byKey = new Map<string, Set<string>>();
  for (const path of candidates) {
    let id: RepoIdentity | null;
    try {
      id = await readRepoIdentity(path, opts.git);
    } catch {
      continue; // one unreadable candidate never blanks the rest
    }
    if (id === null || id.topLevel === "" || id.remoteUrl === "") continue;
    const key = canonicalRepoKey(parseRepoName(id.remoteUrl));
    if (key === undefined) continue;
    let set = byKey.get(key);
    if (set === undefined) {
      set = new Set();
      byKey.set(key, set);
    }
    set.add(id.topLevel);
  }
  const out = new Map<string, string[]>();
  for (const [key, set] of byKey) out.set(key, Array.from(set).sort());
  return out;
}

/**
 * Checkouts a repo-local artifact converges into, or [] when none exists here. The key is host-less
 * on both sides, so same-named repos on two hosts both match; the contract carries no host.
 */
export function checkoutsForRepo(repoCheckouts: Map<string, string[]>, repoFullName: string): string[] {
  const key = canonicalRepoKey(repoFullName);
  if (key === undefined) return [];
  return repoCheckouts.get(key) ?? [];
}
