import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import type { EngineContext } from "../src/engine/context.js";
import { testContext } from "./helpers.js";
import {
  pruneDroppedFiles,
  reconcileArtifactFiles,
  reconcileRemovedArtifacts,
  snapshotEntryForCycle,
  withExpiryMark,
  type EnforcedDesire,
} from "../src/engine/converge.js";
import {
  buildWriteGates,
  gitProbeEnv,
} from "../src/engine/gitGate.js";
import { type TargetedArtifact } from "../src/engine/artifacts.js";
import { type EnforcedArtifact } from "../src/contract.js";
import {
  buildEnforcementState,
  compositeKey,
  readEnforcementSnapshot,
  readLedgerEntry,
  recordReconcileFailure,
  writeEnforcementSnapshot,
  type SnapshotEntry,
} from "../src/engine/state.js";
import { applyHookMerge } from "../src/engine/hookMerge.js";

// Expiry is not absence: a served artifact marked `expired` is frozen, an absent one retracts.
// Driven through the pass's own building blocks in the pass's order (no network or config
// mocked); the git half runs a real temp repo, since tracked-vs-untracked is git's answer.

const SCOPE_USER_GLOBAL = "ARTIFACT_SCOPE_USER_GLOBAL";
const SCOPE_REPO_LOCAL = "ARTIFACT_SCOPE_REPO_LOCAL";
const ENFORCE_UNTIL = "2026-09-01T00:00:00Z";

const sha256 = (s: string): string => createHash("sha256").update(s).digest("hex");
const git = (root: string, ...args: string[]): string =>
  execFileSync("git", ["-C", root, ...args], { encoding: "utf8", env: gitProbeEnv() });

const RULE_BODY = "# Org CLAUDE.md\nstandard\n";
const HOOK_SCRIPT = "#!/bin/sh\necho measured\n";
const HOOK_COMMAND = '"$HOME"/.claude/hooks/measure.sh';
const HAND_WRITTEN_COMMAND = '"$HOME"/.claude/hooks/team-guard.sh';

function rule(overrides: Partial<EnforcedArtifact> = {}): EnforcedArtifact {
  return {
    artifactId: "r1",
    ideType: "IDE_TYPE_CLAUDE_CODE",
    type: "ARTIFACT_TYPE_CLAUDE_MD",
    name: "Org CLAUDE.md",
    scope: SCOPE_USER_GLOBAL,
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
    name: "Measure hook",
    scope: SCOPE_USER_GLOBAL,
    repoFullName: "",
    installPath: ".claude/hooks/measure.sh",
    primaryFileId: "f-hook",
    primaryHash: sha256(HOOK_SCRIPT),
    hookConfig: { event: "PreToolUse", matcher: "Bash", timeoutSeconds: 30 },
    hookCommand: HOOK_COMMAND,
    ...overrides,
  };
}

/** The marked hook this device never converged — its (event, matcher, command) entry is the
 *  engineer's own, written by hand into settings.json. */
function handWrittenHook(): EnforcedArtifact {
  return hook({
    artifactId: "h2",
    name: "Team guard",
    installPath: ".claude/hooks/team-guard.sh",
    primaryFileId: "f-guard",
    primaryHash: sha256("#!/bin/sh\necho guard\n"),
    hookConfig: { event: "UserPromptSubmit", matcher: "", timeoutSeconds: 0 },
    hookCommand: HAND_WRITTEN_COMMAND,
    expired: true,
    enforceUntil: ENFORCE_UNTIL,
  });
}

const userGlobal = (artifact: EnforcedArtifact, home: string): TargetedArtifact => ({
  artifact,
  target: { targetKey: "", root: home },
});

const keyOf = (t: TargetedArtifact): string => compositeKey(t.artifact.artifactId, t.target.targetKey);

/** The pass's end-of-cycle snapshot rewrite, over the previous snapshot. */
function rewriteSnapshot(ctx: EngineContext, targeted: TargetedArtifact[]): SnapshotEntry[] {
  const prevByKey = new Map(readEnforcementSnapshot(ctx).map((e) => [compositeKey(e.artifactId, e.targetKey ?? ""), e]));
  const entries = targeted.flatMap((t) => {
    const entry = snapshotEntryForCycle(ctx, t, prevByKey.get(keyOf(t)));
    return entry === null ? [] : [entry];
  });
  writeEnforcementSnapshot(ctx, entries);
  return entries;
}

function settingsOf(home: string): { hooks?: Record<string, unknown[]> } {
  return JSON.parse(readFileSync(join(home, ".claude", "settings.json"), "utf8")) as { hooks?: Record<string, unknown[]> };
}

describe("a served-but-expired artifact is left exactly as it is", () => {
  let dir: string;
  let home: string;
  let ctx: EngineContext;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "h-expiry-"));
    home = join(dir, "home");
    mkdirSync(home, { recursive: true });
    ctx = testContext(dir);
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  /** Cycle 0: converge the rule and the hook, snapshot them, and let the engineer add their own
   *  hook entry by hand. Returns the reconciled-at stamps so a later cycle can prove no re-record. */
  function convergeFirstCycle(): { ruleReconciledAt: string; hookReconciledAt: string } {
    const bodies = new Map([
      ["f-rule", Buffer.from(RULE_BODY)],
      ["f-hook", Buffer.from(HOOK_SCRIPT)],
    ]);
    reconcileArtifactFiles(ctx, rule(), bodies, new Map(), { markConvergedOnMatch: true });
    reconcileArtifactFiles(ctx, hook(), bodies, new Map(), { markConvergedOnMatch: true });
    rewriteSnapshot(ctx, [userGlobal(rule(), home), userGlobal(hook(), home)]);
    applyHookMerge(join(home, ".claude", "settings.json"), {
      event: "UserPromptSubmit",
      matcher: "",
      command: HAND_WRITTEN_COMMAND,
    });
    assert.equal(readLedgerEntry(ctx, "r1")?.kind, "installed");
    assert.equal(settingsOf(home).hooks?.PreToolUse?.length, 1, "converged hook is active");
    assert.equal(settingsOf(home).hooks?.UserPromptSubmit?.length, 1, "hand-written entry present");
    return {
      ruleReconciledAt: readLedgerEntry(ctx, "r1")!.reconciledAt,
      hookReconciledAt: readLedgerEntry(ctx, "h1")!.reconciledAt,
    };
  }

  it("expired: no rewrite, no revert, no settings change; snapshot kept and labelled ended; never-converged gets no entry", () => {
    const stamps = convergeFirstCycle();
    // A local edit under ENFORCE would be reverted next cycle — unless the window has closed.
    writeFileSync(join(home, ".claude", "CLAUDE.md"), "my local edit\n");

    // Cycle 1: the same set, every artifact served marked. The pass's steps, in its order.
    const served = [
      rule({ expired: true, enforceUntil: ENFORCE_UNTIL }),
      hook({ expired: true, enforceUntil: ENFORCE_UNTIL }),
      handWrittenHook(),
    ];
    const targeted = served.map((a) => userGlobal(a, home));
    const live = targeted.filter((t) => t.artifact.expired !== true);
    assert.deepEqual(live, [], "nothing live this cycle");
    const currentKeys = new Set(targeted.map(keyOf));
    const enforcedById = new Map<string, EnforcedDesire>(
      served.map((a) => [a.artifactId, { scope: a.scope, repoFullName: a.repoFullName }]),
    );
    const deferred = reconcileRemovedArtifacts(ctx, currentKeys, enforcedById);
    assert.deepEqual(deferred, [], "nothing is absent, so nothing retracts or defers");
    pruneDroppedFiles(ctx, targeted); // the exported guard, with the full set

    const bodies = new Map([
      ["f-rule", Buffer.from(RULE_BODY)],
      ["f-hook", Buffer.from(HOOK_SCRIPT)],
      ["f-guard", Buffer.from("#!/bin/sh\necho guard\n")],
    ]);
    const prevHook = { event: "PreToolUse", matcher: "Bash", command: HOOK_COMMAND };
    // The exported reconcile guard: even handed a verified body and placement proof, a marked
    // artifact is not touched (the pass itself never gets this far for one).
    for (const t of targeted) {
      reconcileArtifactFiles(ctx, t.artifact, bodies, new Map(), {
        markConvergedOnMatch: true,
        priorHook: t.artifact.artifactId === "h1" ? prevHook : null,
      });
    }

    assert.equal(readFileSync(join(home, ".claude", "CLAUDE.md"), "utf8"), "my local edit\n", "local edit not reverted");
    assert.equal(readLedgerEntry(ctx, "r1")?.reconciledAt, stamps.ruleReconciledAt, "rule ledger not re-recorded");
    assert.equal(readLedgerEntry(ctx, "r1")?.kind, "installed", "no 'reverted' recorded");
    assert.equal(readLedgerEntry(ctx, "h1")?.reconciledAt, stamps.hookReconciledAt, "hook ledger not re-recorded");
    assert.equal(existsSync(join(home, ".claude", "hooks", "team-guard.sh")), false, "never-converged script not placed");
    const settings = settingsOf(home);
    assert.equal(settings.hooks?.PreToolUse?.length, 1, "converged hook's entry untouched");
    assert.equal(settings.hooks?.UserPromptSubmit?.length, 1, "hand-written entry untouched");

    const snapshot = rewriteSnapshot(ctx, targeted);
    assert.deepEqual(
      snapshot.map((e) => e.artifactId),
      ["r1", "h1"],
      "both converged entries kept; the never-converged marked hook gets none",
    );
    for (const e of snapshot) {
      assert.equal(e.expired, true);
      assert.equal(e.enforceUntil, ENFORCE_UNTIL);
    }
    assert.deepEqual(snapshot[1]!.hook, prevHook, "the hook's placement proof survives the mark");

    const state = buildEnforcementState(ctx);
    assert.deepEqual(
      state.items.map((i) => [i.artifactId, i.status, i.enforceUntil]),
      [
        ["r1", "ended", ENFORCE_UNTIL],
        ["h1", "ended", ENFORCE_UNTIL],
      ],
      "the status labels both ended, with the end date, and lists no third row",
    );
  });

  it("a following set that omits all three retracts the converged hook's entry and leaves the hand-written one", () => {
    convergeFirstCycle();
    const expiredSet = [
      rule({ expired: true, enforceUntil: ENFORCE_UNTIL }),
      hook({ expired: true, enforceUntil: ENFORCE_UNTIL }),
      handWrittenHook(),
    ].map((a) => userGlobal(a, home));
    rewriteSnapshot(ctx, expiredSet);
    assert.equal(readEnforcementSnapshot(ctx).length, 2);

    // Cycle 2: the admin removes all three — absence, the ordinary retract path.
    reconcileRemovedArtifacts(ctx, new Set<string>(), new Map());
    rewriteSnapshot(ctx, []);

    const settings = settingsOf(home);
    assert.equal(settings.hooks?.PreToolUse, undefined, "converged hook's entry dropped through the ordinary retract");
    assert.equal(settings.hooks?.UserPromptSubmit?.length, 1, "hand-written entry left alone — no proof it was ours");
    assert.equal(existsSync(join(home, ".claude", "hooks", "measure.sh")), false, "an unmodified script is removed with its artifact");
    assert.equal(readLedgerEntry(ctx, "r1"), null, "removed artifacts are forgotten");
    assert.deepEqual(readEnforcementSnapshot(ctx), []);
    assert.deepEqual(buildEnforcementState(ctx).items, [], "removed artifacts leave the status");
  });

  it("an admin re-enforcing (served unmarked again) clears the mark, also on a deferred entry", () => {
    convergeFirstCycle();
    const marked = rewriteSnapshot(ctx, [userGlobal(rule({ expired: true, enforceUntil: ENFORCE_UNTIL }), home)]);
    assert.equal(marked[0]!.expired, true);
    // The window restarted server-side: the same artifact is served unmarked. A re-derived entry
    // carries no mark, and a deferred entry re-marked from the read loses its stale one.
    const fresh = rewriteSnapshot(ctx, [userGlobal(rule(), home)]);
    assert.equal(fresh[0]!.expired, undefined);
    assert.equal(fresh[0]!.enforceUntil, undefined);
    const stale = withExpiryMark(marked[0]!, rule());
    assert.equal(stale.expired, undefined);
    assert.equal(stale.enforceUntil, undefined);
    assert.equal(buildEnforcementState(ctx).items[0]!.status, "in_sync");
  });

  it("a garbage enforceUntil is dropped at the wire boundary; the status still reads ended", () => {
    convergeFirstCycle();
    const [entry] = rewriteSnapshot(ctx, [userGlobal(rule({ expired: true, enforceUntil: "not a date" }), home)]);
    assert.equal(entry!.expired, true);
    assert.equal(entry!.enforceUntil, undefined);
    assert.deepEqual(
      buildEnforcementState(ctx).items.map((i) => [i.status, i.enforceUntil]),
      [["ended", null]],
    );
  });

  it("a legacy-shaped response without the expiry bits converges as before", () => {
    convergeFirstCycle();
    writeFileSync(join(home, ".claude", "CLAUDE.md"), "my local edit\n");

    // What an un-upgraded server sends: no `expired`, no `enforceUntil` — the wire shape parsed from
    // its JSON, so absence (not `false`) is the case under test.
    const legacy = JSON.parse(JSON.stringify({ artifacts: [rule(), hook()] })) as { artifacts: EnforcedArtifact[] };
    for (const a of legacy.artifacts) assert.equal("expired" in a, false);
    const targeted = legacy.artifacts.map((a) => userGlobal(a, home));

    const bodies = new Map([["f-rule", Buffer.from(RULE_BODY)]]);
    for (const t of targeted) reconcileArtifactFiles(ctx, t.artifact, bodies, new Map(), { markConvergedOnMatch: true });

    assert.equal(readFileSync(join(home, ".claude", "CLAUDE.md"), "utf8"), RULE_BODY, "local edit reverted under ENFORCE");
    assert.equal(readLedgerEntry(ctx, "r1")?.kind, "reverted");
    const snapshot = rewriteSnapshot(ctx, targeted);
    assert.ok(snapshot.every((e) => e.expired === undefined && e.enforceUntil === undefined));
    assert.ok(buildEnforcementState(ctx).items.every((i) => i.status === "in_sync" && i.enforceUntil === null));
  });
});

describe("expiry on the status rollup", () => {
  let dir: string;
  let ctx: EngineContext;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "h-expiry-store-"));
    ctx = testContext(dir);
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  const repoEntry = (targetKey: string, overrides: Partial<SnapshotEntry> = {}): SnapshotEntry => ({
    artifactId: "rr1",
    ideType: "IDE_TYPE_CLAUDE_CODE",
    type: "ARTIFACT_TYPE_RULE",
    name: "Rule",
    scope: SCOPE_REPO_LOCAL,
    targetKey,
    repoFullName: "example-org/example-repo",
    installPath: ".claude/rules/rule.md",
    primaryHash: "abc",
    isHook: false,
    ...overrides,
  });

  it("one stale-marked checkout cannot relabel a live artifact, nor hide its live checkout's failure", () => {
    writeEnforcementSnapshot(ctx, [repoEntry("/a", { expired: true, enforceUntil: ENFORCE_UNTIL }), repoEntry("/b")]);
    recordReconcileFailure(ctx, "rr1", "EACCES", "/b");
    const [item] = buildEnforcementState(ctx).items;
    assert.equal(item!.status, "failed", "the live checkout's state wins over a stale mark");
    assert.equal(item!.enforceUntil, null);
    assert.deepEqual(item!.targets.map((t) => t.status), ["ended", "failed"]);
  });

  it("every checkout marked → ended, with the end date", () => {
    writeEnforcementSnapshot(ctx, [
      repoEntry("/a", { expired: true, enforceUntil: ENFORCE_UNTIL }),
      repoEntry("/b", { expired: true, enforceUntil: ENFORCE_UNTIL }),
    ]);
    const [item] = buildEnforcementState(ctx).items;
    assert.equal(item!.status, "ended");
    assert.equal(item!.enforceUntil, ENFORCE_UNTIL);
  });
});

describe("the tracked-path notice, with real git", () => {
  let dir: string;
  let home: string;
  let repo: string;
  let ctx: EngineContext;
  const RULE_REL = ".claude/rules/credential-guardrail.md";
  const OTHER_RULE_REL = ".claude/rules/review-standard.md";
  const HOOK_REL = ".claude/hooks/run-go-unit-tests.sh";

  function repoRule(overrides: Partial<EnforcedArtifact> = {}): EnforcedArtifact {
    return rule({
      artifactId: "rr1",
      type: "ARTIFACT_TYPE_RULE",
      scope: SCOPE_REPO_LOCAL,
      repoFullName: "example-org/example-repo",
      installPath: RULE_REL,
      ...overrides,
    });
  }

  const repoTarget = (artifact: EnforcedArtifact): TargetedArtifact => ({ artifact, target: { targetKey: repo, root: repo } });

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "h-expiry-git-"));
    home = join(dir, "home");
    mkdirSync(home, { recursive: true });
    ctx = testContext(dir);
    repo = realpathSync(mkdtempSync(join(tmpdir(), "h-expiry-repo-")));
    git(repo, "init", "-q");
    git(repo, "config", "user.email", "dev@example.com");
    git(repo, "config", "user.name", "dev");
    writeFileSync(join(repo, "README.md"), "hello\n");
    git(repo, "add", "-A");
    git(repo, "commit", "-qm", "init");
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
    rmSync(repo, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  it("a converge into a tracked path carries the notice; an untracked path's does not", async () => {
    // The team committed the rule itself; the org now enforces a newer body of it.
    mkdirSync(join(repo, ".claude", "rules"), { recursive: true });
    writeFileSync(join(repo, RULE_REL), "old committed rule\n");
    git(repo, "add", "-f", RULE_REL);
    git(repo, "commit", "-qm", "the team committed the rule");

    const tracked = repoRule();
    const untracked = repoRule({ artifactId: "rr2", installPath: OTHER_RULE_REL, primaryFileId: "f-other", primaryHash: sha256("review\n") });
    const targeted = [repoTarget(tracked), repoTarget(untracked)];
    const gates = await buildWriteGates(targeted);
    assert.equal(gates.get(repo)?.established, true);
    assert.deepEqual(gates.get(repo)?.trackedPaths, new Set([RULE_REL]), "git, not us, says which path is tracked");

    const bodies = new Map([
      ["f-rule", Buffer.from(RULE_BODY)],
      ["f-other", Buffer.from("review\n")],
    ]);
    for (const t of targeted) {
      reconcileArtifactFiles(ctx, t.artifact, bodies, new Map(), {
        markConvergedOnMatch: true,
        trackedPaths: gates.get(repo)?.trackedPaths,
        cleanTrackedPaths: gates.get(repo)?.cleanTrackedPaths,
      }, t.target);
    }

    assert.equal(readLedgerEntry(ctx, "rr1", repo)?.gitTracked, true, "tracked path: written in place, notice recorded");
    assert.equal(readLedgerEntry(ctx, "rr2", repo)?.gitTracked, false, "untracked path: excluded, no notice");
    rewriteSnapshot(ctx, targeted);
    const byId = new Map(buildEnforcementState(ctx).items.map((i) => [i.artifactId, i]));
    assert.equal(byId.get("rr1")?.gitTracked, true);
    assert.equal(byId.get("rr1")?.targets[0]?.gitTracked, true, "attributed to the checkout");
    assert.equal(byId.get("rr2")?.gitTracked, false);

    // Prove the premise the notice states: the tracked write is what the engineer sees in git status.
    const status = git(repo, "status", "--porcelain");
    assert.match(status, new RegExp(`^ M ${RULE_REL}$`, "m"), "tracked change is visible");
    assert.doesNotMatch(status, new RegExp(OTHER_RULE_REL), "untracked write stays excluded");
  });

  it("the notice follows git's current answer: it clears once the path stops being tracked", async () => {
    mkdirSync(join(repo, ".claude", "rules"), { recursive: true });
    writeFileSync(join(repo, RULE_REL), "old committed rule\n");
    git(repo, "add", "-f", RULE_REL);
    git(repo, "commit", "-qm", "the team committed the rule");
    const targeted = [repoTarget(repoRule())];
    let gates = await buildWriteGates(targeted);
    const bodies = new Map([["f-rule", Buffer.from(RULE_BODY)]]);
    reconcileArtifactFiles(ctx, repoRule(), bodies, new Map(), { markConvergedOnMatch: true, trackedPaths: gates.get(repo)?.trackedPaths, cleanTrackedPaths: gates.get(repo)?.cleanTrackedPaths }, targeted[0]!.target);
    assert.equal(readLedgerEntry(ctx, "rr1", repo)?.gitTracked, true);
    const stamp = readLedgerEntry(ctx, "rr1", repo)!.reconciledAt;

    // The team drops the file from the index; the next steady-state cycle re-asks git.
    git(repo, "rm", "-q", "--cached", RULE_REL);
    git(repo, "commit", "-qm", "stop tracking the rule");
    gates = await buildWriteGates(targeted);
    assert.deepEqual(gates.get(repo)?.trackedPaths, new Set());
    reconcileArtifactFiles(ctx, repoRule(), new Map(), new Map(), { markConvergedOnMatch: true, trackedPaths: gates.get(repo)?.trackedPaths }, targeted[0]!.target);
    assert.equal(readLedgerEntry(ctx, "rr1", repo)?.gitTracked, false, "no longer tracked → no notice");
    assert.equal(readLedgerEntry(ctx, "rr1", repo)?.reconciledAt, stamp, "a flag flip is not a new reconciliation");
  });

  it("an expired REPO_LOCAL artifact gets no exclude line and cannot leave its checkout's gate unestablished", async () => {
    // A committed .gitignore negation on the expired hook's path would make the exclude ineffective
    // and the whole root defer — if the expired artifact were still gated. It is not, so the live rule
    // sharing the checkout converges.
    writeFileSync(join(repo, ".gitignore"), `!${HOOK_REL}\n`);
    git(repo, "add", ".gitignore");
    git(repo, "commit", "-qm", "un-ignore the hook path");

    const expiredHook = hook({
      artifactId: "rh1",
      scope: SCOPE_REPO_LOCAL,
      repoFullName: "example-org/example-repo",
      installPath: HOOK_REL,
      hookCommand: `"$CLAUDE_PROJECT_DIR"/${HOOK_REL}`,
      expired: true,
      enforceUntil: ENFORCE_UNTIL,
    });
    const targeted = [repoTarget(expiredHook), repoTarget(repoRule())];
    const gates = await buildWriteGates(targeted);
    assert.equal(gates.get(repo)?.established, true, "the live rule's gate is established");

    const excludePath = join(repo, ".git", "info", "exclude");
    const lines = existsSync(excludePath) ? readFileSync(excludePath, "utf8").split("\n") : [];
    assert.ok(lines.includes(RULE_REL), "the live rule is excluded before its write");
    assert.ok(!lines.includes(HOOK_REL), "no exclude line for the expired hook");
    assert.ok(!lines.includes(".claude/settings.local.json"), "nor for its activation file");
  });
});
