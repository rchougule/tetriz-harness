import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync, readFileSync, existsSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { applyHookMerge, applyHookRemoval, type HookMergeSpec } from "../src/engine/hookMerge.js";

// The structured merge of one hook entry into a co-owned settings.json.

describe("applyHookMerge / applyHookRemoval", () => {
  let dir: string;
  let settingsPath: string;
  const spec: HookMergeSpec = {
    event: "PreToolUse",
    matcher: "Bash",
    command: '"$CLAUDE_PROJECT_DIR"/.claude/hooks/measure.sh',
    timeoutSeconds: 30,
  };

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "hook-merge-"));
    settingsPath = join(dir, "settings.json");
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  it("creates the file with just the declared entry when absent", () => {
    const result = applyHookMerge(settingsPath, spec);
    assert.deepEqual(result, { ok: true, changed: true, added: true });
    const written = JSON.parse(readFileSync(settingsPath, "utf8"));
    assert.deepEqual(written.hooks.PreToolUse, [
      { matcher: "Bash", hooks: [{ type: "command", command: spec.command, timeout: 30 }] },
    ]);
  });

  it("CT-HK-03: preserves the engineer's other top-level keys and undeclared hooks", () => {
    const existing = {
      permissions: { allow: ["Bash(git *)"] },
      env: { FOO: "bar" },
      hooks: {
        PreToolUse: [
          { matcher: "Write", hooks: [{ type: "command", command: "echo other-hook" }] },
        ],
        Stop: [{ matcher: "", hooks: [{ type: "command", command: "echo stop-hook" }] }],
      },
    };
    writeFileSync(settingsPath, JSON.stringify(existing, null, 2));

    const result = applyHookMerge(settingsPath, spec);
    assert.deepEqual(result, { ok: true, changed: true, added: true });

    const written = JSON.parse(readFileSync(settingsPath, "utf8"));
    assert.deepEqual(written.permissions, existing.permissions);
    assert.deepEqual(written.env, existing.env);
    // The undeclared Write-matcher group and the Stop event are untouched.
    assert.deepEqual(written.hooks.Stop, existing.hooks.Stop);
    assert.equal(written.hooks.PreToolUse.length, 2);
    assert.deepEqual(
      written.hooks.PreToolUse.find((g: { matcher: string }) => g.matcher === "Write"),
      existing.hooks.PreToolUse[0],
    );
    // The declared entry landed in its own matcher group.
    const ours = written.hooks.PreToolUse.find((g: { matcher: string }) => g.matcher === "Bash");
    assert.deepEqual(ours.hooks, [{ type: "command", command: spec.command, timeout: 30 }]);
  });

  it("CT-HK-04: re-running the same spec is idempotent (changed: false, no rewrite)", () => {
    applyHookMerge(settingsPath, spec);
    const afterFirst = readFileSync(settingsPath, "utf8");

    const result = applyHookMerge(settingsPath, spec);
    assert.deepEqual(result, { ok: true, changed: false, added: false });
    assert.equal(readFileSync(settingsPath, "utf8"), afterFirst);
  });

  it("CT-HK-05: malformed settings.json is reported as a failure, file byte-identical", () => {
    const malformed = "{ not valid json ";
    writeFileSync(settingsPath, malformed);

    const result = applyHookMerge(settingsPath, spec);
    assert.equal(result.ok, false);
    if (!result.ok) assert.match(result.reason, /not valid JSON/);
    assert.equal(readFileSync(settingsPath, "utf8"), malformed);
  });

  it("CT-HK-05b: a JSON array (not an object) is also reported as a failure, untouched", () => {
    const notAnObject = "[1, 2, 3]";
    writeFileSync(settingsPath, notAnObject);

    const result = applyHookMerge(settingsPath, spec);
    assert.equal(result.ok, false);
    assert.equal(readFileSync(settingsPath, "utf8"), notAnObject);
  });

  it("CT-HK-06: removal drops exactly one entry, leaving siblings and other keys untouched", () => {
    const sibling: HookMergeSpec = { event: "PreToolUse", matcher: "Bash", command: "echo sibling" };
    const existing = {
      permissions: { allow: ["Bash(git *)"] },
      hooks: {
        PreToolUse: [
          {
            matcher: "Bash",
            hooks: [
              { type: "command", command: spec.command, timeout: 30 },
              { type: "command", command: sibling.command },
            ],
          },
        ],
        Stop: [{ matcher: "", hooks: [{ type: "command", command: "echo stop-hook" }] }],
      },
    };
    writeFileSync(settingsPath, JSON.stringify(existing, null, 2));

    const result = applyHookRemoval(settingsPath, spec);
    assert.deepEqual(result, { ok: true, changed: true });

    const written = JSON.parse(readFileSync(settingsPath, "utf8"));
    assert.deepEqual(written.permissions, existing.permissions);
    assert.deepEqual(written.hooks.Stop, existing.hooks.Stop);
    // Only the declared entry is gone; the sibling in the SAME matcher group survives.
    assert.deepEqual(written.hooks.PreToolUse, [
      { matcher: "Bash", hooks: [{ type: "command", command: sibling.command }] },
    ]);
  });

  it("removal that empties a matcher group drops the group, not a dangling empty one", () => {
    const existing = {
      hooks: {
        PreToolUse: [{ matcher: "Bash", hooks: [{ type: "command", command: spec.command, timeout: 30 }] }],
      },
    };
    writeFileSync(settingsPath, JSON.stringify(existing, null, 2));

    const result = applyHookRemoval(settingsPath, spec);
    assert.deepEqual(result, { ok: true, changed: true });

    const written = JSON.parse(readFileSync(settingsPath, "utf8"));
    assert.equal(written.hooks.PreToolUse, undefined);
  });

  it("removal on a missing file is a no-op success (nothing to remove)", () => {
    const result = applyHookRemoval(settingsPath, spec);
    assert.deepEqual(result, { ok: true, changed: false });
    assert.equal(existsSync(settingsPath), false);
  });

  it("removal of an already-absent entry is a no-op success, file untouched", () => {
    const existing = { hooks: { Stop: [{ matcher: "", hooks: [{ type: "command", command: "echo x" }] }] } };
    writeFileSync(settingsPath, JSON.stringify(existing, null, 2));
    const before = readFileSync(settingsPath, "utf8");

    const result = applyHookRemoval(settingsPath, spec);
    assert.deepEqual(result, { ok: true, changed: false });
    assert.equal(readFileSync(settingsPath, "utf8"), before);
  });

  // Matcher-less events (UserPromptSubmit, Stop, SessionStart, …) carry no matcher. Identity
  // normalizes an absent matcher to "", so a re-run is idempotent and removal finds the entry.
  const matcherlessSpec: HookMergeSpec = {
    event: "UserPromptSubmit",
    command: '"$CLAUDE_PROJECT_DIR"/.claude/hooks/prompt-guard.sh',
  };

  it("matcher-less hook: created without a matcher key", () => {
    const result = applyHookMerge(settingsPath, matcherlessSpec);
    assert.deepEqual(result, { ok: true, changed: true, added: true });
    const written = JSON.parse(readFileSync(settingsPath, "utf8"));
    assert.deepEqual(written.hooks.UserPromptSubmit, [
      { hooks: [{ type: "command", command: matcherlessSpec.command }] },
    ]);
  });

  it("matcher-less hook: re-running is idempotent — no duplicate group appended", () => {
    applyHookMerge(settingsPath, matcherlessSpec);
    applyHookMerge(settingsPath, matcherlessSpec); // simulate further convergence cycles
    const result = applyHookMerge(settingsPath, matcherlessSpec);
    assert.deepEqual(result, { ok: true, changed: false, added: false });
    const written = JSON.parse(readFileSync(settingsPath, "utf8"));
    assert.equal(written.hooks.UserPromptSubmit.length, 1);
  });

  it('matcher-less hook: dedupes against a group whose matcher was written as ""', () => {
    // Another producer / prior write used the explicit-"" shape; identity must treat
    // it as the same entry (normalizeMatcher), not append a second group.
    writeFileSync(
      settingsPath,
      JSON.stringify({
        hooks: {
          UserPromptSubmit: [{ matcher: "", hooks: [{ type: "command", command: matcherlessSpec.command }] }],
        },
      }),
    );
    const result = applyHookMerge(settingsPath, matcherlessSpec);
    assert.deepEqual(result, { ok: true, changed: false, added: false });
    const written = JSON.parse(readFileSync(settingsPath, "utf8"));
    assert.equal(written.hooks.UserPromptSubmit.length, 1);
  });

  it("matcher-less hook: removal finds and drops the entry", () => {
    applyHookMerge(settingsPath, matcherlessSpec);
    const result = applyHookRemoval(settingsPath, matcherlessSpec);
    assert.deepEqual(result, { ok: true, changed: true });
    const written = JSON.parse(readFileSync(settingsPath, "utf8"));
    assert.equal(written.hooks.UserPromptSubmit, undefined);
  });

  it("self-heal: collapses pre-existing duplicate groups into one", () => {
    // A machine polluted by the pre-fix bug — N identical matcher-less groups on
    // disk. The next converge must drive back to a single group, not just stop growing.
    const dup = { hooks: [{ type: "command", command: matcherlessSpec.command }] };
    writeFileSync(settingsPath, JSON.stringify({ hooks: { UserPromptSubmit: [dup, dup, dup] } }));

    const result = applyHookMerge(settingsPath, matcherlessSpec);
    assert.deepEqual(result, { ok: true, changed: true, added: false });
    const written = JSON.parse(readFileSync(settingsPath, "utf8"));
    assert.deepEqual(written.hooks.UserPromptSubmit, [
      { hooks: [{ type: "command", command: matcherlessSpec.command }] },
    ]);
  });

  it("self-heal: collapse strips only our command, leaving the engineer's own hooks", () => {
    const userCmd = "echo engineer-own-hook";
    writeFileSync(
      settingsPath,
      JSON.stringify({
        hooks: {
          UserPromptSubmit: [
            { hooks: [{ type: "command", command: matcherlessSpec.command }] },
            {
              hooks: [
                { type: "command", command: matcherlessSpec.command },
                { type: "command", command: userCmd },
              ],
            },
          ],
        },
      }),
    );

    const result = applyHookMerge(settingsPath, matcherlessSpec);
    assert.deepEqual(result, { ok: true, changed: true, added: false });
    const written = JSON.parse(readFileSync(settingsPath, "utf8"));
    // Our duplicate is collapsed into the canonical (first) group; the engineer's
    // hook survives in its own group (our command stripped from it, nothing relocated).
    assert.deepEqual(written.hooks.UserPromptSubmit, [
      { hooks: [{ type: "command", command: matcherlessSpec.command }] },
      { hooks: [{ type: "command", command: userCmd }] },
    ]);
  });
});
