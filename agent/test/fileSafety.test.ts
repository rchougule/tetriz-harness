import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { createAgent } from "../src/agent.js";
import type { DownloadRef, EnforcedArtifact, EnforcedSet, StateReport } from "../src/contract.js";
import type { EngineContext } from "../src/engine/context.js";
import { COLLISION_REASON, pruneDroppedFiles, reconcileArtifactFiles, reconcileRetained } from "../src/engine/converge.js";
import { removePlacedFile, resolveInstallPath, writeAtomic } from "../src/engine/fsops.js";
import { buildWriteGates, gitProbeEnv } from "../src/engine/gitGate.js";
import { activateHook } from "../src/engine/hookActivation.js";
import { compositeKey, RETAINED_REASON, takePendingApplyFailures, writeEnforcementSnapshot } from "../src/engine/state.js";
import type { Transport } from "../src/ports.js";
import { MemoryStore } from "../src/store/memoryStore.js";
import { sha256, testContext } from "./helpers.js";

// Regression cases for the agent's file-safety contract: it replaces, prunes or removes only what it can
// prove it wrote, it never writes or deletes through a symlink that leaves its root, and it keeps exactly
// one owned activation entry per hook through updates and withdrawal.

const RULE_BODY = "# org rules\nbe careful\n";
const HOOK_SCRIPT = "#!/bin/sh\necho guard\n";
const HOOK_COMMAND = '"$HOME"/.claude/hooks/guard.sh';
const OWN_HOOK = { type: "command", command: "echo engineer-own-hook" };

function rule(overrides: Partial<EnforcedArtifact> = {}): EnforcedArtifact {
  return {
    artifactId: "r1",
    ideType: "IDE_TYPE_CLAUDE_CODE",
    type: "ARTIFACT_TYPE_CLAUDE_MD",
    name: "Org rules",
    scope: "ARTIFACT_SCOPE_USER_GLOBAL",
    repoFullName: "",
    installPath: ".claude/CLAUDE.md",
    primaryFileId: "f-rule",
    primaryHash: sha256(RULE_BODY),
    hookCommand: "",
    ...overrides,
  };
}

function hook(overrides: Partial<EnforcedArtifact> = {}): EnforcedArtifact {
  return {
    artifactId: "h1",
    ideType: "IDE_TYPE_CLAUDE_CODE",
    type: "ARTIFACT_TYPE_HOOK",
    name: "Guard",
    scope: "ARTIFACT_SCOPE_USER_GLOBAL",
    repoFullName: "",
    installPath: ".claude/hooks/guard.sh",
    primaryFileId: "f-hook",
    primaryHash: sha256(HOOK_SCRIPT),
    hookConfig: { event: "PreToolUse", matcher: "Bash", timeoutSeconds: 0 },
    hookCommand: HOOK_COMMAND,
    ...overrides,
  };
}

class FakeTransport implements Transport {
  set: EnforcedSet = { artifacts: [] };
  bodies = new Map<string, string>([
    ["f-rule", RULE_BODY],
    ["f-hook", HOOK_SCRIPT],
  ]);
  downloadCalls: string[][] = [];
  reports: StateReport[] = [];

  async getEnforcedSet(): Promise<EnforcedSet> {
    return structuredClone(this.set);
  }

  async getDownloads(fileIds: string[]): Promise<DownloadRef[]> {
    this.downloadCalls.push(fileIds);
    return fileIds.flatMap((fileId) => {
      const body = this.bodies.get(fileId);
      return body === undefined ? [] : [{ fileId, bytes: new TextEncoder().encode(body) }];
    });
  }

  async report(state: StateReport): Promise<void> {
    this.reports.push(structuredClone(state));
  }
}

function settingsAt(home: string): { hooks?: Record<string, Array<{ matcher?: string; hooks: Array<{ command: string }> }>> } {
  return JSON.parse(readFileSync(join(home, ".claude", "settings.json"), "utf8"));
}

/** Every (event, matcher, command) entry in the settings file, flattened for exact comparison. */
function entries(home: string): string[] {
  const out: string[] = [];
  for (const [event, groups] of Object.entries(settingsAt(home).hooks ?? {})) {
    for (const g of groups) for (const h of g.hooks) out.push(`${event}|${g.matcher ?? ""}|${h.command}`);
  }
  return out.sort();
}

describe("agent file safety", () => {
  let dir: string;
  let home: string;
  let outside: string;
  let transport: FakeTransport;
  let store: MemoryStore;

  const make = () => createAgent({ transport, store, roots: { ...testContext(dir).paths, discoverProjects: false } });

  beforeEach(() => {
    dir = realpathSync(mkdtempSync(join(tmpdir(), "agent-safety-")));
    home = join(dir, "home");
    outside = join(dir, "outside");
    mkdirSync(home, { recursive: true });
    mkdirSync(outside, { recursive: true });
    transport = new FakeTransport();
    store = new MemoryStore();
  });

  afterEach(() => {
    chmodSync(dir, 0o755);
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  describe("files the agent did not write", () => {
    it("a different file at the install path survives install, restart and withdrawal, and is reported as a collision", async () => {
      mkdirSync(join(home, ".claude"), { recursive: true });
      writeFileSync(join(home, ".claude", "CLAUDE.md"), "my own notes\n");

      transport.set = { artifacts: [rule()] };
      const first = await make().sync();
      assert.equal(readFileSync(join(home, ".claude", "CLAUDE.md"), "utf8"), "my own notes\n", "not overwritten");
      assert.equal(first.applyFailures.length, 1);
      assert.match(first.applyFailures[0]!.reason, new RegExp(COLLISION_REASON.split(";")[0]!));

      await make().sync(); // a restarted agent, same state
      assert.equal(readFileSync(join(home, ".claude", "CLAUDE.md"), "utf8"), "my own notes\n", "not overwritten after restart");

      transport.set = { artifacts: [] };
      await make().sync();
      await make().sync(); // the second empty read is the one a retract acts on
      assert.equal(readFileSync(join(home, ".claude", "CLAUDE.md"), "utf8"), "my own notes\n", "not removed on withdrawal");
    });

    it("a file already holding the enforced bytes is reported installed, but never adopted or removed", async () => {
      mkdirSync(join(home, ".claude"), { recursive: true });
      writeFileSync(join(home, ".claude", "CLAUDE.md"), RULE_BODY);

      transport.set = { artifacts: [rule()] };
      const result = await make().sync();
      assert.deepEqual(result.applyFailures, []);
      assert.deepEqual(transport.downloadCalls, [], "nothing to download");
      const installed = transport.reports.at(-1)!.installed;
      assert.deepEqual(installed.map((i) => [i.sourcePath, i.contentHash]), [[".claude/CLAUDE.md", sha256(RULE_BODY)]]);
      const entry = (await store.load())!.snapshot[0]!;
      assert.deepEqual(entry.placedPaths, [], "matching bytes do not make it ours");
      assert.deepEqual(entry.presentPaths, [".claude/CLAUDE.md"]);

      transport.set = { artifacts: [] };
      await make().sync();
      await make().sync();
      assert.equal(readFileSync(join(home, ".claude", "CLAUDE.md"), "utf8"), RULE_BODY, "the engineer's identical file stays");
      assert.deepEqual(transport.downloadCalls, []);
    });

    it("a file the agent wrote is still updated, and removed on withdrawal", async () => {
      transport.set = { artifacts: [rule()] };
      await make().sync();
      const v2 = "# org rules v2\n";
      transport.bodies.set("f-rule-2", v2);
      transport.set = { artifacts: [rule({ primaryFileId: "f-rule-2", primaryHash: sha256(v2) })] };
      const result = await make().sync();
      assert.deepEqual(result.applyFailures, []);
      assert.equal(readFileSync(join(home, ".claude", "CLAUDE.md"), "utf8"), v2);

      transport.set = { artifacts: [] };
      await make().sync();
      await make().sync();
      assert.equal(existsSync(join(home, ".claude", "CLAUDE.md")), false);
    });
  });

  describe("a committed file in a checkout", () => {
    let repo: string;
    const RULE_REL = ".claude/rules/org.md";
    const git = (...args: string[]) => execFileSync("git", ["-C", repo, ...args], { encoding: "utf8", env: gitProbeEnv() });

    const repoRule = (): EnforcedArtifact =>
      rule({ type: "ARTIFACT_TYPE_RULE", scope: "ARTIFACT_SCOPE_REPO_LOCAL", repoFullName: "example-org/example-repo", installPath: RULE_REL });

    beforeEach(() => {
      repo = realpathSync(mkdtempSync(join(tmpdir(), "agent-safety-repo-")));
      git("init", "-q");
      git("config", "user.email", "dev@example.com");
      git("config", "user.name", "dev");
      mkdirSync(join(repo, ".claude", "rules"), { recursive: true });
      writeFileSync(join(repo, RULE_REL), "team rule, committed\n");
      git("add", "-f", RULE_REL);
      git("commit", "-qm", "team rule");
    });

    afterEach(() => rmSync(repo, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 }));

    async function reconcileIntoRepo(ctx: EngineContext): Promise<void> {
      const target = { targetKey: repo, root: repo };
      const gates = await buildWriteGates([{ artifact: repoRule(), target }]);
      reconcileArtifactFiles(
        ctx,
        repoRule(),
        new Map([["f-rule", Buffer.from(RULE_BODY)]]),
        new Map(),
        { markConvergedOnMatch: true, trackedPaths: gates.get(repo)?.trackedPaths, cleanTrackedPaths: gates.get(repo)?.cleanTrackedPaths },
        target,
      );
    }

    it("is replaced only while git holds its working copy", async () => {
      const ctx = testContext(dir);
      await reconcileIntoRepo(ctx);
      assert.equal(readFileSync(join(repo, RULE_REL), "utf8"), RULE_BODY, "clean committed copy: git can restore it");
      assert.deepEqual(takePendingApplyFailures(ctx), []);
    });

    it("with uncommitted edits it is never replaced, and the collision is reported", async () => {
      writeFileSync(join(repo, RULE_REL), "team rule, with an uncommitted local edit\n");
      const ctx = testContext(dir);
      await reconcileIntoRepo(ctx);
      assert.equal(readFileSync(join(repo, RULE_REL), "utf8"), "team rule, with an uncommitted local edit\n");
      const failures = takePendingApplyFailures(ctx);
      assert.equal(failures.length, 1);
      assert.ok(failures[0]!.reason.includes(RULE_REL));
    });
  });

  describe("helpers dropped from a still-enforced skill", () => {
    const PRIMARY = "# skill\n";
    const HELPER = "print('helper')\n";
    const PRIMARY_PATH = ".claude/skills/review/SKILL.md";
    const HELPER_PATH = ".claude/skills/review/helper.py";
    let ctx: EngineContext;

    const skill = (withHelper: boolean): EnforcedArtifact =>
      rule({
        artifactId: "sk1",
        type: "ARTIFACT_TYPE_SKILL",
        installPath: PRIMARY_PATH,
        primaryFileId: "f-primary",
        primaryHash: sha256(PRIMARY),
        files: [
          { relativePath: PRIMARY_PATH, fileId: "f-primary", hash: sha256(PRIMARY), executable: false },
          ...(withHelper ? [{ relativePath: HELPER_PATH, fileId: "f-helper", hash: sha256(HELPER), executable: false }] : []),
        ],
      });

    function placeBoth(hashes: Record<string, string> = { [PRIMARY_PATH]: sha256(PRIMARY), [HELPER_PATH]: sha256(HELPER) }): void {
      mkdirSync(join(home, ".claude", "skills", "review"), { recursive: true });
      writeFileSync(join(home, PRIMARY_PATH), PRIMARY);
      writeFileSync(join(home, HELPER_PATH), HELPER);
      const a = skill(true);
      writeEnforcementSnapshot(ctx, [
        {
          artifactId: a.artifactId,
          ideType: a.ideType,
          type: a.type,
          name: a.name,
          scope: a.scope,
          installPath: a.installPath,
          primaryHash: a.primaryHash,
          isHook: false,
          placedPaths: [PRIMARY_PATH, HELPER_PATH],
          placedHashes: hashes,
        },
      ]);
    }

    beforeEach(() => {
      ctx = testContext(dir);
    });

    it("an unchanged helper is removed", () => {
      placeBoth();
      pruneDroppedFiles(ctx, [skill(false)]);
      assert.equal(existsSync(join(home, HELPER_PATH)), false);
      assert.deepEqual(ctx.state.retained, []);
    });

    it("an edited helper stays on disk and is reported, and goes once restored to what the agent wrote", () => {
      placeBoth();
      writeFileSync(join(home, HELPER_PATH), "print('my local fix')\n");
      pruneDroppedFiles(ctx, [skill(false)]);
      assert.equal(readFileSync(join(home, HELPER_PATH), "utf8"), "print('my local fix')\n");
      assert.equal(ctx.state.retained.length, 1);
      assert.equal(ctx.state.retained[0]!.reason, RETAINED_REASON);

      // The artifact is still enforced: the record survives, so the file keeps being reported.
      const liveKey = compositeKey("sk1", "");
      reconcileRetained(ctx, new Set([liveKey]), new Map([[liveKey, new Set([PRIMARY_PATH])]]));
      assert.equal(ctx.state.retained.length, 1, "still reported while the skill stays enforced");

      writeFileSync(join(home, HELPER_PATH), HELPER);
      reconcileRetained(ctx, new Set([liveKey]), new Map([[liveKey, new Set([PRIMARY_PATH])]]));
      assert.equal(existsSync(join(home, HELPER_PATH)), false, "back to the agent's bytes: removed");
      assert.deepEqual(ctx.state.retained, []);
    });

    it("a helper with no recorded hash is left and reported", () => {
      placeBoth({ [PRIMARY_PATH]: sha256(PRIMARY) });
      pruneDroppedFiles(ctx, [skill(false)]);
      assert.equal(existsSync(join(home, HELPER_PATH)), true);
      assert.equal(ctx.state.retained.length, 1);
    });

    it("an unreadable helper is left and reported", { skip: process.getuid?.() === 0 }, () => {
      placeBoth();
      chmodSync(join(home, HELPER_PATH), 0o000);
      pruneDroppedFiles(ctx, [skill(false)]);
      chmodSync(join(home, HELPER_PATH), 0o644);
      assert.equal(readFileSync(join(home, HELPER_PATH), "utf8"), HELPER);
      assert.equal(ctx.state.retained.length, 1);
    });

    it("a helper replaced by a directory is left and reported", () => {
      placeBoth();
      rmSync(join(home, HELPER_PATH));
      mkdirSync(join(home, HELPER_PATH));
      writeFileSync(join(home, HELPER_PATH, "inside.txt"), "keep\n");
      pruneDroppedFiles(ctx, [skill(false)]);
      assert.equal(readFileSync(join(home, HELPER_PATH, "inside.txt"), "utf8"), "keep\n");
      assert.equal(ctx.state.retained.length, 1);
    });

    it("a helper already gone is a no-op", () => {
      placeBoth();
      rmSync(join(home, HELPER_PATH));
      pruneDroppedFiles(ctx, [skill(false)]);
      assert.deepEqual(ctx.state.retained, []);
    });
  });

  describe("symlinks", () => {
    it("a parent directory symlinked outside the root is refused for resolve, write, hash and remove", () => {
      symlinkSync(outside, join(home, ".claude"));
      writeFileSync(join(outside, "CLAUDE.md"), "outside file\n");

      assert.equal(resolveInstallPath(home, ".claude/CLAUDE.md"), null);
      assert.equal(removePlacedFile(home, ".claude/CLAUDE.md"), false);
      assert.throws(() => writeAtomic(join(home, ".claude", "x.md"), Buffer.from("x"), { root: home }), /escapes root/);
      const activation = activateHook("IDE_TYPE_CLAUDE_CODE", home, ".claude/hooks/guard.sh", { event: "PreToolUse", matcher: "Bash", command: "x" }, "user");
      assert.equal(activation.ok, false);
      assert.equal(readFileSync(join(outside, "CLAUDE.md"), "utf8"), "outside file\n", "outside file untouched");
      assert.equal(existsSync(join(outside, "x.md")), false);
      assert.equal(existsSync(join(outside, "settings.json")), false);
    });

    it("a relative symlink that stays inside the root is followed", () => {
      mkdirSync(join(home, "dotfiles", "claude"), { recursive: true });
      symlinkSync("dotfiles/claude", join(home, ".claude"));
      assert.equal(resolveInstallPath(home, ".claude/CLAUDE.md"), join(home, ".claude", "CLAUDE.md"));
    });

    it("an absolute symlink back inside the root is refused by filesystem operations", () => {
      mkdirSync(join(home, "dotfiles", "claude"), { recursive: true });
      symlinkSync(join(home, "dotfiles", "claude"), join(home, ".claude"));
      assert.throws(() => writeAtomic(join(home, ".claude", "CLAUDE.md"), Buffer.from("new"), { root: home }));
      assert.equal(existsSync(join(home, "dotfiles", "claude", "CLAUDE.md")), false);
    });

    it("a dangling symlink, or a file symlinked outside, is refused", () => {
      mkdirSync(join(home, ".claude"), { recursive: true });
      symlinkSync(join(dir, "nowhere"), join(home, ".claude", "skills"));
      writeFileSync(join(outside, "CLAUDE.md"), "outside file\n");
      symlinkSync(join(outside, "CLAUDE.md"), join(home, ".claude", "CLAUDE.md"));
      assert.equal(resolveInstallPath(home, ".claude/skills/a/SKILL.md"), null);
      assert.equal(resolveInstallPath(home, ".claude/CLAUDE.md"), null);
    });

    it("a sync through a symlinked tool directory writes nothing outside and reports the failure", async () => {
      symlinkSync(outside, join(home, ".claude"));
      transport.set = { artifacts: [rule(), hook()] };
      const result = await make().sync();
      assert.equal(result.applyFailures.length, 2);
      for (const f of result.applyFailures) assert.match(f.reason, /escapes root/);
      assert.deepEqual(execFileSync("ls", ["-A", outside], { encoding: "utf8" }), "");
    });
  });

  describe("hook activation entries", () => {
    beforeEach(() => {
      mkdirSync(join(home, ".claude"), { recursive: true });
      writeFileSync(join(home, ".claude", "settings.json"), JSON.stringify({ hooks: { PreToolUse: [{ matcher: "Bash", hooks: [OWN_HOOK] }] } }));
    });

    async function syncHook(h: EnforcedArtifact | null): Promise<void> {
      transport.set = { artifacts: h === null ? [] : [h] };
      await make().sync();
    }

    const OURS = (event: string, matcher: string, command = HOOK_COMMAND) => `${event}|${matcher}|${command}`;
    const THEIRS = `PreToolUse|Bash|${OWN_HOOK.command}`;

    for (const [label, next, expected] of [
      ["matcher", hook({ hookConfig: { event: "PreToolUse", matcher: "Read", timeoutSeconds: 0 } }), OURS("PreToolUse", "Read")],
      ["event", hook({ hookConfig: { event: "PostToolUse", matcher: "Bash", timeoutSeconds: 0 } }), OURS("PostToolUse", "Bash")],
      ["command", hook({ hookCommand: '"$HOME"/.claude/hooks/guard.sh --strict' }), OURS("PreToolUse", "Bash", '"$HOME"/.claude/hooks/guard.sh --strict')],
      [
        "install path",
        hook({ installPath: ".claude/hooks/guard2.sh", hookCommand: '"$HOME"/.claude/hooks/guard2.sh' }),
        OURS("PreToolUse", "Bash", '"$HOME"/.claude/hooks/guard2.sh'),
      ],
    ] as const) {
      it(`a ${label} change leaves exactly the new owned entry, and withdrawal leaves only the engineer's`, async () => {
        await syncHook(hook());
        assert.deepEqual(entries(home), [OURS("PreToolUse", "Bash"), THEIRS].sort());
        await syncHook(next);
        assert.deepEqual(entries(home), [expected, THEIRS].sort());
        await syncHook(null);
        await syncHook(null);
        assert.deepEqual(entries(home), [THEIRS]);
      });
    }

    it("a matching hand-written entry is not taken over, and survives withdrawal", async () => {
      writeFileSync(
        join(home, ".claude", "settings.json"),
        JSON.stringify({ hooks: { PreToolUse: [{ matcher: "Bash", hooks: [{ type: "command", command: HOOK_COMMAND }] }] } }),
      );
      await syncHook(hook());
      assert.equal((await store.load())!.snapshot[0]!.hook, undefined, "found, not added: no ownership recorded");
      await syncHook(null);
      await syncHook(null);
      assert.deepEqual(entries(home), [OURS("PreToolUse", "Bash")], "the engineer's own entry stays");
    });

    it("a teardown that fails on a malformed settings file is kept and retried once the file is repaired", async () => {
      await syncHook(hook());
      const good = readFileSync(join(home, ".claude", "settings.json"), "utf8");
      writeFileSync(join(home, ".claude", "settings.json"), "{ not json");

      await syncHook(null);
      await syncHook(null);
      assert.equal((await store.load())!.snapshot.length, 1, "the record is kept for a retry");
      assert.equal(existsSync(join(home, ".claude", "hooks", "guard.sh")), true, "the script stays with its live entry");

      writeFileSync(join(home, ".claude", "settings.json"), good);
      await syncHook(null);
      assert.deepEqual(entries(home), [THEIRS]);
      assert.equal(existsSync(join(home, ".claude", "hooks", "guard.sh")), false);
      assert.equal((await store.load())!.snapshot.length, 0);
    });
  });
});
