import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { mkdtempSync, mkdirSync, rmSync, writeFileSync, readFileSync, realpathSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import {
  batchPathspecs,
  buildWriteGates,
  gateVerdict,
  gitProbeEnv,
  gitTrackedSubset,
  writePathsFor,
} from "../src/engine/gitGate.js";
import { type TargetedArtifact } from "../src/engine/artifacts.js";
import { type EnforcedArtifact } from "../src/contract.js";

// These tests drive REAL git in a temp repo: the properties under test (does `git status` stay
// clean, does a worktree share the exclude file) are git's behavior, not ours, so a mocked git
// could assert the wrong thing and stay green.

const SCOPE_REPO_LOCAL = "ARTIFACT_SCOPE_REPO_LOCAL";
const SCOPE_USER_GLOBAL = "ARTIFACT_SCOPE_USER_GLOBAL";
const HOOK_REL = ".claude/hooks/run-go-unit-tests.sh";
const RULE_REL = ".claude/rules/agent-guardrail-prevent-credential-leak.md";
const ACTIVATION_REL = ".claude/settings.local.json";

const sha256 = (s: string): string => createHash("sha256").update(s).digest("hex");
const git = (root: string, ...args: string[]): string =>
  execFileSync("git", ["-C", root, ...args], { encoding: "utf8", env: gitProbeEnv() });

function hookArtifact(overrides: Partial<EnforcedArtifact> = {}): EnforcedArtifact {
  return {
    artifactId: "hook1",
    ideType: "IDE_TYPE_CLAUDE_CODE",
    type: "ARTIFACT_TYPE_HOOK",
    name: "Go unit tests",
    scope: SCOPE_REPO_LOCAL,
    repoFullName: "example-org/example-repo",
    installPath: HOOK_REL,
    primaryFileId: "f-hook",
    primaryHash: sha256("#!/bin/bash\n"),
    hookConfig: { event: "PostToolUse", matcher: "Edit", timeoutSeconds: 120 },
    hookCommand: HOOK_REL,
    ...overrides,
  };
}

function ruleArtifact(overrides: Partial<EnforcedArtifact> = {}): EnforcedArtifact {
  return {
    artifactId: "rule1",
    ideType: "IDE_TYPE_CLAUDE_CODE",
    type: "ARTIFACT_TYPE_CLAUDE_MD",
    name: "Credential guardrail",
    scope: SCOPE_REPO_LOCAL,
    repoFullName: "example-org/example-repo",
    installPath: RULE_REL,
    primaryFileId: "f-rule",
    primaryHash: sha256("never print secrets\n"),
    hookCommand: "",
    ...overrides,
  };
}

const target = (root: string): TargetedArtifact["target"] => ({ targetKey: root, root });

describe("write gate — a device-introduced path never lands committable", () => {
  let repo: string;

  beforeEach(() => {
    repo = realpathSync(mkdtempSync(join(tmpdir(), "wg-")));
    git(repo, "init", "-q");
    git(repo, "config", "user.email", "dev@example.com");
    git(repo, "config", "user.name", "dev");
    // Gives the repo a HEAD (the worktree case needs one) and the realistic .claude/ shape.
    mkdirSync(join(repo, ".claude"), { recursive: true });
    // 2-space JSON + trailing newline, the conventional shape AND what our own writer emits — so
    // retracting our entry restores the file byte-for-byte. A repo formatted differently would be
    // left formatting-only modified after the retract; the entry is gone either way.
    writeFileSync(join(repo, ".claude", "settings.json"), `${JSON.stringify({ hooks: {} }, null, 2)}
`);
    git(repo, "add", "-A");
    git(repo, "commit", "-qm", "committed .claude/settings.json");
  });

  afterEach(() => {
    rmSync(repo, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  it("a hook's write paths include the activation file it merges into", async () => {
    // The activation file is a device write like any other, and the one most likely to be
    // tracked — so the gate must ask git about it, not just the artifact's own files.
    assert.deepEqual(writePathsFor(hookArtifact()), [HOOK_REL, ACTIVATION_REL]);
    assert.deepEqual(writePathsFor(ruleArtifact()), [RULE_REL]);
  });

  it("untracked paths are excluded before the write, so git status stays clean", async () => {
    const targeted: TargetedArtifact[] = [
      { artifact: hookArtifact(), target: target(repo) },
      { artifact: ruleArtifact(), target: target(repo) },
    ];
    const gates = await buildWriteGates(targeted);

    assert.equal(gates.get(repo)?.established, true);
    assert.deepEqual(gateVerdict(gates, targeted[0]!), { kind: "write" });
    assert.deepEqual(gateVerdict(gates, targeted[1]!), { kind: "write" });

    const exclude = readFileSync(join(repo, ".git", "info", "exclude"), "utf8");
    for (const rel of [HOOK_REL, RULE_REL, ACTIVATION_REL]) {
      assert.ok(exclude.split("\n").includes(rel), `${rel} excluded`);
    }

    // Now actually place them, as convergence would, and assert git ignores all of it.
    mkdirSync(join(repo, ".claude", "hooks"), { recursive: true });
    mkdirSync(join(repo, ".claude", "rules"), { recursive: true });
    writeFileSync(join(repo, HOOK_REL), "#!/bin/bash\ngo test ./...\n");
    writeFileSync(join(repo, RULE_REL), "never print secrets\n");
    writeFileSync(join(repo, ACTIVATION_REL), '{"hooks":{"PostToolUse":[]}}\n');
    assert.equal(git(repo, "status", "--porcelain"), "", "placed artifacts are invisible to git");

    // `git add -A` must sweep up nothing we placed.
    git(repo, "add", "-A");
    assert.equal(git(repo, "diff", "--cached", "--name-only"), "", "nothing staged by git add -A");
  });

  it("the exclude append is idempotent across cycles", async () => {
    const targeted: TargetedArtifact[] = [{ artifact: hookArtifact(), target: target(repo) }];
    await buildWriteGates(targeted);
    await buildWriteGates(targeted);
    const lines = readFileSync(join(repo, ".git", "info", "exclude"), "utf8").split("\n");
    assert.equal(lines.filter((l) => l === HOOK_REL).length, 1, "one line per path, not one per cycle");
  });

  it("a git-TRACKED write target is written in place, not held back", async () => {
    // The repo already carries this path, so converging it updates a file that is the team's
    // own. Holding it back would leave the artifact permanently unconverged instead.
    writeFileSync(join(repo, ACTIVATION_REL), "{}\n");
    git(repo, "add", "-f", ACTIVATION_REL);
    git(repo, "commit", "-qm", "someone committed the local settings file");

    const targeted: TargetedArtifact[] = [{ artifact: hookArtifact(), target: target(repo) }];
    assert.deepEqual(gateVerdict(await buildWriteGates(targeted), targeted[0]!), { kind: "write" });
  });

  it("a tracked path takes no exclude line, while its untracked siblings still get one", async () => {
    // An exclude line for a tracked path is inert (git applies exclude rules only to untracked
    // paths), so writing one would claim a protection that does not exist.
    writeFileSync(join(repo, ACTIVATION_REL), "{}\n");
    git(repo, "add", "-f", ACTIVATION_REL);
    git(repo, "commit", "-qm", "tracked activation file");

    const targeted: TargetedArtifact[] = [
      { artifact: hookArtifact(), target: target(repo) },
      { artifact: ruleArtifact(), target: target(repo) },
    ];
    const gates = await buildWriteGates(targeted);
    assert.equal(gates.get(repo)?.established, true);
    assert.deepEqual(gateVerdict(gates, targeted[0]!), { kind: "write" });
    assert.deepEqual(gateVerdict(gates, targeted[1]!), { kind: "write" });

    const lines = readFileSync(join(repo, ".git", "info", "exclude"), "utf8").split("\n");
    assert.ok(!lines.includes(ACTIVATION_REL), "no inert exclude line for the tracked path");
    for (const rel of [HOOK_REL, RULE_REL]) {
      assert.ok(lines.includes(rel), `${rel} still excluded`);
    }
  });

  it("an all-tracked root still establishes its gate, so nothing defers on an empty exclude", async () => {
    // Every converging path already tracked means there is nothing to exclude. That is a
    // satisfied gate, not an unestablished one — otherwise the root would defer forever.
    mkdirSync(join(repo, ".claude", "rules"), { recursive: true });
    writeFileSync(join(repo, RULE_REL), "never print secrets\n");
    git(repo, "add", "-f", RULE_REL);
    git(repo, "commit", "-qm", "the team committed the rule itself");

    const targeted: TargetedArtifact[] = [{ artifact: ruleArtifact(), target: target(repo) }];
    const gates = await buildWriteGates(targeted);
    assert.equal(gates.get(repo)?.established, true);
    assert.deepEqual(gateVerdict(gates, targeted[0]!), { kind: "write" });
  });

  it("a committed .gitignore negation outranks the exclude, so the gate is not established", async () => {
    // .gitignore beats .git/info/exclude, so appending a line does not make git blind to the path.
    // The gate must ask git for the effective answer rather than trust its own append.
    writeFileSync(join(repo, ".gitignore"), `!${HOOK_REL}\n`);
    git(repo, "add", ".gitignore");
    git(repo, "commit", "-qm", "un-ignore the hook path");

    const targeted: TargetedArtifact[] = [{ artifact: hookArtifact(), target: target(repo) }];
    const gates = await buildWriteGates(targeted);
    assert.equal(gates.get(repo)?.established, false, "an ineffective exclude is not an established gate");
    assert.deepEqual(gateVerdict(gates, targeted[0]!), { kind: "defer" });

    // Prove the premise: the path really would have stayed visible to git.
    mkdirSync(join(repo, ".claude", "hooks"), { recursive: true });
    writeFileSync(join(repo, HOOK_REL), "#!/bin/bash\n");
    assert.notEqual(git(repo, "status", "--porcelain"), "", "negated path is visible despite the exclude line");
  });

  it("git that cannot answer defers the write instead of assuming untracked", async () => {
    // Fail-closed: an unprovable tracked-state must not be treated as safe to write.
    rmSync(join(repo, ".git", "index"), { force: true });
    mkdirSync(join(repo, ".git", "index")); // an unmappable index → operational git failure

    const targeted: TargetedArtifact[] = [{ artifact: hookArtifact(), target: target(repo) }];
    const gates = await buildWriteGates(targeted);
    assert.equal(gates.get(repo)?.established, false);
    assert.deepEqual(gateVerdict(gates, targeted[0]!), { kind: "defer" });
  });

  it("a linked worktree is covered by the same exclude write", async () => {
    // Convergence targets worktrees too, and a worktree's `.git` is a FILE — `--git-path`
    // resolves to the shared exclude, so one write covers the checkout and its worktrees.
    const wt = realpathSync(mkdtempSync(join(tmpdir(), "wg-wt-")));
    rmSync(wt, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
    git(repo, "worktree", "add", "-q", wt, "-b", "wt");
    try {
      const targeted: TargetedArtifact[] = [{ artifact: hookArtifact(), target: target(wt) }];
      assert.equal((await buildWriteGates(targeted)).get(wt)?.established, true);

      mkdirSync(join(wt, ".claude", "hooks"), { recursive: true });
      writeFileSync(join(wt, HOOK_REL), "#!/bin/bash\n");
      writeFileSync(join(wt, ACTIVATION_REL), "{}\n");
      assert.equal(git(wt, "status", "--porcelain"), "", "worktree stays clean");
    } finally {
      rmSync(wt, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
    }
  });

  it("an inactive hook is not gated on its activation file, so a disabled hook's entry can be retracted", async () => {
    // `inactive` REMOVES the entry, which needs no more permission than a retract does. Gating it
    // on a tracked activation file would strand a disabled hook live forever.
    writeFileSync(join(repo, ACTIVATION_REL), "{}\n");
    git(repo, "add", "-f", ACTIVATION_REL);
    git(repo, "commit", "-qm", "tracked activation file");

    const inactive = hookArtifact({ inactive: true });
    assert.deepEqual(writePathsFor(inactive), [HOOK_REL], "activation file excluded from the gated set");
    const targeted: TargetedArtifact[] = [{ artifact: inactive, target: target(repo) }];
    assert.deepEqual(gateVerdict(await buildWriteGates(targeted), targeted[0]!), { kind: "write" });
  });

  it("a USER_GLOBAL artifact is ungated — it writes under home, not into a shared repo", async () => {
    const t: TargetedArtifact = {
      artifact: hookArtifact({ scope: SCOPE_USER_GLOBAL, repoFullName: "" }),
      target: { targetKey: "", root: repo },
    };
    const gates = await buildWriteGates([t]);
    assert.equal(gates.size, 0, "no gate is built for a user-global root");
    assert.deepEqual(gateVerdict(gates, t), { kind: "write" });
  });
});

describe("gitTrackedSubset — fail-closed tracked-state probe", () => {
  let repo: string;

  beforeEach(() => {
    repo = realpathSync(mkdtempSync(join(tmpdir(), "gts-")));
    git(repo, "init", "-q");
    git(repo, "config", "user.email", "dev@example.com");
    git(repo, "config", "user.name", "dev");
  });

  afterEach(() => {
    rmSync(repo, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  it("returns exactly the tracked subset", async () => {
    mkdirSync(join(repo, ".claude"), { recursive: true });
    writeFileSync(join(repo, ".claude", "settings.json"), "{}\n");
    writeFileSync(join(repo, ".claude", "untracked.md"), "x\n");
    git(repo, "add", ".claude/settings.json");
    git(repo, "commit", "-qm", "one tracked file");

    const got = await gitTrackedSubset(repo, [".claude/settings.json", ".claude/untracked.md", ".claude/absent.md"]);
    assert.deepEqual(got, new Set([".claude/settings.json"]));
  });

  it("an empty path list needs no git spawn", async () => {
    assert.deepEqual(await gitTrackedSubset(repo, []), new Set());
  });

  it("a non-git directory tracks nothing (empty set, not null)", async () => {
    // There is no git there to carry a file into a commit, so writing is safe.
    const plain = realpathSync(mkdtempSync(join(tmpdir(), "plain-")));
    try {
      assert.deepEqual(await gitTrackedSubset(plain, ["a.md"]), new Set());
    } finally {
      rmSync(plain, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
    }
  });

  it("an operational git failure is null — unprovable, never assumed untracked", async () => {
    rmSync(join(repo, ".git", "index"), { force: true });
    mkdirSync(join(repo, ".git", "index"));
    assert.equal(await gitTrackedSubset(repo, ["a.md"]), null);
  });

  it("an inherited GIT_INDEX_FILE cannot make a tracked path read as untracked", async () => {
    // A repository-local git env var points git at a DIFFERENT repo, and the failure is silent:
    // `ls-files` exits 0 with empty output, which would read as "untracked" and authorize a write
    // into a tracked file. The probe env scrubs these, so the answer is about `root` only.
    mkdirSync(join(repo, ".claude"), { recursive: true });
    writeFileSync(join(repo, ".claude", "settings.json"), "{}\n");
    git(repo, "add", ".claude/settings.json");
    git(repo, "commit", "-qm", "tracked file");

    const other = realpathSync(mkdtempSync(join(tmpdir(), "gts-other-")));
    git(other, "init", "-q");
    const saved = process.env.GIT_INDEX_FILE;
    process.env.GIT_INDEX_FILE = join(other, ".git", "index"); // another repo's (empty) index
    try {
      const got = await gitTrackedSubset(repo, [".claude/settings.json"]);
      assert.deepEqual(got, new Set([".claude/settings.json"]), "answers about root, not the hijacked index");
    } finally {
      if (saved === undefined) delete process.env.GIT_INDEX_FILE;
      else process.env.GIT_INDEX_FILE = saved;
      rmSync(other, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
    }
  });

  it("a path set far past one argv survives, batched, without a false negative", async () => {
    // At the backend ceiling (~100 artifacts x ~100 files) one checkout's path set is thousands of
    // entries; spread into a single argv that is E2BIG, which is fail-closed here and would defer
    // the whole checkout forever. Batching must keep the tracked path visible among the noise.
    mkdirSync(join(repo, ".claude"), { recursive: true });
    writeFileSync(join(repo, ".claude", "settings.json"), "{}\n");
    git(repo, "add", ".claude/settings.json");
    git(repo, "commit", "-qm", "tracked file");

    const many = Array.from({ length: 4000 }, (_, i) => `.claude/skills/generated-skill-${i}/SKILL.md`);
    const got = await gitTrackedSubset(repo, [...many, ".claude/settings.json"]);
    assert.deepEqual(got, new Set([".claude/settings.json"]));
  });
});

describe("batchPathspecs — argv budgeting", () => {
  it("keeps every path exactly once, in order, across batches", () => {
    const paths = Array.from({ length: 500 }, (_, i) => `.claude/rules/rule-${i}.md`);
    const batches = batchPathspecs(paths, 1024);
    assert.ok(batches.length > 1, "a 1 KiB budget must split 500 paths");
    assert.deepEqual(batches.flat(), paths, "no path dropped, duplicated, or reordered");
    for (const batch of batches) {
      const bytes = batch.reduce((n, path) => n + Buffer.byteLength(path, "utf8") + 1, 0);
      const first = Buffer.byteLength(batch[0]!, "utf8") + 1;
      assert.ok(bytes <= 1024 || batch.length === 1, `batch within budget (${bytes}) or a single oversized path (${first})`);
    }
  });

  it("an empty list is no batches, and one oversized path still gets its own batch", () => {
    assert.deepEqual(batchPathspecs([], 1024), []);
    const huge = "x".repeat(4096);
    assert.deepEqual(batchPathspecs([huge], 1024), [[huge]], "never silently dropped");
  });
});
