import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync, readFileSync, existsSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { applyCursorHookMerge, applyCursorHookRemoval } from "../src/engine/cursorHookMerge.js";
import type { HookMergeSpec } from "../src/engine/hookMerge.js";

// Cursor's .cursor/hooks.json is FLAT — {version, hooks:{<event>:[{command, matcher?}]}}
// — the command inline, no `type`, no nested hooks[]. Identity is (event, command).

describe("applyCursorHookMerge / applyCursorHookRemoval — flat hooks.json", () => {
  let dir: string;
  let hooksPath: string;
  // A real Cursor event (camelCase) with no matcher — a lifecycle hook.
  const spec: HookMergeSpec = {
    event: "beforeSubmitPrompt",
    matcher: "",
    command: ".cursor/hooks/guard.sh",
  };

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "cursor-hook-"));
    hooksPath = join(dir, "hooks.json");
  });
  afterEach(() => rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 }));

  it("creates hooks.json with version 1 and a flat entry (no type, no matcher when empty)", () => {
    const result = applyCursorHookMerge(hooksPath, spec);
    assert.deepEqual(result, { ok: true, changed: true, added: true });
    const written = JSON.parse(readFileSync(hooksPath, "utf8"));
    assert.equal(written.version, 1);
    assert.deepEqual(written.hooks.beforeSubmitPrompt, [{ command: ".cursor/hooks/guard.sh" }]);
  });

  it("writes a matcher inline only when the spec carries one", () => {
    applyCursorHookMerge(hooksPath, { event: "beforeReadFile", matcher: "**/*.env", command: ".cursor/hooks/secret.sh" });
    const written = JSON.parse(readFileSync(hooksPath, "utf8"));
    assert.deepEqual(written.hooks.beforeReadFile, [{ command: ".cursor/hooks/secret.sh", matcher: "**/*.env" }]);
  });

  it("idempotent re-run writes nothing", () => {
    applyCursorHookMerge(hooksPath, spec);
    const after = readFileSync(hooksPath, "utf8");
    const result = applyCursorHookMerge(hooksPath, spec);
    assert.deepEqual(result, { ok: true, changed: false, added: false });
    assert.equal(readFileSync(hooksPath, "utf8"), after);
  });

  it("preserves the engineer's other events and a co-owned version, merging into its own event", () => {
    const existing = {
      version: 1,
      hooks: {
        stop: [{ command: ".cursor/hooks/user-stop.sh" }],
        beforeSubmitPrompt: [{ command: ".cursor/hooks/user-submit.sh" }],
      },
    };
    writeFileSync(hooksPath, JSON.stringify(existing, null, 2));

    const result = applyCursorHookMerge(hooksPath, spec);
    assert.deepEqual(result, { ok: true, changed: true, added: true });
    const written = JSON.parse(readFileSync(hooksPath, "utf8"));
    assert.equal(written.version, 1);
    assert.deepEqual(written.hooks.stop, existing.hooks.stop);
    // Our entry is appended beside the engineer's own beforeSubmitPrompt hook.
    assert.deepEqual(written.hooks.beforeSubmitPrompt, [
      { command: ".cursor/hooks/user-submit.sh" },
      { command: ".cursor/hooks/guard.sh" },
    ]);
  });

  it("removal drops exactly the (event, command) entry, leaving a sibling in the same event", () => {
    const existing = {
      version: 1,
      hooks: {
        beforeSubmitPrompt: [{ command: ".cursor/hooks/guard.sh" }, { command: ".cursor/hooks/other.sh" }],
      },
    };
    writeFileSync(hooksPath, JSON.stringify(existing, null, 2));

    const result = applyCursorHookRemoval(hooksPath, spec);
    assert.deepEqual(result, { ok: true, changed: true });
    const written = JSON.parse(readFileSync(hooksPath, "utf8"));
    assert.deepEqual(written.hooks.beforeSubmitPrompt, [{ command: ".cursor/hooks/other.sh" }]);
  });

  it("removal that empties an event drops the event key, not a dangling []", () => {
    applyCursorHookMerge(hooksPath, spec);
    applyCursorHookRemoval(hooksPath, spec);
    const written = JSON.parse(readFileSync(hooksPath, "utf8"));
    assert.equal(written.hooks.beforeSubmitPrompt, undefined);
  });

  it("malformed hooks.json is a failure with the file left byte-identical", () => {
    const malformed = "{ nope ";
    writeFileSync(hooksPath, malformed);
    const result = applyCursorHookMerge(hooksPath, spec);
    assert.equal(result.ok, false);
    assert.equal(readFileSync(hooksPath, "utf8"), malformed);
  });

  it("a JSON array (not an object) is a failure, file untouched", () => {
    const notObj = "[1,2,3]";
    writeFileSync(hooksPath, notObj);
    const result = applyCursorHookMerge(hooksPath, spec);
    assert.equal(result.ok, false);
    assert.equal(readFileSync(hooksPath, "utf8"), notObj);
  });

  it("removal on a missing file is a no-op success", () => {
    const result = applyCursorHookRemoval(hooksPath, spec);
    assert.deepEqual(result, { ok: true, changed: false });
    assert.equal(existsSync(hooksPath), false);
  });
});
