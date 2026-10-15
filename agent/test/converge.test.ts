import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync, readFileSync, existsSync, statSync, utimesSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import type { EngineContext } from "../src/engine/context.js";
import { sha256, testContext, recordPlaced } from "./helpers.js";
import {
  diskRetracting,
  emptyResponseFloorDecision,
  reconcileArtifactFiles,
  reconcileOneArtifact,
  reconcileRemovedArtifacts,
} from "../src/engine/converge.js";
import type { EnforcedArtifact } from "../src/contract.js";
import {
  buildEnforcementState,
  compositeKey,
  readLedgerEntry,
  recordReconcileFailure,
  recordReconcileSuccess,
  takePendingApplyFailures,
  writeEnforcementSnapshot,
  type SnapshotEntry,
} from "../src/engine/state.js";
import { applyHookMerge } from "../src/engine/hookMerge.js";

// Per-artifact reconcile scenarios driven directly through reconcileOneArtifact, with a real temp
// home: install, update, revert, the live-session guard, and failure reporting.

function artifact(overrides: Partial<EnforcedArtifact> = {}): EnforcedArtifact {
  return {
    artifactId: "a1",
    ideType: "IDE_TYPE_CLAUDE_CODE",
    type: "ARTIFACT_TYPE_CLAUDE_MD",
    name: "Org CLAUDE.md",
    scope: "ARTIFACT_SCOPE_USER_GLOBAL",
    repoFullName: "",
    installPath: ".claude/CLAUDE.md",
    primaryFileId: "f1",
    primaryHash: "",
    hookCommand: "",
    ...overrides,
  };
}

describe("reconcileOneArtifact — install / update / revert / guard / failure", () => {
  let dir: string;
  let home: string;
  let ctx: EngineContext;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "h-reconcile-"));
    home = join(dir, "home");
    mkdirSync(home, { recursive: true });
    ctx = testContext(dir);
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  it("install: no local file, a verified body arrives → written, ledger records 'installed'", () => {
    const body = Buffer.from("# Org CLAUDE.md\nstandard\n");
    const a = artifact({ primaryHash: sha256(body.toString()) });

    reconcileOneArtifact(ctx, a, body);

    assert.equal(readFileSync(join(home, ".claude", "CLAUDE.md"), "utf8"), body.toString());
    const entry = readLedgerEntry(ctx, "a1");
    assert.equal(entry?.kind, "installed");
    assert.equal(entry?.appliedHash, a.primaryHash);
    assert.equal(entry?.previousPreview, null);
  });

  it("Codex skill: the .agents/ install path resolves against $HOME, not $CODEX_HOME", () => {
    // Codex skills use the cross-tool `.agents/skills` convention, which is HOME-rooted,
    // NOT the Codex config root. The write must land at $HOME/.agents/skills/… — where
    // the tool reads it — even when $CODEX_HOME points elsewhere; a Codex-root anchor
    // would double-nest under ~/.codex/.agents and never match the status join.
    const codexHome = join(dir, "codex-home");
    mkdirSync(codexHome, { recursive: true });
    ctx.paths.codexHome = codexHome;

    const body = Buffer.from("# codex skill\nreview carefully\n");
    const a = artifact({
      artifactId: "cx1",
      ideType: "IDE_TYPE_CODEX",
      type: "ARTIFACT_TYPE_SKILL",
      name: "Code Review",
      installPath: ".agents/skills/code-review/SKILL.md",
      primaryHash: sha256(body.toString()),
    });
    reconcileOneArtifact(ctx, a, body);

    assert.equal(readFileSync(join(home, ".agents", "skills", "code-review", "SKILL.md"), "utf8"), body.toString());
    // NOT under the Codex root — a getCodexHomePath()-anchored write would double-nest here.
    assert.throws(() => readFileSync(join(codexHome, ".agents", "skills", "code-review", "SKILL.md"), "utf8"));
    assert.equal(readLedgerEntry(ctx, "cx1")?.kind, "installed");
  });

  it("Codex CLAUDE.md: a user-global .claude/ path lands under $HOME, not nested in the Codex home", () => {
    const codexHome = join(dir, "codex-home");
    mkdirSync(codexHome, { recursive: true });
    ctx.paths.codexHome = codexHome;

    const body = Buffer.from("# org instructions\n");
    const a = artifact({
      artifactId: "cx-md",
      ideType: "IDE_TYPE_CODEX",
      type: "ARTIFACT_TYPE_CLAUDE_MD",
      installPath: ".claude/CLAUDE.md",
      primaryHash: sha256(body.toString()),
    });
    reconcileOneArtifact(ctx, a, body);

    assert.equal(readFileSync(join(home, ".claude", "CLAUDE.md"), "utf8"), body.toString());
    assert.equal(existsSync(join(codexHome, ".claude", "CLAUDE.md")), false, "not under the Codex home");
  });

  it("Codex hook: a user-global hook script lands under the Codex home", () => {
    const codexHome = join(dir, "codex-home");
    mkdirSync(codexHome, { recursive: true });
    ctx.paths.codexHome = codexHome;

    const script = Buffer.from("#!/bin/sh\necho codex\n");
    const a = artifact({
      artifactId: "cx-hook",
      ideType: "IDE_TYPE_CODEX",
      type: "ARTIFACT_TYPE_HOOK",
      installPath: "hooks/guard.sh",
      primaryHash: sha256(script.toString()),
      hookConfig: { event: "PreToolUse", matcher: "Bash", timeoutSeconds: 0 },
      hookCommand: '"${CODEX_HOME:-$HOME/.codex}"/hooks/guard.sh',
    });
    reconcileOneArtifact(ctx, a, script);

    assert.equal(readFileSync(join(codexHome, "hooks", "guard.sh"), "utf8"), script.toString());
    const hooks = JSON.parse(readFileSync(join(codexHome, "hooks.json"), "utf8"));
    assert.equal(hooks.hooks.PreToolUse[0].hooks[0].command, a.hookCommand, "command written verbatim");
  });

  it("update: local file matches our own last write, server pushed new content → 'updated', not 'reverted'", () => {
    const claudeDir = join(home, ".claude");
    mkdirSync(claudeDir, { recursive: true });
    const v1 = "v1 body";
    writeFileSync(join(claudeDir, "CLAUDE.md"), v1);

    recordReconcileSuccess(ctx, "a1", {
      name: "Org CLAUDE.md", ideType: "IDE_TYPE_CLAUDE_CODE", installPath: ".claude/CLAUDE.md",
      appliedHash: sha256(v1), kind: "installed", previousPreview: null, newPreview: v1,
    });
    recordPlaced(ctx, artifact({ primaryHash: sha256(v1) }), { ".claude/CLAUDE.md": sha256(v1) });

    const v2 = Buffer.from("v2 body — admin updated the definition");
    const a = artifact({ primaryHash: sha256(v2.toString()) });
    reconcileOneArtifact(ctx, a, v2);

    assert.equal(readFileSync(join(claudeDir, "CLAUDE.md"), "utf8"), v2.toString());
    const entry = readLedgerEntry(ctx, "a1");
    assert.equal(entry?.kind, "updated");
    assert.equal(entry?.previousPreview, v1);
  });

  it("revert: local file was locally edited (does not match our last write) → 'reverted'", () => {
    const claudeDir = join(home, ".claude");
    mkdirSync(claudeDir, { recursive: true });
    const ours = "org-standard body";
    writeFileSync(join(claudeDir, "CLAUDE.md"), ours);

    recordReconcileSuccess(ctx, "a1", {
      name: "Org CLAUDE.md", ideType: "IDE_TYPE_CLAUDE_CODE", installPath: ".claude/CLAUDE.md",
      appliedHash: sha256(ours), kind: "installed", previousPreview: null, newPreview: ours,
    });
    recordPlaced(ctx, artifact({ primaryHash: sha256(ours) }), { ".claude/CLAUDE.md": sha256(ours) });

    // Engineer edits the file locally — it no longer matches our applied hash.
    writeFileSync(join(claudeDir, "CLAUDE.md"), "an engineer's local edit");

    const body = Buffer.from(ours); // the server still enforces the original content
    const a = artifact({ primaryHash: sha256(ours) });
    reconcileOneArtifact(ctx, a, body);

    assert.equal(readFileSync(join(claudeDir, "CLAUDE.md"), "utf8"), ours);
    const entry = readLedgerEntry(ctx, "a1");
    assert.equal(entry?.kind, "reverted");
    assert.equal(entry?.previousPreview, "an engineer's local edit");
  });

  // Point hasLiveSession("IDE_TYPE_CLAUDE_CODE") at a session file written moments
  // ago so the guard sees a live session for the duration of the test.
  function simulateLiveSession(): void {
    const projectsPath = join(dir, "claude-projects");
    const projectDir = join(projectsPath, "-Users-eng-repo");
    mkdirSync(projectDir, { recursive: true });
    const sessionFile = join(projectDir, "session-1.jsonl");
    writeFileSync(sessionFile, "{}\n");
    const now = new Date();
    utimesSync(sessionFile, now, now);
    ctx.paths.claudeProjects = projectsPath;
  }

  it("guard active — EDIT: a live session defers overwriting an existing file (retries next cycle)", () => {
    // A file this agent placed, now on disk at a different hash, is an EDIT: writing it would swap
    // the file out from under the running agent, so the guard defers it.
    const claudeDir = join(home, ".claude");
    mkdirSync(claudeDir, { recursive: true });
    writeFileSync(join(claudeDir, "CLAUDE.md"), "old on-disk body");
    recordPlaced(ctx, artifact({ primaryHash: sha256("old on-disk body") }), { ".claude/CLAUDE.md": sha256("old on-disk body") });
    simulateLiveSession();

    const body = Buffer.from("would-be-updated content");
    const a = artifact({ primaryHash: sha256(body.toString()) });
    reconcileOneArtifact(ctx, a, body);

    // Untouched — old body still on disk, no ledger entry, no failure. A pure skip.
    assert.equal(readFileSync(join(claudeDir, "CLAUDE.md"), "utf8"), "old on-disk body");
    assert.equal(readLedgerEntry(ctx, "a1"), null);
    assert.deepEqual(takePendingApplyFailures(ctx), []);
  });

  it("guard carve-out — FRESH install: a live session does NOT defer a brand-new file", () => {
    // Nothing on disk yet → the write only CREATES, overwriting nothing, so it cannot
    // disturb the live session. The carve-out lets it land immediately.
    simulateLiveSession();

    const body = Buffer.from("brand-new skill body");
    const a = artifact({
      artifactId: "fresh1",
      type: "ARTIFACT_TYPE_SKILL",
      installPath: ".claude/skills/new-skill/SKILL.md",
      primaryHash: sha256(body.toString()),
    });
    reconcileOneArtifact(ctx, a, body);

    assert.equal(readFileSync(join(home, ".claude", "skills", "new-skill", "SKILL.md"), "utf8"), body.toString());
    assert.equal(readLedgerEntry(ctx, "fresh1")?.kind, "installed");
  });

  it("guard — FRESH hook is excluded from the carve-out: its co-owned activation edit still waits for idle", () => {
    // A fresh hook's script is a new file, but activateHookForArtifact merges into a
    // co-owned settings.json — an edit of an existing file the live session may read —
    // so a fresh hook stays fully gated.
    simulateLiveSession();

    const body = Buffer.from("#!/usr/bin/env bash\necho guard\n");
    const a = artifact({
      artifactId: "hook1",
      type: "ARTIFACT_TYPE_HOOK",
      installPath: ".claude/hooks/guard.sh",
      primaryHash: sha256(body.toString()),
      hookConfig: { event: "PreToolUse", matcher: "Bash", timeoutSeconds: 120 },
      hookCommand: ".claude/hooks/guard.sh",
    });
    reconcileOneArtifact(ctx, a, body);

    // Neither the script nor an activation entry landed — deferred to a quiet cycle.
    assert.throws(() => readFileSync(join(home, ".claude", "hooks", "guard.sh"), "utf8"));
    assert.throws(() => readFileSync(join(home, ".claude", "settings.json"), "utf8"));
    assert.equal(readLedgerEntry(ctx, "hook1"), null);
    assert.deepEqual(takePendingApplyFailures(ctx), []);
  });

  it("guard — FRESH Kiro-style hook (HOOK type, empty hookCommand) is excluded by type, not activation", () => {
    // A Kiro hook is HOOK-typed but carries no hookCommand — the self-contained
    // .kiro.hook file IS the artifact, auto-discovered by the workspace. isHookArtifact is
    // false for it, so the carve-out must exclude it by TYPE: landing the file activates a
    // hook in the live workspace, so it waits for idle like any other hook.
    // Make hasLiveSession("IDE_TYPE_KIRO") see a recently-touched state store. The override
    // is the global-storage DIR; the probe reads state.vscdb inside it.
    const kiroDir = join(dir, "kiro-storage");
    mkdirSync(kiroDir, { recursive: true });
    const kiroDb = join(kiroDir, "state.vscdb");
    writeFileSync(kiroDb, "x");
    const now = new Date();
    utimesSync(kiroDb, now, now);
    ctx.paths.kiroStateDb = kiroDb;

    const body = Buffer.from('{"when":{"type":"fileEdited"},"then":{"type":"askAgent"}}\n');
    const a = artifact({
      artifactId: "kiro-hook1",
      ideType: "IDE_TYPE_KIRO",
      type: "ARTIFACT_TYPE_HOOK",
      installPath: ".kiro/hooks/review.kiro.hook",
      primaryHash: sha256(body.toString()),
      // no hookConfig, empty hookCommand — exactly how the backend sends a Kiro hook.
    });
    reconcileOneArtifact(ctx, a, body);

    assert.throws(() => readFileSync(join(home, ".kiro", "hooks", "review.kiro.hook"), "utf8"));
    assert.equal(readLedgerEntry(ctx, "kiro-hook1"), null);
    assert.deepEqual(takePendingApplyFailures(ctx), []);
  });

  it("failure: write fails (install_path escapes root) → apply_failures populated, no file written", () => {
    const a = artifact({ installPath: "../../etc/passwd", primaryHash: sha256("x") });
    reconcileOneArtifact(ctx, a, Buffer.from("x"));

    const pending = takePendingApplyFailures(ctx);
    assert.equal(pending.length, 1);
    assert.equal(pending[0]!.artifactId, "a1");
    assert.match(pending[0]!.reason, /escapes root/);
    assert.equal(readLedgerEntry(ctx, "a1"), null); // never recorded as a success
  });

  it("failure: no body, transient download failure → reason says 'download', not 'hash' (transient, not an integrity alert)", () => {
    const a = artifact({ primaryHash: sha256("expected-but-never-arrived") });
    reconcileOneArtifact(ctx, a, undefined, "download");

    const pending = takePendingApplyFailures(ctx);
    assert.equal(pending.length, 1);
    assert.match(pending[0]!.reason, /download failed/);
    assert.doesNotMatch(pending[0]!.reason, /hash/);
    assert.throws(() => readFileSync(join(home, ".claude", "CLAUDE.md"), "utf8"));
  });

  it("failure: no body, genuine hash mismatch → reason says 'hash' (the integrity signal)", () => {
    const a = artifact({ primaryHash: sha256("expected-but-never-arrived") });
    reconcileOneArtifact(ctx, a, undefined, "hash");

    const pending = takePendingApplyFailures(ctx);
    assert.equal(pending.length, 1);
    assert.match(pending[0]!.reason, /hash/);
    assert.throws(() => readFileSync(join(home, ".claude", "CLAUDE.md"), "utf8"));
  });

  it("failure: no body with no explicit kind defaults to a 'download' reason (safe, non-paging)", () => {
    const a = artifact({ primaryHash: sha256("expected-but-never-arrived") });
    reconcileOneArtifact(ctx, a, undefined);

    const pending = takePendingApplyFailures(ctx);
    assert.equal(pending.length, 1);
    assert.match(pending[0]!.reason, /download failed/);
    assert.doesNotMatch(pending[0]!.reason, /hash/);
  });

  it("hash already matches: a no-op that writes nothing and records nothing", () => {
    const claudeDir = join(home, ".claude");
    mkdirSync(claudeDir, { recursive: true });
    const content = "already converged";
    writeFileSync(join(claudeDir, "CLAUDE.md"), content);

    const a = artifact({ primaryHash: sha256(content) });
    reconcileOneArtifact(ctx, a, undefined); // no body needed — hashes already match

    assert.equal(readFileSync(join(claudeDir, "CLAUDE.md"), "utf8"), content);
    assert.equal(readLedgerEntry(ctx, "a1"), null);
  });

  it("hook artifact: a successful script write also structured-merges the settings.json entry", () => {
    const script = Buffer.from("#!/bin/sh\necho measured\n");
    const a = artifact({
      type: "ARTIFACT_TYPE_HOOK",
      installPath: ".claude/hooks/measure.sh",
      primaryHash: sha256(script.toString()),
      hookConfig: { event: "PreToolUse", matcher: "Bash", timeoutSeconds: 30 },
      hookCommand: '"$CLAUDE_PROJECT_DIR"/.claude/hooks/measure.sh',
    });

    reconcileOneArtifact(ctx, a, script);

    const scriptPath = join(home, ".claude", "hooks", "measure.sh");
    assert.equal(readFileSync(scriptPath, "utf8"), script.toString());
    // The placed hook script must be executable — the derived command runs it directly.
    assert.equal(statSync(scriptPath).mode & 0o111, 0o111);
    const settings = JSON.parse(readFileSync(join(home, ".claude", "settings.json"), "utf8"));
    assert.deepEqual(settings.hooks.PreToolUse, [
      { matcher: "Bash", hooks: [{ type: "command", command: a.hookCommand, timeout: 30 }] },
    ]);
  });

  it("inactive hook (write path): places the script but merges NO activation entry", () => {
    // Place-but-do-not-activate: the script lands but the settings.json entry is not merged,
    // so the hook stops firing while the file stays on disk.
    const script = Buffer.from("#!/bin/sh\necho dispatch\n");
    const a = artifact({
      type: "ARTIFACT_TYPE_HOOK",
      installPath: ".claude/hooks/measure.sh",
      primaryHash: sha256(script.toString()),
      hookConfig: { event: "UserPromptSubmit", matcher: "", timeoutSeconds: 0 },
      hookCommand: '"$CLAUDE_PROJECT_DIR"/.claude/hooks/measure.sh',
      inactive: true,
    });

    reconcileOneArtifact(ctx, a, script);

    // Script placed + executable, but settings.json was never created (no entry merged).
    const scriptPath = join(home, ".claude", "hooks", "measure.sh");
    assert.equal(readFileSync(scriptPath, "utf8"), script.toString());
    assert.equal(statSync(scriptPath).mode & 0o111, 0o111);
    assert.equal(existsSync(join(home, ".claude", "settings.json")), false, "no activation entry for an inactive hook");
    // The placement itself still converged — it is only the activation that is withheld.
    assert.equal(readLedgerEntry(ctx, "a1")?.appliedHash, a.primaryHash);
  });

  it("inactive hook (steady-state): a pre-existing activation entry is RETRACTED, script kept", () => {
    // A hook that was active (entry present, script already on disk) flips to inactive: the
    // steady-state re-assert must remove the entry an earlier active cycle merged.
    const script = Buffer.from("#!/bin/sh\necho dispatch\n");
    const scriptPath = join(home, ".claude", "hooks", "measure.sh");
    mkdirSync(join(home, ".claude", "hooks"), { recursive: true });
    writeFileSync(scriptPath, script); // already on disk at the enforced hash → changed == 0

    const spec = { event: "UserPromptSubmit", matcher: "", command: '"$CLAUDE_PROJECT_DIR"/.claude/hooks/measure.sh' };
    applyHookMerge(join(home, ".claude", "settings.json"), spec); // the prior active entry

    const a = artifact({
      type: "ARTIFACT_TYPE_HOOK",
      installPath: ".claude/hooks/measure.sh",
      primaryHash: sha256(script.toString()),
      hookConfig: { event: "UserPromptSubmit", matcher: "", timeoutSeconds: 0 },
      hookCommand: spec.command,
      inactive: true,
    });

    const bodies = new Map<string, Buffer>();
    const failures = new Map<string, "download" | "hash">();
    // priorHook is the proof the pass carries from the previous snapshot: this device placed this entry.
    reconcileArtifactFiles(ctx, a, bodies, failures, { markConvergedOnMatch: true, priorHook: spec });

    assert.equal(readFileSync(scriptPath, "utf8"), script.toString(), "script left in place");
    const settings = JSON.parse(readFileSync(join(home, ".claude", "settings.json"), "utf8"));
    assert.equal(settings.hooks?.UserPromptSubmit, undefined, "activation entry retracted");
  });

  it("inactive hook: an entry this device never recorded placing is LEFT ALONE", () => {
    // The removal is keyed on (event, matcher, command), which a team's own hand-written entry can
    // match exactly. Without the snapshot as proof we would delete a line they committed on purpose
    // — and re-delete it every cycle. No proof → leave it, even though that keeps the hook live.
    const script = Buffer.from("#!/bin/sh\necho dispatch\n");
    const scriptPath = join(home, ".claude", "hooks", "measure.sh");
    mkdirSync(join(home, ".claude", "hooks"), { recursive: true });
    writeFileSync(scriptPath, script);

    const command = '"$CLAUDE_PROJECT_DIR"/.claude/hooks/measure.sh';
    const settingsPath = join(home, ".claude", "settings.json");
    applyHookMerge(settingsPath, { event: "UserPromptSubmit", matcher: "", command }); // theirs, not ours

    const a = artifact({
      type: "ARTIFACT_TYPE_HOOK",
      installPath: ".claude/hooks/measure.sh",
      primaryHash: sha256(script.toString()),
      hookConfig: { event: "UserPromptSubmit", matcher: "", timeoutSeconds: 0 },
      hookCommand: command,
      inactive: true,
    });

    reconcileArtifactFiles(ctx, a, new Map(), new Map(), { markConvergedOnMatch: true, priorHook: null });

    const settings = JSON.parse(readFileSync(settingsPath, "utf8"));
    assert.equal(settings.hooks.UserPromptSubmit[0].hooks[0].command, command, "their entry survives");
  });

  it("inactive hook: a recorded entry for a DIFFERENT identity is not proof for this one", () => {
    // Proof must match the identity being removed, not merely exist — otherwise a stale record for
    // some other event/command would authorize deleting an unrelated line.
    const script = Buffer.from("#!/bin/sh\necho dispatch\n");
    mkdirSync(join(home, ".claude", "hooks"), { recursive: true });
    writeFileSync(join(home, ".claude", "hooks", "measure.sh"), script);

    const command = '"$CLAUDE_PROJECT_DIR"/.claude/hooks/measure.sh';
    const settingsPath = join(home, ".claude", "settings.json");
    applyHookMerge(settingsPath, { event: "UserPromptSubmit", matcher: "", command });

    const a = artifact({
      type: "ARTIFACT_TYPE_HOOK",
      installPath: ".claude/hooks/measure.sh",
      primaryHash: sha256(script.toString()),
      hookConfig: { event: "UserPromptSubmit", matcher: "", timeoutSeconds: 0 },
      hookCommand: command,
      inactive: true,
    });

    reconcileArtifactFiles(ctx, a, new Map(), new Map(), {
      markConvergedOnMatch: true,
      priorHook: { event: "PreToolUse", matcher: "Bash", command: ".claude/hooks/other.sh" },
    });

    const settings = JSON.parse(readFileSync(settingsPath, "utf8"));
    assert.equal(settings.hooks.UserPromptSubmit[0].hooks[0].command, command, "mismatched proof is no proof");
  });

  it("inactive hook (steady-state): a FAILED deactivation escalates an apply failure, not best-effort", () => {
    // The script is already on disk at the enforced hash (changed == 0), but a malformed activation
    // file makes deactivateHook fail, so a disabled hook stays active. Unlike an active re-assert
    // (best-effort, to avoid flapping a live hook), this is a real divergence and must be reported.
    const script = Buffer.from("#!/bin/sh\necho dispatch\n");
    const scriptPath = join(home, ".claude", "hooks", "measure.sh");
    mkdirSync(join(home, ".claude", "hooks"), { recursive: true });
    writeFileSync(scriptPath, script); // on disk at the enforced hash → changed == 0

    const settingsPath = join(home, ".claude", "settings.json");
    const malformed = "{ not valid json";
    writeFileSync(settingsPath, malformed); // deactivateHook returns ok:false

    const a = artifact({
      artifactId: "disp1",
      type: "ARTIFACT_TYPE_HOOK",
      installPath: ".claude/hooks/measure.sh",
      primaryHash: sha256(script.toString()),
      hookConfig: { event: "UserPromptSubmit", matcher: "", timeoutSeconds: 0 },
      hookCommand: '"$CLAUDE_PROJECT_DIR"/.claude/hooks/measure.sh',
      inactive: true,
    });

    reconcileArtifactFiles(ctx, a, new Map(), new Map(), {
      markConvergedOnMatch: true,
      priorHook: { event: "UserPromptSubmit", matcher: "", command: '"$CLAUDE_PROJECT_DIR"/.claude/hooks/measure.sh' },
    });

    assert.equal(readFileSync(settingsPath, "utf8"), malformed, "malformed settings left untouched");
    const queued = takePendingApplyFailures(ctx);
    assert.equal(queued.length, 1, "a deactivation failure queues an apply failure");
    assert.equal(queued[0]!.artifactId, "disp1");
  });
});

describe("reconcileRemovedArtifacts — retraction of de-served artifacts", () => {
  let dir: string;
  let home: string;
  let ctx: EngineContext;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "h-retract-"));
    home = join(dir, "home");
    mkdirSync(home, { recursive: true });
    ctx = testContext(dir);
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  function hookSnapshotEntry(overrides: Partial<SnapshotEntry> = {}): SnapshotEntry {
    return {
      artifactId: "h1",
      ideType: "IDE_TYPE_CLAUDE_CODE",
      type: "ARTIFACT_TYPE_HOOK",
      name: "measure hook",
      scope: "ARTIFACT_SCOPE_USER_GLOBAL",
      installPath: ".claude/hooks/measure.sh",
      primaryHash: "abc",
      isHook: true,
      hook: { event: "PreToolUse", matcher: "Bash", command: '"$CLAUDE_PROJECT_DIR"/.claude/hooks/measure.sh' },
      ...overrides,
    };
  }

  it("drops the settings.json entry for a hook no longer in the enforced set", () => {
    const settingsPath = join(home, ".claude", "settings.json");
    const spec = { event: "PreToolUse", matcher: "Bash", command: '"$CLAUDE_PROJECT_DIR"/.claude/hooks/measure.sh', timeoutSeconds: 30 };
    applyHookMerge(settingsPath, spec);
    writeEnforcementSnapshot(ctx, [hookSnapshotEntry()]);

    // The hook is gone from the current set → its settings entry is retracted.
    reconcileRemovedArtifacts(ctx, new Set<string>(), new Map());

    const settings = JSON.parse(readFileSync(settingsPath, "utf8"));
    assert.equal(settings.hooks?.PreToolUse, undefined);
  });

  it("leaves the settings entry in place for a hook still in the enforced set", () => {
    const settingsPath = join(home, ".claude", "settings.json");
    const spec = { event: "PreToolUse", matcher: "Bash", command: '"$CLAUDE_PROJECT_DIR"/.claude/hooks/measure.sh', timeoutSeconds: 30 };
    applyHookMerge(settingsPath, spec);
    writeEnforcementSnapshot(ctx, [hookSnapshotEntry()]);

    reconcileRemovedArtifacts(ctx, new Set(["h1"]), new Map());

    const settings = JSON.parse(readFileSync(settingsPath, "utf8"));
    assert.deepEqual(settings.hooks.PreToolUse, [
      { matcher: "Bash", hooks: [{ type: "command", command: spec.command, timeout: 30 }] },
    ]);
  });

  it("does not touch settings for a removed non-Claude-Code hook (never placed here)", () => {
    const settingsPath = join(home, ".claude", "settings.json");
    const spec = { event: "PreToolUse", matcher: "Bash", command: "other" };
    applyHookMerge(settingsPath, spec);
    writeEnforcementSnapshot(ctx, [hookSnapshotEntry({ ideType: "IDE_TYPE_CURSOR" })]);

    reconcileRemovedArtifacts(ctx, new Set<string>(), new Map());

    // The Claude settings entry is untouched — this hook's merge never landed here.
    const settings = JSON.parse(readFileSync(settingsPath, "utf8"));
    assert.equal(settings.hooks.PreToolUse.length, 1);
  });
});

describe("emptyResponseFloorDecision — one-shot empty-response floor", () => {
  it("defers the FIRST empty cycle when the previous snapshot had artifacts", () => {
    assert.deepEqual(emptyResponseFloorDecision(true, true, 0), { deferRetracts: true, nextEmptyStreak: 1 });
  });

  it("PROGRESSES (does not defer) on a confirmed second consecutive empty cycle", () => {
    // A legitimate "admin removed everything" must converge, not defer forever.
    assert.deepEqual(emptyResponseFloorDecision(true, true, 1), { deferRetracts: false, nextEmptyStreak: 2 });
    // And it stays progressed + clamped on further sustained-empty cycles.
    assert.deepEqual(emptyResponseFloorDecision(true, true, 2), { deferRetracts: false, nextEmptyStreak: 2 });
  });

  it("a non-empty read never defers and resets the streak to 0", () => {
    assert.deepEqual(emptyResponseFloorDecision(false, true, 1), { deferRetracts: false, nextEmptyStreak: 0 });
  });

  it("an empty read with no prior artifacts never defers (nothing to protect)", () => {
    assert.deepEqual(emptyResponseFloorDecision(true, false, 0), { deferRetracts: false, nextEmptyStreak: 1 });
  });
});

describe("diskRetracting — the floor also catches PARTIAL drops", () => {
  const hookEntry = (id: string): SnapshotEntry => ({
    artifactId: id, ideType: "IDE_TYPE_CLAUDE_CODE", type: "ARTIFACT_TYPE_HOOK",
    name: id, scope: "ARTIFACT_SCOPE_USER_GLOBAL", installPath: `.claude/hooks/${id}.sh`,
    primaryHash: "h", isHook: true, hook: { event: "PreToolUse", matcher: "Bash", command: `.claude/hooks/${id}.sh` },
  });
  const skill = (id: string): SnapshotEntry => ({
    artifactId: id, ideType: "IDE_TYPE_CLAUDE_CODE", type: "ARTIFACT_TYPE_SKILL",
    name: id, scope: "ARTIFACT_SCOPE_USER_GLOBAL", installPath: `.claude/skills/${id}/SKILL.md`,
    primaryHash: "h", isHook: false,
  });
  const key = (e: SnapshotEntry): string => compositeKey(e.artifactId, e.targetKey ?? "");

  it("false when every activation-bearing entry is still resolved this cycle", () => {
    const h = hookEntry("h1");
    assert.equal(diskRetracting([h], new Set([key(h)])), false);
  });

  it("false for an empty prior snapshot (nothing to retract)", () => {
    assert.equal(diskRetracting([], new Set<string>()), false);
  });

  it("true when a hook drops from the set (the full-empty case, subsumed)", () => {
    assert.equal(diskRetracting([hookEntry("h1")], new Set<string>()), true);
  });

  it("true on a PARTIAL drop — hook gone while a skill stays", () => {
    const h = hookEntry("h1");
    const s = skill("s1");
    assert.equal(diskRetracting([h, s], new Set([key(s)])), true);
  });

  it("true when a skill with placed files drops: its retract deletes them", () => {
    const h = hookEntry("h1");
    const s = skill("s1");
    assert.equal(diskRetracting([h, s], new Set([key(h)])), true);
  });

  it("false when the dropped entry placed nothing and co-owns no activation file", () => {
    const h = { ...hookEntry("h1"), ideType: "IDE_TYPE_KIRO", placedPaths: [] };
    const s = { ...skill("s1"), placedPaths: [] };
    assert.equal(diskRetracting([h, s], new Set<string>()), false);
  });
});

describe("repo-local retract on removal — discovery miss, orphaned checkout, floor", () => {
  let dir: string;
  let repo: string; // the REPO_LOCAL checkout root (== targetKey)
  let ctx: EngineContext;
  const COMMAND = '"$CLAUDE_PROJECT_DIR"/.claude/hooks/guard.sh';

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "h-repo-retract-"));
    repo = join(dir, "repo");
    mkdirSync(join(dir, "home"), { recursive: true });
    mkdirSync(repo, { recursive: true });
    mkdirSync(join(repo, ".git"), { recursive: true });
    ctx = testContext(dir);
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  function hookEntry(): SnapshotEntry {
    return {
      artifactId: "h-guard",
      ideType: "IDE_TYPE_CLAUDE_CODE",
      type: "ARTIFACT_TYPE_HOOK",
      name: "guard",
      scope: "ARTIFACT_SCOPE_REPO_LOCAL",
      installPath: ".claude/hooks/guard.sh",
      primaryHash: sha256("#!/bin/sh\n"),
      isHook: true,
      targetKey: repo,
      repoFullName: "example-org/example-repo",
      placedPaths: [".claude/hooks/guard.sh"],
      hook: { event: "PreToolUse", matcher: "Bash", command: COMMAND },
    };
  }

  function ruleEntry(): SnapshotEntry {
    return {
      artifactId: "r-guard",
      ideType: "IDE_TYPE_CLAUDE_CODE",
      type: "ARTIFACT_TYPE_RULE",
      name: "guard",
      scope: "ARTIFACT_SCOPE_REPO_LOCAL",
      installPath: ".claude/rules/guard.md",
      primaryHash: sha256("guard body"),
      isHook: false,
      targetKey: repo,
      repoFullName: "example-org/example-repo",
      placedPaths: [".claude/rules/guard.md"],
    };
  }

  const settingsPath = (): string => join(repo, ".claude", "settings.local.json");
  const placeActivation = (): void => {
    applyHookMerge(settingsPath(), { event: "PreToolUse", matcher: "Bash", command: COMMAND });
  };
  const stillEnforced = (id: string) =>
    new Map([[id, { scope: "ARTIFACT_SCOPE_REPO_LOCAL", repoFullName: "example-org/example-repo" }]]);

  it("a discovery miss on a still-valid checkout KEEPS its failure (a real divergence must survive)", () => {
    writeEnforcementSnapshot(ctx, [ruleEntry()]);
    // The checkout failed to converge, then dropped out of session-derived discovery this cycle;
    // its `.git` still marks it a real checkout.
    recordReconcileFailure(ctx, "r-guard", "write failed: EACCES", repo);

    const deferred = reconcileRemovedArtifacts(ctx, new Set<string>(), stillEnforced("r-guard"));

    assert.equal(deferred.length, 1, "still a checkout → deferred (carried forward), not retracted");
    assert.equal(
      buildEnforcementState(ctx).items[0]!.targets[0]!.failureReason,
      "write failed: EACCES",
      "a real failure is untouched while the checkout still exists",
    );
  });

  it("an orphaned dir (no .git) drops an artifact with nothing to tear down, clearing its stale failure", () => {
    writeEnforcementSnapshot(ctx, [ruleEntry()]);
    recordReconcileFailure(ctx, "r-guard", "some prior failure", repo);
    rmSync(join(repo, ".git"), { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });

    const deferred = reconcileRemovedArtifacts(ctx, new Set<string>(), stillEnforced("r-guard"));

    assert.equal(deferred.length, 0, "not a checkout anymore → tracking dropped, not carried forward");
    assert.equal(readLedgerEntry(ctx, "r-guard", repo), null, "ledger entry forgotten");
    assert.equal(buildEnforcementState(ctx).items[0]!.targets[0]!.failureReason, null, "the stale failure is cleared");
  });

  it("an orphaned dir (no .git) KEEPS a hook with a live activation entry so its teardown record survives", () => {
    placeActivation();
    writeEnforcementSnapshot(ctx, [hookEntry()]);
    recordReconcileFailure(ctx, "h-guard", "some prior failure", repo);
    rmSync(join(repo, ".git"), { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });

    const deferred = reconcileRemovedArtifacts(ctx, new Set<string>(), stillEnforced("h-guard"));

    assert.equal(deferred.length, 1, "a hook entry needs teardown → deferred (record kept), not forgotten");
    assert.equal(
      buildEnforcementState(ctx).items[0]!.targets[0]!.failureReason,
      "some prior failure",
      "not forgotten → its failure/record survives for a later teardown",
    );
    assert.equal(JSON.parse(readFileSync(settingsPath(), "utf8")).hooks.PreToolUse.length, 1, "entry untouched");
  });

  it("empty-response floor: an activation retract is DEFERRED, the entry left in place", () => {
    placeActivation();
    writeEnforcementSnapshot(ctx, [hookEntry()]);

    // deferRetracts=true models the pass's floor on an empty or partial read.
    const deferred = reconcileRemovedArtifacts(ctx, new Set<string>(), new Map(), true);

    assert.equal(deferred.length, 1, "the entry is carried forward for a later cycle");
    assert.equal(JSON.parse(readFileSync(settingsPath(), "utf8")).hooks.PreToolUse.length, 1, "activation kept");
    assert.notEqual(readLedgerEntry(ctx, "h-guard", repo), undefined);
  });

  it("empty-response floor: an entry with nothing to retract is still forgotten eagerly", () => {
    writeEnforcementSnapshot(ctx, [{ ...ruleEntry(), placedPaths: [] }]);
    recordReconcileFailure(ctx, "r-guard", "stale", repo);

    const deferred = reconcileRemovedArtifacts(ctx, new Set<string>(), new Map(), true);

    assert.equal(deferred.length, 0);
    assert.equal(buildEnforcementState(ctx).items[0]!.targets[0]!.failureReason, null);
  });

  it("once the floor progresses, the same removal retracts the activation entry", () => {
    placeActivation();
    writeEnforcementSnapshot(ctx, [hookEntry()]);

    reconcileRemovedArtifacts(ctx, new Set<string>(), new Map(), false);

    assert.equal(JSON.parse(readFileSync(settingsPath(), "utf8")).hooks?.PreToolUse, undefined, "entry retracted");
  });
});
