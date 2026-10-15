import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync, readFileSync, existsSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import type { EngineContext } from "../src/engine/context.js";
import { testContext } from "./helpers.js";
import {
  reconcileArtifactFiles,
  reconcileRemovedArtifacts,
  type EnforcedDesire,
} from "../src/engine/converge.js";
import { type ConvergenceTarget } from "../src/engine/artifacts.js";
import { type EnforcedArtifact } from "../src/contract.js";
import {
  buildEnforcementState,
  readEnforcementSnapshot,
  readLedgerEntry,
  recordReconcileSuccess,
  takePendingApplyFailures,
  writeEnforcementSnapshot,
  type SnapshotEntry,
} from "../src/engine/state.js";
import { applyCodexHookMerge } from "../src/engine/codexHookMerge.js";

function sha256(s: string): string {
  return createHash("sha256").update(s).digest("hex");
}

const CODEX_COMMAND = '"$(git rev-parse --show-toplevel)"/.codex/hooks/guard.sh';
const CODEX_SCRIPT = "#!/bin/sh\necho guard\n";
const CODEX_INSTALL_PATH = ".codex/hooks/guard.sh";

function codexHookArtifact(): EnforcedArtifact {
  return {
    artifactId: "rlh1",
    ideType: "IDE_TYPE_CODEX",
    type: "ARTIFACT_TYPE_HOOK",
    name: "Codex guard",
    scope: "ARTIFACT_SCOPE_REPO_LOCAL",
    repoFullName: "example-org/example-repo",
    installPath: CODEX_INSTALL_PATH,
    primaryFileId: "f1",
    primaryHash: sha256(CODEX_SCRIPT),
    hookConfig: { event: "PreToolUse", matcher: "Bash", timeoutSeconds: 120 },
    hookCommand: CODEX_COMMAND,
  };
}

const REPO = "example-org/example-repo";
const SCOPE_REPO_LOCAL = "ARTIFACT_SCOPE_REPO_LOCAL";
const SCOPE_USER_GLOBAL = "ARTIFACT_SCOPE_USER_GLOBAL";

function codexHookSnapshotEntry(checkout: string): SnapshotEntry {
  return {
    artifactId: "rlh1",
    ideType: "IDE_TYPE_CODEX",
    type: "ARTIFACT_TYPE_HOOK",
    name: "Codex guard",
    scope: SCOPE_REPO_LOCAL,
    targetKey: checkout,
    repoFullName: REPO,
    installPath: CODEX_INSTALL_PATH,
    primaryHash: sha256(CODEX_SCRIPT),
    isHook: true,
    hook: { event: "PreToolUse", matcher: "Bash", command: CODEX_COMMAND },
  };
}

// This cycle's device-read desire for the hook: still REPO_LOCAL for the same repo.
const stillRepoLocal = (): Map<string, EnforcedDesire> =>
  new Map([["rlh1", { scope: SCOPE_REPO_LOCAL, repoFullName: REPO }]]);

const repoTarget = (root: string): ConvergenceTarget => ({ targetKey: root, root });

describe("reconcileRemovedArtifacts — discovery churn must not retract a still-present checkout", () => {
  let dir: string;
  let home: string;
  let checkout: string;
  let ctx: EngineContext;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "h-selfheal-r-"));
    home = join(dir, "home");
    checkout = join(dir, "checkout");
    mkdirSync(home, { recursive: true });
    mkdirSync(checkout, { recursive: true });
    // The `.git` marks `checkout` a real checkout (a discovery miss defers it); its absence is how
    // reconcileRemovedArtifacts tells an orphaned dir from a genuinely-missed checkout.
    mkdirSync(join(checkout, ".git"), { recursive: true });
    ctx = testContext(dir);
  });
  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  function seedPlaced(): string {
    // A hook that was converged into the checkout last cycle: activation on disk + ledger.
    const hooksPath = join(checkout, ".codex", "hooks.json");
    applyCodexHookMerge(hooksPath, { event: "PreToolUse", matcher: "Bash", command: CODEX_COMMAND, timeoutSeconds: 120 });
    recordReconcileSuccess(ctx, "rlh1", {
      name: "Codex guard", ideType: "IDE_TYPE_CODEX", installPath: CODEX_INSTALL_PATH,
      appliedHash: sha256(CODEX_SCRIPT), kind: "installed", previousPreview: null, newPreview: CODEX_SCRIPT,
    }, checkout);
    writeEnforcementSnapshot(ctx, [codexHookSnapshotEntry(checkout)]);
    return hooksPath;
  }

  it("NOT retracted (and returned as deferred) when the artifact is still enforced for the same repo but the checkout wasn't discovered this cycle", () => {
    const hooksPath = seedPlaced();
    // currentKeys empty (checkout missed this cycle), but the artifact is STILL enforced
    // REPO_LOCAL for the SAME repo, and the checkout still exists → defer, touch nothing.
    const deferred = reconcileRemovedArtifacts(ctx, new Set<string>(), stillRepoLocal());

    const hooks = JSON.parse(readFileSync(hooksPath, "utf8"));
    assert.deepEqual(hooks.hooks.PreToolUse, [
      { matcher: "Bash", hooks: [{ type: "command", command: CODEX_COMMAND, timeout: 120 }] },
    ], "activation entry left intact");
    assert.notEqual(readLedgerEntry(ctx, "rlh1", checkout), null, "per-target ledger kept");
    // The deferred entry is returned so the pass can carry it forward into the snapshot.
    assert.equal(deferred.length, 1);
    assert.equal(deferred[0]!.targetKey, checkout);
  });

  it("IS retracted when the artifact left the enforced set (a real de-serve)", () => {
    const hooksPath = seedPlaced();
    const deferred = reconcileRemovedArtifacts(ctx, new Set<string>(), new Map()); // artifact gone

    const hooks = JSON.parse(readFileSync(hooksPath, "utf8"));
    assert.equal(hooks.hooks.PreToolUse, undefined, "activation retracted on de-serve");
    assert.equal(readLedgerEntry(ctx, "rlh1", checkout), null, "ledger forgotten");
    assert.deepEqual(deferred, [], "nothing deferred on a real removal");
  });

  it("IS retracted when the def MOVED to a DIFFERENT repo (same artifactId, new repoFullName)", () => {
    const hooksPath = seedPlaced();
    // Still served, still REPO_LOCAL, but now for a different repo → this checkout's target
    // is no longer desired → retract (must NOT defer just because the dir exists).
    const moved: Map<string, EnforcedDesire> = new Map([["rlh1", { scope: SCOPE_REPO_LOCAL, repoFullName: "example-org/other-repo" }]]);
    const deferred = reconcileRemovedArtifacts(ctx, new Set<string>(), moved);

    const hooks = JSON.parse(readFileSync(hooksPath, "utf8"));
    assert.equal(hooks.hooks.PreToolUse, undefined, "activation retracted on a repo move");
    assert.equal(readLedgerEntry(ctx, "rlh1", checkout), null, "ledger forgotten on a repo move");
    assert.deepEqual(deferred, []);
  });

  it("IS retracted when the def MOVED to USER_GLOBAL (no longer a repo-local target)", () => {
    const hooksPath = seedPlaced();
    const moved: Map<string, EnforcedDesire> = new Map([["rlh1", { scope: SCOPE_USER_GLOBAL, repoFullName: "" }]]);
    const deferred = reconcileRemovedArtifacts(ctx, new Set<string>(), moved);

    const hooks = JSON.parse(readFileSync(hooksPath, "utf8"));
    assert.equal(hooks.hooks.PreToolUse, undefined, "activation retracted when the scope moved to USER_GLOBAL");
    assert.equal(readLedgerEntry(ctx, "rlh1", checkout), null);
    assert.deepEqual(deferred, []);
  });

  it("IS retracted when the checkout root is gone from disk (even if still enforced for the same repo)", () => {
    seedPlaced();
    rmSync(checkout, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 }); // checkout deleted on disk
    const deferred = reconcileRemovedArtifacts(ctx, new Set<string>(), stillRepoLocal());

    // The checkout (and its activation file) are gone; the device stops managing the target.
    assert.equal(readLedgerEntry(ctx, "rlh1", checkout), null, "ledger forgotten for a vanished checkout");
    assert.deepEqual(deferred, []);
  });

  it("a deferred entry SURVIVES the snapshot rewrite, and a later real de-serve then retracts it", () => {
    const hooksPath = seedPlaced();

    // Cycle N+1: discovery miss → deferred. Simulate the pass's rewrite = resolved (none this
    // cycle) ∪ deferred. Without the carry-forward the entry would vanish here.
    const deferred = reconcileRemovedArtifacts(ctx, new Set<string>(), stillRepoLocal());
    writeEnforcementSnapshot(ctx, [...deferred]);
    assert.equal(readEnforcementSnapshot(ctx).length, 1, "deferred entry survived the rewrite");
    assert.notEqual(readLedgerEntry(ctx, "rlh1", checkout), null, "ledger still kept after the rewrite");
    assert.deepEqual(JSON.parse(readFileSync(hooksPath, "utf8")).hooks.PreToolUse.length, 1, "activation still present");

    // Cycle N+2: genuine de-serve → the carried-forward entry is now retracted.
    reconcileRemovedArtifacts(ctx, new Set<string>(), new Map());
    assert.equal(JSON.parse(readFileSync(hooksPath, "utf8")).hooks.PreToolUse, undefined, "retracted on the real de-serve");
    assert.equal(readLedgerEntry(ctx, "rlh1", checkout), null, "ledger forgotten on the real de-serve");
  });
});

describe("reconcileArtifactFiles — hook activation self-heals every cycle (script unchanged)", () => {
  let dir: string;
  let home: string;
  let checkout: string;
  let ctx: EngineContext;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "h-selfheal-a-"));
    home = join(dir, "home");
    checkout = join(dir, "checkout");
    mkdirSync(home, { recursive: true });
    mkdirSync(checkout, { recursive: true });
    ctx = testContext(dir);
  });
  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  it("re-adds a deleted activation entry when the script is still on disk at the enforced hash", () => {
    const a = codexHookArtifact();
    const target = repoTarget(checkout);
    const hooksPath = join(checkout, ".codex", "hooks.json");

    // Cycle 1: place the script + activation.
    reconcileArtifactFiles(ctx, a, new Map([["f1", Buffer.from(CODEX_SCRIPT)]]), new Map(), { markConvergedOnMatch: true }, target);
    assert.ok(existsSync(join(checkout, CODEX_INSTALL_PATH)), "script placed");
    assert.ok(existsSync(hooksPath), "activation placed");

    // Engineer deletes the activation entry but leaves the script intact (a bypass attempt).
    rmSync(hooksPath);

    // Cycle 2: the script still matches (changed === 0), no body supplied — activation must
    // still be re-asserted rather than silently skipped.
    reconcileArtifactFiles(ctx, a, new Map(), new Map(), { markConvergedOnMatch: true }, target);

    const hooks = JSON.parse(readFileSync(hooksPath, "utf8"));
    assert.deepEqual(hooks.hooks.PreToolUse, [
      { matcher: "Bash", hooks: [{ type: "command", command: CODEX_COMMAND, timeout: 120 }] },
    ], "deleted activation re-added on the unchanged-script cycle");
  });

  it("re-assert is idempotent when already correct — no failure recorded, file byte-identical", () => {
    const a = codexHookArtifact();
    const target = repoTarget(checkout);
    const hooksPath = join(checkout, ".codex", "hooks.json");

    reconcileArtifactFiles(ctx, a, new Map([["f1", Buffer.from(CODEX_SCRIPT)]]), new Map(), { markConvergedOnMatch: true }, target);
    takePendingApplyFailures(ctx); // clear anything from cycle 1
    const before = readFileSync(hooksPath, "utf8");

    // Cycle 2: everything already correct → self-heal is a pure no-op.
    reconcileArtifactFiles(ctx, a, new Map(), new Map(), { markConvergedOnMatch: true }, target);

    assert.equal(readFileSync(hooksPath, "utf8"), before, "no churn when already correct");
    assert.deepEqual(takePendingApplyFailures(ctx), [], "no spurious failure reported");
    assert.notEqual(readLedgerEntry(ctx, "rlh1", checkout), null, "still converged");
  });

  it("a persistently-malformed activation file does NOT flip a converged hook to failed or push apply_failures", () => {
    const a = codexHookArtifact();
    const target = repoTarget(checkout);
    const hooksPath = join(checkout, ".codex", "hooks.json");

    // Cycle 1: script + activation placed and converged; snapshot recorded (as the pass does).
    reconcileArtifactFiles(ctx, a, new Map([["f1", Buffer.from(CODEX_SCRIPT)]]), new Map(), { markConvergedOnMatch: true }, target);
    writeEnforcementSnapshot(ctx, [codexHookSnapshotEntry(checkout)]);
    takePendingApplyFailures(ctx); // clear cycle 1
    assert.equal(buildEnforcementState(ctx).items[0]!.status, "in_sync");

    // The co-owned activation file is corrupted by something unrelated to our hook.
    writeFileSync(hooksPath, "{ not valid json — a user's stray trailing comma,");

    // Two more steady-state cycles: script unchanged (changed === 0) so the best-effort
    // re-assert runs, hits the malformed file, and must NOT escalate.
    for (let cycle = 0; cycle < 2; cycle++) {
      reconcileArtifactFiles(ctx, a, new Map(), new Map(), { markConvergedOnMatch: true }, target);
      const item = buildEnforcementState(ctx).items[0]!;
      assert.equal(item.status, "in_sync", "converged hook stays in_sync despite the malformed file");
      assert.equal(item.failureReason, null, "no failure recorded");
      assert.equal(item.persistentFailure, false, "no failure streak accumulates");
      assert.deepEqual(takePendingApplyFailures(ctx), [], "no apply_failure pushed to the backend");
    }

    // The malformed file was left byte-untouched (readSettings refuses to write over it).
    assert.equal(readFileSync(hooksPath, "utf8"), "{ not valid json — a user's stray trailing comma,");
  });
});
