import { readdirSync, statSync } from "node:fs";
import { join } from "node:path";
import { codexSessionsPath, type AgentPaths } from "./context.js";

// Never swap a file out from under a running agent. An active session touches its own transcript or
// state store on essentially every turn, so a recent write there means "live".

// Too short risks a mid-turn swap; too long risks never converging on a busy machine.
const LIVE_SESSION_WINDOW_MS = 5 * 60 * 1000;

// Bounds the scan so a deep session tree (Codex's YYYY/MM/DD layout) keeps each pass cheap.
const MAX_SCAN_DEPTH = 4;
const MAX_SCAN_ENTRIES = 2000;

interface ScanBudget {
  remaining: number;
}

/** Newest mtime among matching files under `root`, or null when missing or nothing matched. */
function newestMtimeUnder(
  root: string,
  matches: (name: string) => boolean,
  depth: number = MAX_SCAN_DEPTH,
  budget: ScanBudget = { remaining: MAX_SCAN_ENTRIES },
): number | null {
  let entries;
  try {
    entries = readdirSync(root, { withFileTypes: true });
  } catch {
    return null;
  }

  let newest: number | null = null;
  for (const entry of entries) {
    if (budget.remaining-- <= 0) break;
    const full = join(root, entry.name);
    if (entry.isDirectory()) {
      if (depth <= 0) continue;
      const sub = newestMtimeUnder(full, matches, depth - 1, budget);
      if (sub !== null && (newest === null || sub > newest)) newest = sub;
      continue;
    }
    if (!entry.isFile() || !matches(entry.name)) continue;
    try {
      const mtimeMs = statSync(full).mtimeMs;
      if (newest === null || mtimeMs > newest) newest = mtimeMs;
    } catch {
      // Removed between readdir and stat.
    }
  }
  return newest;
}

function fileMtime(path: string): number | null {
  try {
    return statSync(path).mtimeMs;
  } catch {
    return null;
  }
}

const isJsonl = (name: string): boolean => name.endsWith(".jsonl");
const isRollout = (name: string): boolean => name.startsWith("rollout-");

function probeMtime(paths: AgentPaths, ideType: string): number | null {
  switch (ideType) {
    case "IDE_TYPE_CLAUDE_CODE":
      return newestMtimeUnder(paths.claudeProjects, isJsonl);
    case "IDE_TYPE_CURSOR":
      return fileMtime(paths.cursorDb);
    case "IDE_TYPE_CODEX":
      return newestMtimeUnder(codexSessionsPath(paths), isRollout);
    case "IDE_TYPE_ANTIGRAVITY":
      return fileMtime(paths.antigravityDb);
    case "IDE_TYPE_KIRO":
      return fileMtime(paths.kiroStateDb);
    case "IDE_TYPE_COPILOT":
      return newestMtimeUnder(paths.copilotSessions, isJsonl);
    default:
      return null;
  }
}

/**
 * Whether `ideType` has a session live now. Fails open (not live) when the probe finds nothing, so a
 * probe gap can never wedge convergence; the cost is no protection for a store it cannot read.
 * Machine-wide per tool: over-broad for a repo-local target, but it only ever defers.
 */
export function hasLiveSession(paths: AgentPaths, ideType: string, nowMs: number = Date.now()): boolean {
  const newest = probeMtime(paths, ideType);
  if (newest === null) return false;
  return nowMs - newest < LIVE_SESSION_WINDOW_MS;
}
