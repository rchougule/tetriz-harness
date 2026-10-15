import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import type { EngineContext } from "../src/engine/context.js";
import { testContext } from "./helpers.js";
import {
  buildEnforcementState,
  classifyReconcileKind,
  markConverged,
  queueApplyFailure,
  recordReconcileFailure,
  recordReconcileSuccess,
  takePendingApplyFailures,
  writeEnforcementSnapshot,
  type SnapshotEntry,
} from "../src/engine/state.js";

function entry(overrides: Partial<SnapshotEntry> = {}): SnapshotEntry {
  return {
    artifactId: "a1",
    ideType: "IDE_TYPE_CLAUDE_CODE",
    type: "ARTIFACT_TYPE_CLAUDE_MD",
    name: "Org CLAUDE.md",
    scope: "ARTIFACT_SCOPE_USER_GLOBAL",
    installPath: ".claude/CLAUDE.md",
    primaryHash: "hash-v1",
    isHook: false,
    ...overrides,
  };
}

describe("state — ledger, failures, and the status view-model", () => {
  let dir: string;
  let ctx: EngineContext;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "h-enforcement-store-"));
    ctx = testContext(dir);
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  it("classifyReconcileKind: no prior file → installed", () => {
    assert.equal(classifyReconcileKind(ctx, "a1", null), "installed");
  });

  it("classifyReconcileKind: prior file matches our last applied hash → updated", () => {
    recordReconcileSuccess(ctx, "a1", {
      name: "n", ideType: "IDE_TYPE_CLAUDE_CODE", installPath: "p",
      appliedHash: "hash-v1", kind: "installed", previousPreview: null, newPreview: "v1 body",
    });
    assert.equal(classifyReconcileKind(ctx, "a1", "hash-v1"), "updated");
  });

  it("classifyReconcileKind: prior file diverges from our last applied hash → reverted", () => {
    recordReconcileSuccess(ctx, "a1", {
      name: "n", ideType: "IDE_TYPE_CLAUDE_CODE", installPath: "p",
      appliedHash: "hash-v1", kind: "installed", previousPreview: null, newPreview: "v1 body",
    });
    // The local file no longer matches what we last wrote — an engineer edit.
    assert.equal(classifyReconcileKind(ctx, "a1", "hash-locally-edited"), "reverted");
  });

  it("classifyReconcileKind: a pre-existing file we never converged reads as reverted, not installed", () => {
    // No ledger entry at all, but a file DID exist locally before this write.
    assert.equal(classifyReconcileKind(ctx, "never-seen", "some-hash"), "reverted");
  });

  it("buildEnforcementState: in_sync when the ledger's applied hash matches the current snapshot", () => {
    writeEnforcementSnapshot(ctx, [entry({ primaryHash: "hash-v1" })]);
    recordReconcileSuccess(ctx, "a1", {
      name: "Org CLAUDE.md", ideType: "IDE_TYPE_CLAUDE_CODE", installPath: ".claude/CLAUDE.md",
      appliedHash: "hash-v1", kind: "installed", previousPreview: null, newPreview: "v1 body",
    });

    const state = buildEnforcementState(ctx);
    assert.equal(state.items.length, 1);
    assert.equal(state.items[0]!.status, "in_sync");
    assert.equal(state.items[0]!.lastReconcileKind, "installed");
    assert.deepEqual(state.items[0]!.diff, { before: null, after: "v1 body", truncated: false });
  });

  it("buildEnforcementState: pending when nothing has been reconciled yet", () => {
    writeEnforcementSnapshot(ctx, [entry()]);
    const state = buildEnforcementState(ctx);
    assert.equal(state.items[0]!.status, "pending");
    assert.equal(state.items[0]!.diff, null);
  });

  it("buildEnforcementState: pending when the ledger's applied hash is stale (server pushed a new version)", () => {
    writeEnforcementSnapshot(ctx, [entry({ primaryHash: "hash-v2" })]);
    recordReconcileSuccess(ctx, "a1", {
      name: "Org CLAUDE.md", ideType: "IDE_TYPE_CLAUDE_CODE", installPath: ".claude/CLAUDE.md",
      appliedHash: "hash-v1", kind: "installed", previousPreview: null, newPreview: "v1 body",
    });
    assert.equal(buildEnforcementState(ctx).items[0]!.status, "pending");
  });

  it("buildEnforcementState: a persistent failure survives a re-render (re-read) and is flagged", () => {
    writeEnforcementSnapshot(ctx, [entry()]);
    recordReconcileFailure(ctx, "a1", "EACCES: permission denied");
    recordReconcileFailure(ctx, "a1", "EACCES: permission denied"); // second consecutive cycle

    const first = buildEnforcementState(ctx);
    assert.equal(first.items[0]!.status, "failed");
    assert.equal(first.items[0]!.persistentFailure, true);
    assert.equal(first.items[0]!.failureReason, "EACCES: permission denied");

    // Re-render: calling buildEnforcementState again (no new writes) must
    // return the identical persistent-failure state, not reset it.
    const second = buildEnforcementState(ctx);
    assert.deepEqual(second, first);
  });

  it("recordReconcileFailure reports whether the REASON changed, so a new condition is not suppressed", () => {
    // The caller reports to the backend once per distinct reason. A repeat must read false, and a
    // DIFFERENT reason arriving mid-streak must read true — else a refusal following an unrelated
    // failure would never be reported and the dashboard would keep the stale reason.
    writeEnforcementSnapshot(ctx, [entry()]);
    assert.equal(recordReconcileFailure(ctx, "a1", "download failed"), true, "first failure is a change");
    assert.equal(recordReconcileFailure(ctx, "a1", "download failed"), false, "same reason is a repeat");
    assert.equal(recordReconcileFailure(ctx, "a1", "git-tracked write target"), true, "new reason is a change");
    assert.equal(buildEnforcementState(ctx).items[0]!.failureReason, "git-tracked write target");
  });

  it("a single failure (not yet persistent) is still reported but not flagged persistent", () => {
    writeEnforcementSnapshot(ctx, [entry()]);
    recordReconcileFailure(ctx, "a1", "disk full");
    const state = buildEnforcementState(ctx);
    assert.equal(state.items[0]!.status, "failed");
    assert.equal(state.items[0]!.persistentFailure, false);
  });

  it("a successful reconcile clears a prior failure streak", () => {
    writeEnforcementSnapshot(ctx, [entry({ primaryHash: "hash-v1" })]);
    recordReconcileFailure(ctx, "a1", "transient blip");
    recordReconcileSuccess(ctx, "a1", {
      name: "Org CLAUDE.md", ideType: "IDE_TYPE_CLAUDE_CODE", installPath: ".claude/CLAUDE.md",
      appliedHash: "hash-v1", kind: "installed", previousPreview: null, newPreview: "v1 body",
    });
    const state = buildEnforcementState(ctx);
    assert.equal(state.items[0]!.status, "in_sync");
    assert.equal(state.items[0]!.failureReason, null);
  });

  it("markConverged: a file that already matches with no prior ledger entry becomes in_sync", () => {
    writeEnforcementSnapshot(ctx, [entry({ primaryHash: "hash-v1" })]);
    // No reconcile ever ran (engineer already had identical bytes) → pending.
    assert.equal(buildEnforcementState(ctx).items[0]!.status, "pending");

    markConverged(ctx, {
      artifactId: "a1", name: "Org CLAUDE.md", ideType: "IDE_TYPE_CLAUDE_CODE",
      installPath: ".claude/CLAUDE.md", primaryHash: "hash-v1",
    }, "v1 body");

    const state = buildEnforcementState(ctx);
    assert.equal(state.items[0]!.status, "in_sync");
    assert.equal(state.items[0]!.lastReconcileKind, "installed");
  });

  it("markConverged: clears a prior failure when the engineer manually fixed the file", () => {
    writeEnforcementSnapshot(ctx, [entry({ primaryHash: "hash-v1" })]);
    recordReconcileFailure(ctx, "a1", "write failed: EACCES");
    assert.equal(buildEnforcementState(ctx).items[0]!.status, "failed");

    markConverged(ctx, {
      artifactId: "a1", name: "Org CLAUDE.md", ideType: "IDE_TYPE_CLAUDE_CODE",
      installPath: ".claude/CLAUDE.md", primaryHash: "hash-v1",
    }, "v1 body");

    const state = buildEnforcementState(ctx);
    assert.equal(state.items[0]!.status, "in_sync");
    assert.equal(state.items[0]!.failureReason, null);
  });

  it("markConverged: idempotent — the reconciled time does not tick once already converged and clean", () => {
    writeEnforcementSnapshot(ctx, [entry({ primaryHash: "hash-v1" })]);
    markConverged(ctx, {
      artifactId: "a1", name: "Org CLAUDE.md", ideType: "IDE_TYPE_CLAUDE_CODE",
      installPath: ".claude/CLAUDE.md", primaryHash: "hash-v1",
    }, "v1 body");
    const first = buildEnforcementState(ctx).items[0]!.lastReconciledAt;

    markConverged(ctx, {
      artifactId: "a1", name: "Org CLAUDE.md", ideType: "IDE_TYPE_CLAUDE_CODE",
      installPath: ".claude/CLAUDE.md", primaryHash: "hash-v1",
    }, "v1 body");
    assert.equal(buildEnforcementState(ctx).items[0]!.lastReconciledAt, first);
  });

  it("takePendingApplyFailures reads and clears — a second call returns empty", () => {
    queueApplyFailure(ctx, { artifactId: "a1", repoFullName: "", primaryHash: "h1" }, "write failed: EACCES");
    queueApplyFailure(ctx, { artifactId: "a2", repoFullName: "example-org/example-repo", primaryHash: "h2" }, "hash mismatch");

    const first = takePendingApplyFailures(ctx);
    assert.deepEqual(first, [
      { artifactId: "a1", repository: "", contentHash: "h1", reason: "write failed: EACCES" },
      { artifactId: "a2", repository: "example-org/example-repo", contentHash: "h2", reason: "hash mismatch" },
    ]);

    const second = takePendingApplyFailures(ctx);
    assert.deepEqual(second, []);
  });
});

describe("buildEnforcementState — per-target placement", () => {
  let tmpDir: string;
  let ctx: EngineContext;

  beforeEach(() => {
    tmpDir = mkdtempSync(join(tmpdir(), "harness-enf-targets-"));
    ctx = testContext(tmpDir);
  });
  afterEach(() => rmSync(tmpDir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 }));

  it("keeps every checkout an artifact was placed into, not just the representative", () => {
    // The same artifactId in two worktrees of one repo, plus a third in another repo.
    writeEnforcementSnapshot(ctx, [
      entry({
        scope: "ARTIFACT_SCOPE_REPO_LOCAL",
        targetKey: "/Users/x/core",
        repoFullName: "example-org/example-repo",
        installPath: ".claude/hooks/h.sh",
        placedPaths: [".claude/hooks/h.sh"],
      }),
      entry({
        scope: "ARTIFACT_SCOPE_REPO_LOCAL",
        targetKey: "/Users/x/core-wt",
        repoFullName: "example-org/example-repo",
        installPath: ".claude/hooks/h.sh",
        placedPaths: [".claude/hooks/h.sh"],
      }),
    ]);

    const [item] = buildEnforcementState(ctx).items;
    assert.ok(item);
    assert.equal(item.targets.length, 2, "both checkouts must survive the artifactId grouping");
    assert.deepEqual(
      item.targets.map((t) => t.root),
      ["/Users/x/core", "/Users/x/core-wt"],
      "each worktree keeps its own root — this is what the collapsed installPath cannot show",
    );
    for (const t of item.targets) assert.equal(t.repoFullName, "example-org/example-repo");
  });

  it("marks the machine-wide target with a null root rather than an empty string", () => {
    // targetKey "" is the USER_GLOBAL sentinel; rendering it verbatim would show a blank path.
    writeEnforcementSnapshot(ctx, [entry()]);
    const [item] = buildEnforcementState(ctx).items;
    assert.ok(item);
    assert.equal(item.targets.length, 1);
    assert.equal(item.targets[0]!.root, null);
    assert.equal(item.targets[0]!.repoFullName, null);
  });

  it("falls back to installPath when a pre-multi-file snapshot has no placedPaths", () => {
    writeEnforcementSnapshot(ctx, [entry({ installPath: ".claude/CLAUDE.md" })]);
    const [item] = buildEnforcementState(ctx).items;
    assert.deepEqual(item!.targets[0]!.placedPaths, [".claude/CLAUDE.md"]);
  });
});
