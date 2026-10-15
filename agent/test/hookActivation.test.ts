import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtempSync, mkdirSync, rmSync, readFileSync, existsSync, statSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import type { EngineContext } from "../src/engine/context.js";
import { testContext } from "./helpers.js";
import { activationFilePath, ideSupportsHookActivation, activateHook, deactivateHook } from "../src/engine/hookActivation.js";
import {
  reconcileArtifactFiles,
  reconcileRemovedArtifacts,
} from "../src/engine/converge.js";
import { type ConvergenceTarget } from "../src/engine/artifacts.js";
import { type EnforcedArtifact } from "../src/contract.js";
import { writeEnforcementSnapshot, type SnapshotEntry } from "../src/engine/state.js";

function sha256(s: string): string {
  return createHash("sha256").update(s).digest("hex");
}

describe("hookActivation — file-path dispatch by ide_type", () => {
  it("locates each tool's activation file beside its placed hooks/ dir (from the script path)", () => {
    // Codex/Cursor → hooks.json under their dir, at either scope.
    assert.equal(
      activationFilePath("IDE_TYPE_CODEX", "/repo", ".codex/hooks/guard.sh", "repo"),
      "/repo/.codex/hooks.json",
    );
    assert.equal(
      activationFilePath("IDE_TYPE_CURSOR", "/repo", ".cursor/hooks/guard.sh", "repo"),
      "/repo/.cursor/hooks.json",
    );
  });

  it("Claude Code's activation file is chosen by scope: local in a checkout, shareable at user scope", () => {
    // A checkout's settings.json is the committed, shareable file, so a repo-scoped device write
    // goes to settings.local.json. ~/.claude/settings.json is in nobody's repo — and is the only
    // user-scope file Claude Code reads — so user scope keeps it.
    assert.equal(
      activationFilePath("IDE_TYPE_CLAUDE_CODE", "/repo", ".claude/hooks/guard.sh", "repo"),
      "/repo/.claude/settings.local.json",
    );
    assert.equal(
      activationFilePath("IDE_TYPE_CLAUDE_CODE", "/home/dev", ".claude/hooks/guard.sh", "user"),
      "/home/dev/.claude/settings.json",
    );
  });

  it("returns null for an unsupported ide and for a path escaping the root", () => {
    assert.equal(ideSupportsHookActivation("IDE_TYPE_COPILOT"), false);
    assert.equal(activationFilePath("IDE_TYPE_COPILOT", "/root", ".x/hooks/g.sh", "repo"), null);
    assert.equal(activationFilePath("IDE_TYPE_CLAUDE_CODE", "/root", "../../etc/hooks/x.sh", "repo"), null);
  });

  it("an unsupported ide activation is a silent no-op success, not a failure", () => {
    const dir = mkdtempSync(join(tmpdir(), "act-unsup-"));
    try {
      const spec = { event: "PreToolUse", matcher: "", command: "x" };
      assert.deepEqual(activateHook("IDE_TYPE_COPILOT", dir, ".x/hooks/g.sh", spec, "repo"), { ok: true, changed: false });
      assert.deepEqual(deactivateHook("IDE_TYPE_COPILOT", dir, ".x/hooks/g.sh", spec, "repo"), { ok: true, changed: false });
    } finally {
      rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
    }
  });
});

describe("hook activation through convergence — per-target root (REPO_LOCAL under checkout)", () => {
  let dir: string;
  let home: string;
  let checkout: string;
  let ctx: EngineContext;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "act-conv-"));
    home = join(dir, "home");
    checkout = join(dir, "checkout");
    mkdirSync(home, { recursive: true });
    mkdirSync(checkout, { recursive: true });
    ctx = testContext(dir);
  });
  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  const repoTarget = (root: string): ConvergenceTarget => ({ targetKey: root, root });

  it("Codex REPO_LOCAL hook: script + nested .codex/hooks.json land under the checkout, not home", () => {
    const script = "#!/bin/sh\necho codex\n";
    const command = '"$(git rev-parse --show-toplevel)"/.codex/hooks/pre-bash.sh';
    const a: EnforcedArtifact = {
      artifactId: "cxh1",
      ideType: "IDE_TYPE_CODEX",
      type: "ARTIFACT_TYPE_HOOK",
      name: "Codex pre-bash",
      scope: "ARTIFACT_SCOPE_REPO_LOCAL",
      repoFullName: "example-org/example-repo",
      installPath: ".codex/hooks/pre-bash.sh",
      primaryFileId: "f1",
      primaryHash: sha256(script),
      hookConfig: { event: "PreToolUse", matcher: "Bash", timeoutSeconds: 120 },
      hookCommand: command,
    };
    reconcileArtifactFiles(ctx, a, new Map([["f1", Buffer.from(script)]]), new Map(), { markConvergedOnMatch: true }, repoTarget(checkout));

    const scriptPath = join(checkout, ".codex", "hooks", "pre-bash.sh");
    assert.equal(readFileSync(scriptPath, "utf8"), script);
    assert.equal(statSync(scriptPath).mode & 0o111, 0o111, "hook script is executable");
    // Activation is the nested Codex shape, in the checkout — never in home.
    const hooks = JSON.parse(readFileSync(join(checkout, ".codex", "hooks.json"), "utf8"));
    assert.deepEqual(hooks.hooks.PreToolUse, [
      { matcher: "Bash", hooks: [{ type: "command", command, timeout: 120 }] },
    ]);
    assert.equal(existsSync(join(home, ".codex", "hooks.json")), false, "no activation leaked to home");
  });

  it("Cursor REPO_LOCAL hook: script + flat .cursor/hooks.json land under the checkout", () => {
    const script = "#!/bin/sh\necho cursor\n";
    const command = ".cursor/hooks/guard.sh";
    const a: EnforcedArtifact = {
      artifactId: "cuh1",
      ideType: "IDE_TYPE_CURSOR",
      type: "ARTIFACT_TYPE_HOOK",
      name: "Cursor guard",
      scope: "ARTIFACT_SCOPE_REPO_LOCAL",
      repoFullName: "example-org/example-repo",
      installPath: ".cursor/hooks/guard.sh",
      primaryFileId: "f1",
      primaryHash: sha256(script),
      hookConfig: { event: "beforeSubmitPrompt", matcher: "", timeoutSeconds: 0 },
      hookCommand: command,
    };
    reconcileArtifactFiles(ctx, a, new Map([["f1", Buffer.from(script)]]), new Map(), { markConvergedOnMatch: true }, repoTarget(checkout));

    assert.equal(readFileSync(join(checkout, ".cursor", "hooks", "guard.sh"), "utf8"), script);
    const hooks = JSON.parse(readFileSync(join(checkout, ".cursor", "hooks.json"), "utf8"));
    assert.equal(hooks.version, 1);
    // Flat shape: command inline, no `type`, no matcher (spec matcher empty).
    assert.deepEqual(hooks.hooks.beforeSubmitPrompt, [{ command }]);
  });

  it("removal retracts a Codex REPO_LOCAL hook from the checkout's hooks.json (right file + target)", () => {
    const command = '"$(git rev-parse --show-toplevel)"/.codex/hooks/pre-bash.sh';
    const script = "#!/bin/sh\necho codex\n";
    const a: EnforcedArtifact = {
      artifactId: "cxh1",
      ideType: "IDE_TYPE_CODEX",
      type: "ARTIFACT_TYPE_HOOK",
      name: "Codex pre-bash",
      scope: "ARTIFACT_SCOPE_REPO_LOCAL",
      repoFullName: "example-org/example-repo",
      installPath: ".codex/hooks/pre-bash.sh",
      primaryFileId: "f1",
      primaryHash: sha256(script),
      hookConfig: { event: "PreToolUse", matcher: "Bash", timeoutSeconds: 120 },
      hookCommand: command,
    };
    // Place + activate, then record the snapshot entry (as the pass does).
    reconcileArtifactFiles(ctx, a, new Map([["f1", Buffer.from(script)]]), new Map(), { markConvergedOnMatch: true }, repoTarget(checkout));
    const snapEntry: SnapshotEntry = {
      artifactId: "cxh1",
      ideType: "IDE_TYPE_CODEX",
      type: "ARTIFACT_TYPE_HOOK",
      name: "Codex pre-bash",
      scope: "ARTIFACT_SCOPE_REPO_LOCAL",
      targetKey: checkout,
      installPath: ".codex/hooks/pre-bash.sh",
      primaryHash: sha256(script),
      isHook: true,
      hook: { event: "PreToolUse", matcher: "Bash", command },
    };
    writeEnforcementSnapshot(ctx, [snapEntry]);

    // De-served this cycle → retract from the exact checkout hooks.json.
    reconcileRemovedArtifacts(ctx, new Set<string>(), new Map());

    const hooks = JSON.parse(readFileSync(join(checkout, ".codex", "hooks.json"), "utf8"));
    assert.equal(hooks.hooks.PreToolUse, undefined, "activation retracted from the checkout");
  });
});
