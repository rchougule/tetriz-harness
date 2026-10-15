import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync, readFileSync, existsSync, realpathSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import type { EngineContext } from "../src/engine/context.js";
import { testContext } from "./helpers.js";
import {
  reconcileArtifactFiles,
  pruneDroppedFiles,
  toSnapshotEntry,
} from "../src/engine/converge.js";
import { type TargetedArtifact, type ConvergenceTarget } from "../src/engine/artifacts.js";
import { type EnforcedArtifact } from "../src/contract.js";
import {
  buildEnforcementState,
  readLedgerEntry,
  recordReconcileSuccess,
  writeEnforcementSnapshot,
  type SnapshotEntry,
} from "../src/engine/state.js";
import {
  canonicalRepoKey,
  checkoutsForRepo,
  discoverProjectRoots,
  parseRepoName,
  resolveRepoCheckouts,
  stripRemoteCredentials,
  type GitRunner,
} from "../src/engine/repoCheckouts.js";

// REPO_LOCAL convergence: a repo-local artifact fans out to one target per local
// checkout, and every per-target state (ledger/failure/snapshot/prune) keys on
// (artifactId, targetKey = checkout top-level). These tests point the live-session
// probes at nonexistent dirs so hasLiveSession fails open (see testContext).

const SCOPE_REPO_LOCAL = "ARTIFACT_SCOPE_REPO_LOCAL";
const RULE_PATH = ".claude/rules/org-rule.md";
const RULE_BODY = "# org rule\nalways do X\n";

function sha256(s: string): string {
  return createHash("sha256").update(s).digest("hex");
}

function repoLocalArtifact(overrides: Partial<EnforcedArtifact> = {}): EnforcedArtifact {
  return {
    artifactId: "rl1",
    ideType: "IDE_TYPE_CLAUDE_CODE",
    type: "ARTIFACT_TYPE_CLAUDE_MD",
    name: "Org rule",
    scope: SCOPE_REPO_LOCAL,
    repoFullName: "example-org/example-repo",
    installPath: RULE_PATH,
    primaryFileId: "f1",
    primaryHash: sha256(RULE_BODY),
    hookCommand: "",
    ...overrides,
  };
}

const repoTarget = (root: string): ConvergenceTarget => ({ targetKey: root, root });

describe("REPO_LOCAL convergence — per-checkout keying, isolation, and prune", () => {
  let dir: string;
  let home: string;
  let checkoutA: string;
  let checkoutB: string;
  let ctx: EngineContext;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "h-repolocal-"));
    home = join(dir, "home");
    checkoutA = join(dir, "checkout-a");
    checkoutB = join(dir, "checkout-b");
    mkdirSync(home, { recursive: true });
    mkdirSync(checkoutA, { recursive: true });
    mkdirSync(checkoutB, { recursive: true });
    ctx = testContext(dir);

  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  it("writes the artifact into EVERY resolved checkout, under each checkout's root", () => {
    const a = repoLocalArtifact();
    const bodies = new Map([["f1", Buffer.from(RULE_BODY)]]);

    reconcileArtifactFiles(ctx, a, bodies, new Map(), { markConvergedOnMatch: true }, repoTarget(checkoutA));
    reconcileArtifactFiles(ctx, a, bodies, new Map(), { markConvergedOnMatch: true }, repoTarget(checkoutB));

    assert.equal(readFileSync(join(checkoutA, RULE_PATH), "utf8"), RULE_BODY);
    assert.equal(readFileSync(join(checkoutB, RULE_PATH), "utf8"), RULE_BODY);
    // Never leaks into the home tree — a repo-local artifact is checkout-scoped.
    assert.equal(existsSync(join(home, RULE_PATH)), false);
  });

  it("per-target ledger/failure isolation: checkout A converges, checkout B fails — no clobber", () => {
    const a = repoLocalArtifact();
    // A gets a verified body → converges.
    reconcileArtifactFiles(ctx, a, new Map([["f1", Buffer.from(RULE_BODY)]]), new Map(), {}, repoTarget(checkoutA));
    // B gets no body with a real integrity failure → recorded failed, nothing written.
    reconcileArtifactFiles(ctx, a, new Map(), new Map([["f1", "hash"]]), {}, repoTarget(checkoutB));

    // The two targets keep independent ledger state under the same artifactId.
    const ledgerA = readLedgerEntry(ctx, "rl1", checkoutA);
    const ledgerB = readLedgerEntry(ctx, "rl1", checkoutB);
    assert.equal(ledgerA?.appliedHash, a.primaryHash, "A recorded converged");
    assert.equal(ledgerB, null, "B never recorded a success");

    // File landed in A only.
    assert.equal(existsSync(join(checkoutA, RULE_PATH)), true);
    assert.equal(existsSync(join(checkoutB, RULE_PATH)), false);

    // Roll-up view: one status row for the artifact, worst status (failed, from B) wins,
    // and the newest-reconciled target (A) is the representative for kind.
    writeEnforcementSnapshot(ctx, [
      toSnapshotEntry(ctx, a, [], repoTarget(checkoutA)),
      toSnapshotEntry(ctx, a, [], repoTarget(checkoutB)),
    ]);
    const state = buildEnforcementState(ctx);
    assert.equal(state.items.length, 1);
    assert.equal(state.items[0]!.artifactId, "rl1");
    assert.equal(state.items[0]!.status, "failed");
    assert.equal(state.items[0]!.lastReconcileKind, "installed");
    assert.match(state.items[0]!.failureReason!, /hash/);
  });

  it("recorded ledger entries under different checkouts do not overwrite one another", () => {
    recordReconcileSuccess(ctx, "rl1", {
      name: "Org rule", ideType: "IDE_TYPE_CLAUDE_CODE", installPath: RULE_PATH,
      appliedHash: "hash-A", kind: "installed", previousPreview: null, newPreview: "A body",
    }, checkoutA);
    recordReconcileSuccess(ctx, "rl1", {
      name: "Org rule", ideType: "IDE_TYPE_CLAUDE_CODE", installPath: RULE_PATH,
      appliedHash: "hash-B", kind: "updated", previousPreview: null, newPreview: "B body",
    }, checkoutB);

    assert.equal(readLedgerEntry(ctx, "rl1", checkoutA)?.appliedHash, "hash-A");
    assert.equal(readLedgerEntry(ctx, "rl1", checkoutB)?.appliedHash, "hash-B");
    // The USER_GLOBAL sentinel key is a third, independent slot — untouched here.
    assert.equal(readLedgerEntry(ctx, "rl1"), null);
  });

  it("per-target prune: a helper dropped from checkout A only is pruned from A, kept in B", () => {
    const PRIMARY_PATH = ".claude/skills/review/SKILL.md";
    const HELPER_PATH = ".claude/skills/review/helper.py";
    const PRIMARY = "# skill\n";
    const HELPER = "print('h')\n";

    for (const co of [checkoutA, checkoutB]) {
      mkdirSync(join(co, ".claude", "skills", "review"), { recursive: true });
      writeFileSync(join(co, PRIMARY_PATH), PRIMARY);
      writeFileSync(join(co, HELPER_PATH), HELPER);
    }

    const skill = (files: EnforcedArtifact["files"]): EnforcedArtifact =>
      repoLocalArtifact({
        artifactId: "sk1",
        type: "ARTIFACT_TYPE_SKILL",
        name: "Review",
        installPath: PRIMARY_PATH,
        primaryFileId: "f-primary",
        primaryHash: sha256(PRIMARY),
        files,
      });

    // Previous snapshot: both checkouts placed SKILL.md + helper.py.
    const prevEntry = (root: string): SnapshotEntry => ({
      artifactId: "sk1",
      ideType: "IDE_TYPE_CLAUDE_CODE",
      type: "ARTIFACT_TYPE_SKILL",
      name: "Review",
      scope: SCOPE_REPO_LOCAL,
      targetKey: root,
      installPath: PRIMARY_PATH,
      primaryHash: sha256(PRIMARY),
      isHook: false,
      placedPaths: [PRIMARY_PATH, HELPER_PATH],
      placedHashes: { [PRIMARY_PATH]: sha256(PRIMARY), [HELPER_PATH]: sha256(HELPER) },
    });
    writeEnforcementSnapshot(ctx, [prevEntry(checkoutA), prevEntry(checkoutB)]);

    // Current: A dropped the helper; B still enforces both.
    const droppedA = skill([{ relativePath: PRIMARY_PATH, fileId: "f-primary", hash: sha256(PRIMARY), executable: false }]);
    const fullB = skill([
      { relativePath: PRIMARY_PATH, fileId: "f-primary", hash: sha256(PRIMARY), executable: false },
      { relativePath: HELPER_PATH, fileId: "f-helper", hash: sha256(HELPER), executable: false },
    ]);
    const current: TargetedArtifact[] = [
      { artifact: droppedA, target: repoTarget(checkoutA) },
      { artifact: fullB, target: repoTarget(checkoutB) },
    ];
    pruneDroppedFiles(ctx, current);

    assert.equal(existsSync(join(checkoutA, HELPER_PATH)), false, "helper pruned from A");
    assert.equal(existsSync(join(checkoutB, HELPER_PATH)), true, "helper kept in B");
    assert.equal(existsSync(join(checkoutA, PRIMARY_PATH)), true, "primary kept in A");
    assert.equal(existsSync(join(checkoutB, PRIMARY_PATH)), true, "primary kept in B");
  });

  it("backward-compat: an old snapshot entry with no targetKey reads as the USER_GLOBAL sentinel", () => {
    // Snapshot written by a pre-REPO_LOCAL build: no targetKey field.
    const oldEntry: SnapshotEntry = {
      artifactId: "ug1",
      ideType: "IDE_TYPE_CLAUDE_CODE",
      type: "ARTIFACT_TYPE_CLAUDE_MD",
      name: "Org CLAUDE.md",
      scope: "ARTIFACT_SCOPE_USER_GLOBAL",
      installPath: ".claude/CLAUDE.md",
      primaryHash: "hash-v1",
      isHook: false,
    };
    writeEnforcementSnapshot(ctx, [oldEntry]);
    // A ledger row written by the old build is keyed by the bare artifactId — which
    // IS compositeKey(id, "") — so the default-targetKey read must find it.
    recordReconcileSuccess(ctx, "ug1", {
      name: "Org CLAUDE.md", ideType: "IDE_TYPE_CLAUDE_CODE", installPath: ".claude/CLAUDE.md",
      appliedHash: "hash-v1", kind: "installed", previousPreview: null, newPreview: "v1 body",
    });

    const state = buildEnforcementState(ctx);
    assert.equal(state.items.length, 1);
    assert.equal(state.items[0]!.status, "in_sync");
    assert.equal(state.items[0]!.lastReconcileKind, "installed");
  });
});

describe("resolveRepoCheckouts — repo → every local checkout (worktrees included)", () => {
  let dir: string;
  let home: string;
  const projectsDir = (): string => join(home, ".claude", "projects");
  // Force the filesystem fallback: deterministic, no host git.
  const noGit: GitRunner = () => Promise.resolve(null);
  const resolveFromSessions = async (): Promise<Map<string, string[]>> => {
    const paths = { ...testContext(dir).paths, claudeProjects: projectsDir() };
    return resolveRepoCheckouts(await discoverProjectRoots(paths), { git: noGit });
  };

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "h-resolver-"));
    home = join(dir, "home");
    mkdirSync(home, { recursive: true });
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  function makeCheckout(name: string, remote: string): string {
    const co = join(dir, name);
    mkdirSync(join(co, ".git"), { recursive: true });
    writeFileSync(join(co, ".git", "config"), `[remote "origin"]\n\turl = ${remote}\n`);
    return realpathSync(co);
  }

  function seedClaudeProject(name: string, cwd: string): void {
    const pdir = join(projectsDir(), name);
    mkdirSync(pdir, { recursive: true });
    writeFileSync(join(pdir, "session.jsonl"), `${JSON.stringify({ cwd })}\n`);
  }

  it("maps two clones of one repo to both checkout top-levels", async () => {
    const a = makeCheckout("clone-a", "git@github.com:example-org/example-repo.git");
    const b = makeCheckout("clone-b", "https://github.com/example-org/example-repo.git");
    seedClaudeProject("proj-a", a);
    seedClaudeProject("proj-b", b);

    const map = await resolveFromSessions();
    const key = canonicalRepoKey("example-org/example-repo")!;
    assert.deepEqual(map.get(key), [a, b].sort());

    // checkoutsForRepo canonicalizes both sides, so a case/host variant still matches.
    assert.deepEqual(checkoutsForRepo(map, "Example-Org/Example-Repo").sort(), [a, b].sort());
    assert.deepEqual(checkoutsForRepo(map, "example-org/other-repo"), []);
  });

  it("dedupes two cwds under one checkout to a single top-level", async () => {
    const a = makeCheckout("clone-a", "git@github.com:example-org/example-repo.git");
    // Two Claude project cwds: the checkout root and a subdir under it — both
    // resolve to the same git top-level, so the checkout appears once.
    const sub = join(a, "packages", "core");
    mkdirSync(sub, { recursive: true });
    seedClaudeProject("proj-root", a);
    seedClaudeProject("proj-sub", sub);

    const map = await resolveFromSessions();
    assert.deepEqual(map.get(canonicalRepoKey("example-org/example-repo")!), [a]);
  });
});

describe("discoverProjectRoots and repo identity helpers", () => {
  let dir: string;

  beforeEach(() => {
    dir = realpathSync(mkdtempSync(join(tmpdir(), "h-discover-")));
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  it("reads Codex rollout heads and puts explicit projects first", async () => {
    const work = join(dir, "work");
    mkdirSync(work, { recursive: true });
    const day = join(dir, "codex", "sessions", "2026", "09", "01");
    mkdirSync(day, { recursive: true });
    writeFileSync(join(day, "rollout-2026-09-01T10-00-00-a.jsonl"), `${JSON.stringify({ type: "session_meta", payload: { cwd: work } })}\n`);
    writeFileSync(join(day, "rollout-2026-09-01T11-00-00-b.jsonl"), `${JSON.stringify({ type: "other", payload: { cwd: "/nope" } })}\n`);
    const paths = { ...testContext(dir).paths, codexHome: join(dir, "codex") };

    assert.deepEqual(await discoverProjectRoots(paths, ["/explicit"]), ["/explicit", work].sort());
    assert.deepEqual(await discoverProjectRoots(paths, ["/explicit"], { sessions: false }), ["/explicit"]);
  });

  it("strips credentials from a remote and reduces it to org/repo", () => {
    assert.equal(stripRemoteCredentials("https://user:secret@git.example.com/example-org/example-repo.git"), "https://git.example.com/example-org/example-repo.git");
    assert.equal(stripRemoteCredentials("git@git.example.com:example-org/example-repo.git"), "git@git.example.com:example-org/example-repo.git");
    assert.equal(parseRepoName("ssh://git@git.example.com/prefix/example-org/example-repo.git"), "example-org/example-repo");
    assert.equal(parseRepoName("not a url"), undefined);
  });
});
