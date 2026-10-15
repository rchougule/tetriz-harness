import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync, readFileSync, existsSync, utimesSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import type { EngineContext } from "../src/engine/context.js";
import { recordPlaced, testContext } from "./helpers.js";
import {
  reconcileArtifactFiles,
  pruneDroppedFiles,
  toSnapshotEntry,
} from "../src/engine/converge.js";
import { resolveArtifactFiles } from "../src/engine/artifacts.js";
import { type EnforcedArtifact } from "../src/contract.js";
import {
  writeEnforcementSnapshot,
  readLedgerEntry,
  takePendingApplyFailures,
  type SnapshotEntry,
} from "../src/engine/state.js";

// Multi-file (directory) artifact placement. testContext points the live-session probes at
// nonexistent paths so the guard fails open rather than deferring every write.

function sha256(s: string): string {
  return createHash("sha256").update(s).digest("hex");
}

const PRIMARY = "# skill body\n";
const HELPER = "print('helper')\n";
const PRIMARY_PATH = ".claude/skills/code-review/SKILL.md";
const HELPER_PATH = ".claude/skills/code-review/helper.py";

function skillArtifact(overrides: Partial<EnforcedArtifact> = {}): EnforcedArtifact {
  return {
    artifactId: "sk1",
    ideType: "IDE_TYPE_CLAUDE_CODE",
    type: "ARTIFACT_TYPE_SKILL",
    name: "Code Review",
    scope: "ARTIFACT_SCOPE_USER_GLOBAL",
    repoFullName: "",
    installPath: PRIMARY_PATH,
    primaryFileId: "f-primary",
    primaryHash: sha256(PRIMARY),
    hookCommand: "",
    files: [
      { relativePath: PRIMARY_PATH, fileId: "f-primary", hash: sha256(PRIMARY), executable: false },
      { relativePath: HELPER_PATH, fileId: "f-helper", hash: sha256(HELPER), executable: false },
    ],
    ...overrides,
  };
}

describe("multi-file directory artifact placement", () => {
  let dir: string;
  let home: string;
  let ctx: EngineContext;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "h-multifile-"));
    home = join(dir, "home");
    mkdirSync(home, { recursive: true });
    ctx = testContext(dir);
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  it("place-set: the whole file set (SKILL.md + helper) lands, sibling helper beside the primary", () => {
    const a = skillArtifact();
    const bodies = new Map([
      ["f-primary", Buffer.from(PRIMARY)],
      ["f-helper", Buffer.from(HELPER)],
    ]);
    reconcileArtifactFiles(ctx, a, bodies, new Map(), { markConvergedOnMatch: true });

    assert.equal(readFileSync(join(home, PRIMARY_PATH), "utf8"), PRIMARY);
    assert.equal(readFileSync(join(home, HELPER_PATH), "utf8"), HELPER);
  });

  it("place-set: only the differing helper is fetched/written; the already-matching primary is left", () => {
    // Primary already on disk and matching; only the helper differs.
    mkdirSync(join(home, ".claude", "skills", "code-review"), { recursive: true });
    writeFileSync(join(home, PRIMARY_PATH), PRIMARY);

    const a = skillArtifact();
    // Only the helper body is provided — the primary is unchanged so no body needed.
    const bodies = new Map([["f-helper", Buffer.from(HELPER)]]);
    reconcileArtifactFiles(ctx, a, bodies, new Map(), { markConvergedOnMatch: true });

    assert.equal(readFileSync(join(home, HELPER_PATH), "utf8"), HELPER);
    // Primary untouched (still the same bytes).
    assert.equal(readFileSync(join(home, PRIMARY_PATH), "utf8"), PRIMARY);
  });

  it("guard: a MIXED new+edit set stays fully gated during a live session (set-atomicity)", () => {
    // Helper already on disk at a DIFFERENT hash (an edit); primary absent (a create).
    // The fresh-install carve-out must NOT fire for a mixed set — placing the new primary
    // while the helper edit defers would leave a half-installed skill referenced mid-session.
    mkdirSync(join(home, ".claude", "skills", "code-review"), { recursive: true });
    writeFileSync(join(home, HELPER_PATH), "print('OLD helper')\n"); // present, differs from enforced HELPER
    recordPlaced(ctx, skillArtifact(), { [HELPER_PATH]: sha256("print('OLD helper')\n") }); // an earlier pass wrote it

    // Make hasLiveSession("IDE_TYPE_CLAUDE_CODE") see a session file touched moments ago.
    const projectsPath = join(dir, "claude-projects");
    mkdirSync(join(projectsPath, "-Users-eng-repo"), { recursive: true });
    const sessionFile = join(projectsPath, "-Users-eng-repo", "session-1.jsonl");
    writeFileSync(sessionFile, "{}\n");
    const now = new Date();
    utimesSync(sessionFile, now, now);
    ctx.paths.claudeProjects = projectsPath;

    const a = skillArtifact();
    const bodies = new Map([
      ["f-primary", Buffer.from(PRIMARY)],
      ["f-helper", Buffer.from(HELPER)],
    ]);
    reconcileArtifactFiles(ctx, a, bodies, new Map(), { markConvergedOnMatch: true });

    // Deferred wholesale: the new primary is never placed, the helper's old bytes stand,
    // and nothing is recorded — a pure skip that retries next cycle.
    assert.equal(existsSync(join(home, PRIMARY_PATH)), false, "new primary not placed while a helper edit defers");
    assert.equal(readFileSync(join(home, HELPER_PATH), "utf8"), "print('OLD helper')\n", "helper edit deferred");
    assert.equal(readLedgerEntry(ctx, "sk1"), null);
    assert.deepEqual(takePendingApplyFailures(ctx), []);
  });

  it("guard: adding a NEW helper to an already-present skill stays gated during a live session", () => {
    // Primary already on disk and matching; only the helper is new (absent). This is NOT a
    // brand-new artifact — the carve-out keys on every RESOLVED file being absent, so it
    // must not fire, or convergence would add a helper into an active skill directory the
    // already-present primary may start using mid-session.
    mkdirSync(join(home, ".claude", "skills", "code-review"), { recursive: true });
    writeFileSync(join(home, PRIMARY_PATH), PRIMARY); // present + matching enforced

    const projectsPath = join(dir, "claude-projects");
    mkdirSync(join(projectsPath, "-Users-eng-repo"), { recursive: true });
    const sessionFile = join(projectsPath, "-Users-eng-repo", "session-1.jsonl");
    writeFileSync(sessionFile, "{}\n");
    const now = new Date();
    utimesSync(sessionFile, now, now);
    ctx.paths.claudeProjects = projectsPath;

    const a = skillArtifact();
    const bodies = new Map([["f-helper", Buffer.from(HELPER)]]); // only the helper differs
    reconcileArtifactFiles(ctx, a, bodies, new Map(), { markConvergedOnMatch: true });

    assert.equal(existsSync(join(home, HELPER_PATH)), false, "new helper not added to an active skill dir mid-session");
    assert.equal(readFileSync(join(home, PRIMARY_PATH), "utf8"), PRIMARY, "primary untouched");
    assert.equal(readLedgerEntry(ctx, "sk1"), null);
  });

  it("prune: a helper dropped from a still-enforced skill is deleted; primary + user file kept", () => {
    mkdirSync(join(home, ".claude", "skills", "code-review"), { recursive: true });
    writeFileSync(join(home, PRIMARY_PATH), PRIMARY);
    writeFileSync(join(home, HELPER_PATH), HELPER);
    const userFile = join(home, ".claude", "skills", "code-review", "user-notes.md");
    writeFileSync(userFile, "my own notes");

    // Previous snapshot recorded placing SKILL.md + helper.py (NOT the user file).
    const prevSnapshot: SnapshotEntry[] = [
      {
        artifactId: "sk1",
        ideType: "IDE_TYPE_CLAUDE_CODE",
        type: "ARTIFACT_TYPE_SKILL",
        name: "Code Review",
        scope: "ARTIFACT_SCOPE_USER_GLOBAL",
        installPath: PRIMARY_PATH,
        primaryHash: sha256(PRIMARY),
        isHook: false,
        placedPaths: [PRIMARY_PATH, HELPER_PATH],
        placedHashes: { [PRIMARY_PATH]: sha256(PRIMARY), [HELPER_PATH]: sha256(HELPER) },
      },
    ];
    writeEnforcementSnapshot(ctx, prevSnapshot);

    // Current set dropped the helper (only SKILL.md remains enforced).
    const current = skillArtifact({
      files: [{ relativePath: PRIMARY_PATH, fileId: "f-primary", hash: sha256(PRIMARY), executable: false }],
    });
    pruneDroppedFiles(ctx, [current]);

    assert.equal(existsSync(join(home, HELPER_PATH)), false, "dropped helper removed");
    assert.equal(existsSync(join(home, PRIMARY_PATH)), true, "primary kept");
    assert.equal(existsSync(userFile), true, "user-added file never touched (not a recorded path)");
  });

  it("placedPaths: only files this agent wrote are recorded (helper absent → excluded)", () => {
    // This agent wrote the primary; the helper was never written.
    const primaryOnly = skillArtifact({ files: [{ relativePath: PRIMARY_PATH, fileId: "f-primary", hash: sha256(PRIMARY), executable: false }] });
    reconcileArtifactFiles(ctx, primaryOnly, new Map([["f-primary", Buffer.from(PRIMARY)]]), new Map(), { markConvergedOnMatch: true });

    const entry = toSnapshotEntry(ctx, skillArtifact());

    // A path not on disk must never be recorded as device-placed — else a later
    // prune could delete a file the device never actually converged.
    assert.deepEqual(entry.placedPaths, [PRIMARY_PATH]);
    assert.equal(entry.presentPaths, undefined);
  });

  it("placedPaths: a file already at the enforced bytes that this agent never wrote is present, not placed", () => {
    mkdirSync(join(home, ".claude", "skills", "code-review"), { recursive: true });
    writeFileSync(join(home, PRIMARY_PATH), PRIMARY);
    writeFileSync(join(home, HELPER_PATH), "print('stale different helper')\n");

    const entry = toSnapshotEntry(ctx, skillArtifact());

    assert.deepEqual(entry.placedPaths, [], "matching bytes are not proof of placement");
    assert.deepEqual(entry.presentPaths, [PRIMARY_PATH], "reported as installed; the differing helper is neither");
  });

  it("placedPaths: files written by this agent are placed with the hash it wrote", () => {
    const a = skillArtifact();
    reconcileArtifactFiles(
      ctx,
      a,
      new Map([
        ["f-primary", Buffer.from(PRIMARY)],
        ["f-helper", Buffer.from(HELPER)],
      ]),
      new Map(),
      { markConvergedOnMatch: true },
    );

    const entry = toSnapshotEntry(ctx, a);

    assert.deepEqual(entry.placedPaths, [PRIMARY_PATH, HELPER_PATH]);
    assert.deepEqual(entry.placedHashes, { [PRIMARY_PATH]: sha256(PRIMARY), [HELPER_PATH]: sha256(HELPER) });
  });

  it("placedPaths: a deferred prune is carried forward — a dropped helper still on disk stays recorded", () => {
    // Cycle N placed SKILL.md + helper.py; both are on disk.
    mkdirSync(join(home, ".claude", "skills", "code-review"), { recursive: true });
    writeFileSync(join(home, PRIMARY_PATH), PRIMARY);
    writeFileSync(join(home, HELPER_PATH), HELPER);
    const prevPlaced = [PRIMARY_PATH, HELPER_PATH];

    // Current set dropped the helper (e.g. the live-session guard deferred its prune),
    // but the helper file still exists on disk.
    const dropped = skillArtifact({
      files: [{ relativePath: PRIMARY_PATH, fileId: "f-primary", hash: sha256(PRIMARY), executable: false }],
    });

    const entry = toSnapshotEntry(ctx, dropped, prevPlaced);

    // The dropped-but-still-present helper must survive the snapshot rewrite so the
    // NEXT prune can still remove it — otherwise the guard-deferred prune is lost forever.
    assert.deepEqual(entry.placedPaths, [PRIMARY_PATH, HELPER_PATH]);

    // Once the deferred prune actually runs and the helper is gone from disk, it falls out.
    rmSync(join(home, HELPER_PATH));
    const afterPrune = toSnapshotEntry(ctx, dropped, prevPlaced);
    assert.deepEqual(afterPrune.placedPaths, [PRIMARY_PATH]);
  });

  it("placedPaths: a prevPlaced path NOT on disk is not carried forward (no resurrecting a user-deleted path)", () => {
    // Primary on disk; helper was in the previous snapshot but the user deleted it.
    mkdirSync(join(home, ".claude", "skills", "code-review"), { recursive: true });
    writeFileSync(join(home, PRIMARY_PATH), PRIMARY);
    const prevPlaced = [PRIMARY_PATH, HELPER_PATH]; // HELPER_PATH not on disk

    const dropped = skillArtifact({
      files: [{ relativePath: PRIMARY_PATH, fileId: "f-primary", hash: sha256(PRIMARY), executable: false }],
    });

    const entry = toSnapshotEntry(ctx, dropped, prevPlaced);

    // A path absent from disk is neither converged-desired nor a live deferred prune —
    // it must not be re-recorded (else a later prune could target a path the user removed).
    assert.deepEqual(entry.placedPaths, [PRIMARY_PATH]);
  });

  it("prune: a dropped helper is LEFT on disk while a session is live (never mutate a live tree)", () => {
    mkdirSync(join(home, ".claude", "skills", "code-review"), { recursive: true });
    writeFileSync(join(home, PRIMARY_PATH), PRIMARY);
    writeFileSync(join(home, HELPER_PATH), HELPER);

    // Make hasLiveSession("IDE_TYPE_CLAUDE_CODE") see a session file touched moments ago.
    const projectsPath = join(dir, "claude-projects");
    const projectDir = join(projectsPath, "-Users-eng-repo");
    mkdirSync(projectDir, { recursive: true });
    const sessionFile = join(projectDir, "session-1.jsonl");
    writeFileSync(sessionFile, "{}\n");
    const now = new Date();
    utimesSync(sessionFile, now, now);
    ctx.paths.claudeProjects = projectsPath;

    // Previous snapshot recorded placing SKILL.md + helper.py.
    writeEnforcementSnapshot(ctx, [
      {
        artifactId: "sk1",
        ideType: "IDE_TYPE_CLAUDE_CODE",
        type: "ARTIFACT_TYPE_SKILL",
        name: "Code Review",
        scope: "ARTIFACT_SCOPE_USER_GLOBAL",
        installPath: PRIMARY_PATH,
        primaryHash: sha256(PRIMARY),
        isHook: false,
        placedPaths: [PRIMARY_PATH, HELPER_PATH],
      },
    ]);

    // Current set dropped the helper — but a session is live for this ide_type.
    const current = skillArtifact({
      files: [{ relativePath: PRIMARY_PATH, fileId: "f-primary", hash: sha256(PRIMARY), executable: false }],
    });
    pruneDroppedFiles(ctx, [current]);

    // Contrast with the no-session prune test above: the live guard leaves it in place.
    assert.equal(existsSync(join(home, HELPER_PATH)), true, "dropped helper left on disk during a live session");
    assert.equal(existsSync(join(home, PRIMARY_PATH)), true, "primary kept");
  });

  it("prune: a whole-artifact removal leaves its files in place (handled elsewhere)", () => {
    mkdirSync(join(home, ".claude", "skills", "code-review"), { recursive: true });
    writeFileSync(join(home, PRIMARY_PATH), PRIMARY);
    writeFileSync(join(home, HELPER_PATH), HELPER);

    writeEnforcementSnapshot(ctx, [
      {
        artifactId: "sk1",
        ideType: "IDE_TYPE_CLAUDE_CODE",
        type: "ARTIFACT_TYPE_SKILL",
        name: "Code Review",
        scope: "ARTIFACT_SCOPE_USER_GLOBAL",
        installPath: PRIMARY_PATH,
        primaryHash: sha256(PRIMARY),
        isHook: false,
        placedPaths: [PRIMARY_PATH, HELPER_PATH],
      },
    ]);

    // The artifact is gone from the current set entirely.
    pruneDroppedFiles(ctx, []);

    assert.equal(existsSync(join(home, PRIMARY_PATH)), true, "files left in place on whole-artifact removal");
    assert.equal(existsSync(join(home, HELPER_PATH)), true);
  });

  it("backward-compat: an old server (no files list) synthesizes a single primary from the scalars", () => {
    const a: EnforcedArtifact = {
      artifactId: "old1",
      ideType: "IDE_TYPE_CLAUDE_CODE",
      type: "ARTIFACT_TYPE_CLAUDE_MD",
      name: "Org CLAUDE.md",
      scope: "ARTIFACT_SCOPE_USER_GLOBAL",
      repoFullName: "",
      installPath: ".claude/CLAUDE.md",
      primaryFileId: "f1",
      primaryHash: sha256("body"),
      hookCommand: "",
      // no `files`
    };
    const files = resolveArtifactFiles(a);
    assert.equal(files.length, 1);
    assert.deepEqual(files[0], {
      installPath: ".claude/CLAUDE.md",
      fileId: "f1",
      hash: sha256("body"),
      executable: false,
      isPrimary: true,
    });
  });

  it("backward-compat: a hook's synthesized single file is executable", () => {
    const a: EnforcedArtifact = {
      artifactId: "hook1",
      ideType: "IDE_TYPE_CLAUDE_CODE",
      type: "ARTIFACT_TYPE_HOOK",
      name: "Guard",
      scope: "ARTIFACT_SCOPE_USER_GLOBAL",
      repoFullName: "",
      installPath: ".claude/hooks/guard.sh",
      primaryFileId: "f1",
      primaryHash: sha256("#!/bin/sh\n"),
      hookConfig: { event: "PreToolUse", matcher: "*", timeoutSeconds: 0 },
      hookCommand: '"$HOME"/.claude/hooks/guard.sh',
      // no `files`
    };
    const files = resolveArtifactFiles(a);
    assert.equal(files.length, 1);
    assert.equal(files[0]?.executable, true);
  });
});
