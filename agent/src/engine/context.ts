import { homedir, platform } from "node:os";
import { join } from "node:path";
import type { Clock, Logger } from "../ports.js";
import type { AgentState } from "./state.js";

/**
 * Filesystem locations the engine reads and writes. `home` roots user-global artifacts,
 * `codexHome` roots Codex user-global hooks, and the session stores feed the live-session guard.
 */
export interface AgentPaths {
  home: string;
  codexHome: string;
  claudeProjects: string;
  /** Defaults to `<codexHome>/sessions` when unset. */
  codexSessions?: string;
  cursorDb: string;
  antigravityDb: string;
  kiroStateDb: string;
  copilotSessions: string;
}

export interface EngineContext {
  state: AgentState;
  log: Logger;
  now: Clock;
  paths: AgentPaths;
}

export const noopLogger: Logger = {
  debug: () => {},
  info: () => {},
  warn: () => {},
  error: () => {},
};

/** Per-platform location of a VS Code-family app's global state database. */
function vscodeFamilyStateDb(home: string, appDir: string): string {
  if (platform() === "darwin") return join(home, "Library", "Application Support", appDir, "User", "globalStorage", "state.vscdb");
  if (platform() === "win32") return join(home, "AppData", "Roaming", appDir, "User", "globalStorage", "state.vscdb");
  return join(home, ".config", appDir, "User", "globalStorage", "state.vscdb");
}

export function defaultPaths(overrides: Partial<AgentPaths> = {}): AgentPaths {
  const home = overrides.home ?? homedir();
  const codexHome = overrides.codexHome ?? (process.env["CODEX_HOME"] || join(home, ".codex"));
  return {
    home,
    codexHome,
    claudeProjects: overrides.claudeProjects ?? join(home, ".claude", "projects"),
    codexSessions: overrides.codexSessions,
    cursorDb: overrides.cursorDb ?? vscodeFamilyStateDb(home, "Cursor"),
    antigravityDb: overrides.antigravityDb ?? vscodeFamilyStateDb(home, "Antigravity"),
    kiroStateDb: overrides.kiroStateDb ?? vscodeFamilyStateDb(home, "kiro"),
    copilotSessions: overrides.copilotSessions ?? join(home, ".copilot", "session-state"),
  };
}

export function codexSessionsPath(paths: AgentPaths): string {
  return paths.codexSessions ?? join(paths.codexHome, "sessions");
}
