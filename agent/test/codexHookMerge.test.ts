import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync, readFileSync, existsSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { applyCodexHookMerge, applyCodexHookRemoval } from "../src/engine/codexHookMerge.js";
import type { HookMergeSpec } from "../src/engine/hookMerge.js";

// Codex's .codex/hooks.json is the same nested matcher-group shape as Claude's
// settings.json hooks block, so the merge engine is reused — these tests assert the
// Codex file path and shape, and that a Codex-only `statusMessage` survives an upsert.

describe("applyCodexHookMerge / applyCodexHookRemoval — nested hooks.json", () => {
  let dir: string;
  let hooksPath: string;
  const spec: HookMergeSpec = {
    event: "PreToolUse",
    matcher: "Bash",
    command: '"$(git rev-parse --show-toplevel)"/.codex/hooks/pre-bash.sh',
    timeoutSeconds: 120,
  };

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "codex-hook-"));
    hooksPath = join(dir, "hooks.json");
  });
  afterEach(() => rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 }));

  it("creates hooks.json with the nested entry under top-level hooks", () => {
    const result = applyCodexHookMerge(hooksPath, spec);
    assert.deepEqual(result, { ok: true, changed: true, added: true });
    const written = JSON.parse(readFileSync(hooksPath, "utf8"));
    assert.deepEqual(written.hooks.PreToolUse, [
      { matcher: "Bash", hooks: [{ type: "command", command: spec.command, timeout: 120 }] },
    ]);
  });

  it("idempotent re-run writes nothing", () => {
    applyCodexHookMerge(hooksPath, spec);
    const after = readFileSync(hooksPath, "utf8");
    const result = applyCodexHookMerge(hooksPath, spec);
    assert.deepEqual(result, { ok: true, changed: false, added: false });
    assert.equal(readFileSync(hooksPath, "utf8"), after);
  });

  it("preserves a Codex-only statusMessage on the same entry when only the timeout changes", () => {
    const existing = {
      hooks: {
        PreToolUse: [
          {
            matcher: "Bash",
            hooks: [{ type: "command", command: spec.command, timeout: 10, statusMessage: "Checking policy" }],
          },
        ],
      },
    };
    writeFileSync(hooksPath, JSON.stringify(existing, null, 2));

    const result = applyCodexHookMerge(hooksPath, { ...spec, timeoutSeconds: 120 });
    assert.deepEqual(result, { ok: true, changed: true, added: false });
    const written = JSON.parse(readFileSync(hooksPath, "utf8"));
    assert.deepEqual(written.hooks.PreToolUse[0].hooks[0], {
      type: "command",
      command: spec.command,
      timeout: 120,
      statusMessage: "Checking policy",
    });
  });

  it("removal drops exactly the one entry and prunes the emptied event", () => {
    applyCodexHookMerge(hooksPath, spec);
    const result = applyCodexHookRemoval(hooksPath, spec);
    assert.deepEqual(result, { ok: true, changed: true });
    const written = JSON.parse(readFileSync(hooksPath, "utf8"));
    assert.equal(written.hooks.PreToolUse, undefined);
  });

  it("malformed hooks.json is a failure with the file left byte-identical", () => {
    const malformed = "{ not json ";
    writeFileSync(hooksPath, malformed);
    const result = applyCodexHookMerge(hooksPath, spec);
    assert.equal(result.ok, false);
    assert.equal(readFileSync(hooksPath, "utf8"), malformed);
  });

  it("removal on a missing file is a no-op success", () => {
    const result = applyCodexHookRemoval(hooksPath, spec);
    assert.deepEqual(result, { ok: true, changed: false });
    assert.equal(existsSync(hooksPath), false);
  });
});
