import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync, utimesSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import type { AgentPaths } from "../src/engine/context.js";
import { hasLiveSession } from "../src/engine/liveSession.js";
import { testContext } from "./helpers.js";

// hasLiveSession probes a per-tool session store's newest mtime; each test points the probes at
// fixture paths under a temp dir.

describe("hasLiveSession — recent-mtime heuristic", () => {
  let dir: string;
  let paths: AgentPaths;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "live-session-"));
    paths = testContext(dir).paths;
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  function touch(path: string, ageMs: number): void {
    const t = new Date(Date.now() - ageMs);
    utimesSync(path, t, t);
  }

  it("IDE_TYPE_CLAUDE_CODE: a session file written moments ago reads as live", () => {
    const projectsPath = join(dir, "claude-projects");
    const projectDir = join(projectsPath, "-Users-eng-repo");
    mkdirSync(projectDir, { recursive: true });
    const sessionFile = join(projectDir, "session-1.jsonl");
    writeFileSync(sessionFile, "{}\n");
    touch(sessionFile, 30_000); // 30s ago — well inside the window
    paths.claudeProjects = projectsPath;

    assert.equal(hasLiveSession(paths, "IDE_TYPE_CLAUDE_CODE"), true);
  });

  it("IDE_TYPE_CLAUDE_CODE: a session file quiet for a long time reads as not live", () => {
    const projectsPath = join(dir, "claude-projects");
    const projectDir = join(projectsPath, "-Users-eng-repo");
    mkdirSync(projectDir, { recursive: true });
    const sessionFile = join(projectDir, "session-1.jsonl");
    writeFileSync(sessionFile, "{}\n");
    touch(sessionFile, 60 * 60 * 1000); // 1h ago — well outside the window
    paths.claudeProjects = projectsPath;

    assert.equal(hasLiveSession(paths, "IDE_TYPE_CLAUDE_CODE"), false);
  });

  it("IDE_TYPE_CURSOR: a recently-touched state.vscdb reads as live", () => {
    const dbPath = join(dir, "state.vscdb");
    writeFileSync(dbPath, "");
    touch(dbPath, 10_000);
    paths.cursorDb = dbPath;

    assert.equal(hasLiveSession(paths, "IDE_TYPE_CURSOR"), true);
  });

  it("IDE_TYPE_CODEX: a rollout file under the Codex home sessions tree", () => {
    const codexHome = join(dir, "codex-home");
    const rolloutDir = join(codexHome, "sessions", "2026", "07", "30");
    mkdirSync(rolloutDir, { recursive: true });
    const rollout = join(rolloutDir, "rollout-abc.jsonl");
    writeFileSync(rollout, "{}\n");
    touch(rollout, 15_000);
    paths.codexHome = codexHome;

    assert.equal(hasLiveSession(paths, "IDE_TYPE_CODEX"), true);
  });

  it("fails OPEN (not live) when the probe path doesn't exist", () => {
    paths.claudeProjects = join(dir, "does-not-exist");
    assert.equal(hasLiveSession(paths, "IDE_TYPE_CLAUDE_CODE"), false);
  });

  it("fails OPEN (not live) for an unrecognized ide_type", () => {
    assert.equal(hasLiveSession(paths, "IDE_TYPE_UNSPECIFIED"), false);
  });
});
