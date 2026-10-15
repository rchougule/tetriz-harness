import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, utimesSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { createAgent } from "../src/agent.js";
import type { DownloadRef, EnforcedArtifact, EnforcedSet, StateReport } from "../src/contract.js";
import { MemoryStore } from "../src/store/memoryStore.js";
import { gitProbeEnv } from "../src/engine/gitGate.js";
import type { Transport } from "../src/ports.js";
import { sha256, testContext } from "./helpers.js";

const RULE = "# org rules\n";
const SKILL = "---\nname: review\n---\nreview\n";
const HELPER = "#!/bin/sh\necho help\n";
const SCRIPT = "#!/bin/sh\necho guard\n";
const SKILL_DIR = ".claude/skills/review";

function rule(): EnforcedArtifact {
  return {
    artifactId: "r1",
    ideType: "IDE_TYPE_CLAUDE_CODE",
    type: "ARTIFACT_TYPE_CLAUDE_MD",
    name: "Org rules",
    scope: "ARTIFACT_SCOPE_USER_GLOBAL",
    repoFullName: "",
    installPath: ".claude/CLAUDE.md",
    primaryFileId: sha256(RULE),
    primaryHash: sha256(RULE),
    hookCommand: "",
  };
}

function skill(): EnforcedArtifact {
  return {
    artifactId: "s1",
    ideType: "IDE_TYPE_CLAUDE_CODE",
    type: "ARTIFACT_TYPE_SKILL",
    name: "Review",
    scope: "ARTIFACT_SCOPE_USER_GLOBAL",
    repoFullName: "",
    installPath: `${SKILL_DIR}/SKILL.md`,
    primaryFileId: sha256(SKILL),
    primaryHash: sha256(SKILL),
    hookCommand: "",
    files: [
      { relativePath: `${SKILL_DIR}/SKILL.md`, fileId: sha256(SKILL), hash: sha256(SKILL), executable: false },
      { relativePath: `${SKILL_DIR}/run.sh`, fileId: sha256(HELPER), hash: sha256(HELPER), executable: false },
    ],
  };
}

function hook(): EnforcedArtifact {
  return {
    artifactId: "h1",
    ideType: "IDE_TYPE_CLAUDE_CODE",
    type: "ARTIFACT_TYPE_HOOK",
    name: "Guard",
    scope: "ARTIFACT_SCOPE_USER_GLOBAL",
    repoFullName: "",
    installPath: ".claude/hooks/guard.sh",
    primaryFileId: sha256(SCRIPT),
    primaryHash: sha256(SCRIPT),
    hookConfig: { event: "PreToolUse", matcher: "Bash", timeoutSeconds: 0 },
    hookCommand: '"$HOME"/.claude/hooks/guard.sh',
  };
}

class FakeTransport implements Transport {
  set: EnforcedSet = { artifacts: [] };
  reports: StateReport[] = [];
  private readonly bodies = new Map([RULE, SKILL, HELPER, SCRIPT].map((b) => [sha256(b), b]));

  async getEnforcedSet(): Promise<EnforcedSet> {
    return structuredClone(this.set);
  }

  async getDownloads(ids: string[]): Promise<DownloadRef[]> {
    return ids.flatMap((fileId) => {
      const b = this.bodies.get(fileId);
      return b === undefined ? [] : [{ fileId, bytes: new TextEncoder().encode(b) }];
    });
  }

  async report(state: StateReport): Promise<void> {
    this.reports.push(structuredClone(state));
  }

  last(): StateReport {
    return this.reports.at(-1)!;
  }
}

describe("retraction removes what this agent placed, and only that", () => {
  let dir: string;
  let home: string;
  let transport: FakeTransport;
  let store: MemoryStore;
  const make = () => createAgent({ transport, store, roots: { ...testContext(dir).paths, discoverProjects: false } });

  /** A retract is held one pass by the floor; the second read without the artifact confirms it. */
  async function retract(agent: ReturnType<typeof make>, remaining: EnforcedArtifact[]) {
    transport.set = { artifacts: remaining };
    await agent.sync();
    return agent.sync();
  }

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "retract-"));
    home = join(dir, "home");
    mkdirSync(home, { recursive: true });
    transport = new FakeTransport();
    store = new MemoryStore();
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  it("an unmodified file is deleted and drops out of the report", async () => {
    transport.set = { artifacts: [rule()] };
    const agent = make();
    await agent.sync();
    assert.equal(transport.last().installed.length, 1);

    const result = await retract(agent, []);

    assert.equal(existsSync(join(home, ".claude", "CLAUDE.md")), false);
    assert.deepEqual(transport.last().installed, []);
    assert.deepEqual(transport.last().applyFailures, []);
    assert.deepEqual(result.retained, []);
  });

  it("the floor holds the deletion on the first read without the artifact", async () => {
    transport.set = { artifacts: [rule()] };
    const agent = make();
    await agent.sync();
    transport.set = { artifacts: [] };
    await agent.sync();
    assert.equal(existsSync(join(home, ".claude", "CLAUDE.md")), true, "held one pass");
  });

  it("a locally modified file is left in place and reported with a reason on every pass", async () => {
    transport.set = { artifacts: [rule()] };
    const agent = make();
    await agent.sync();
    const path = join(home, ".claude", "CLAUDE.md");
    writeFileSync(path, "# my own edits\n");

    const result = await retract(agent, []);

    assert.equal(readFileSync(path, "utf8"), "# my own edits\n");
    assert.deepEqual(result.applyFailures, [], "not an apply failure: sync still exits clean");
    assert.deepEqual(result.retained.map((r) => [r.artifactId, r.sourcePath]), [["r1", ".claude/CLAUDE.md"]]);
    const report = transport.last();
    assert.deepEqual(report.installed, [
      { ide: "IDE_TYPE_CLAUDE_CODE", repo: "", checkout: "", sourcePath: ".claude/CLAUDE.md", contentHash: sha256("# my own edits\n") },
    ]);
    assert.equal(report.applyFailures.length, 1);
    assert.equal(report.applyFailures[0]!.artifactId, "r1");
    assert.match(report.applyFailures[0]!.reason, /modified since installed/);

    await agent.sync();
    assert.equal(transport.last().installed.length, 1, "still reported on a later pass");
  });

  it("a retained file the engineer deletes drops out; one restored to the installed bytes is removed", async () => {
    transport.set = { artifacts: [rule(), skill()] };
    const agent = make();
    await agent.sync();
    const rulePath = join(home, ".claude", "CLAUDE.md");
    const skillPath = join(home, SKILL_DIR, "SKILL.md");
    writeFileSync(rulePath, "edited\n");
    writeFileSync(skillPath, "edited\n");
    await retract(agent, []);
    assert.equal(transport.last().installed.length, 2);

    rmSync(rulePath);
    writeFileSync(skillPath, SKILL);
    const result = await agent.sync();

    assert.equal(existsSync(skillPath), false, "back to what was installed, so it is ours to remove");
    assert.deepEqual(transport.last().installed, []);
    assert.deepEqual(result.retained, []);
  });

  it("a skill loses its unmodified files and keeps a modified helper", async () => {
    transport.set = { artifacts: [skill()] };
    const agent = make();
    await agent.sync();
    const helper = join(home, SKILL_DIR, "run.sh");
    writeFileSync(helper, "#!/bin/sh\necho mine\n");

    await retract(agent, []);

    assert.equal(existsSync(join(home, SKILL_DIR, "SKILL.md")), false);
    assert.equal(readFileSync(helper, "utf8"), "#!/bin/sh\necho mine\n");
    assert.deepEqual(transport.last().installed.map((i) => i.sourcePath), [`${SKILL_DIR}/run.sh`]);
  });

  it("a hook loses its script and its settings entry", async () => {
    transport.set = { artifacts: [hook()] };
    const agent = make();
    await agent.sync();
    const settings = join(home, ".claude", "settings.json");
    assert.equal(JSON.parse(readFileSync(settings, "utf8")).hooks.PreToolUse.length, 1);

    await retract(agent, []);

    assert.equal(existsSync(join(home, ".claude", "hooks", "guard.sh")), false);
    assert.equal(JSON.parse(readFileSync(settings, "utf8")).hooks?.PreToolUse, undefined);
  });

  it("a re-enforced artifact takes its retained file back over", async () => {
    transport.set = { artifacts: [rule()] };
    const agent = make();
    await agent.sync();
    const path = join(home, ".claude", "CLAUDE.md");
    writeFileSync(path, "edited\n");
    await retract(agent, []);

    transport.set = { artifacts: [rule()] };
    const result = await agent.sync();

    assert.equal(readFileSync(path, "utf8"), RULE);
    assert.deepEqual(result.retained, []);
    assert.deepEqual(transport.last().applyFailures, []);
    assert.equal(transport.last().installed[0]!.contentHash, sha256(RULE));
  });

  it("a live session holds the deletion until it ends", async () => {
    const projects = join(dir, "claude-projects");
    const transcript = join(projects, "p", "session.jsonl");
    mkdirSync(join(projects, "p"), { recursive: true });
    writeFileSync(transcript, "{}\n");
    const old = new Date(Date.now() - 60 * 60 * 1000);
    utimesSync(transcript, old, old);
    const agent = createAgent({
      transport,
      store,
      roots: { ...testContext(dir).paths, claudeProjects: projects, discoverProjects: false },
    });
    transport.set = { artifacts: [rule()] };
    await agent.sync();

    utimesSync(transcript, new Date(), new Date());
    await retract(agent, []);
    assert.equal(existsSync(join(home, ".claude", "CLAUDE.md")), true, "held while a session is live");
    assert.equal(transport.last().installed.length, 1);

    utimesSync(transcript, old, old);
    await agent.sync();
    assert.equal(existsSync(join(home, ".claude", "CLAUDE.md")), false);
  });
});

describe("retraction leaves no folder or git exclude line of its own behind", () => {
  let dir: string;
  let home: string;
  let transport: FakeTransport;
  let store: MemoryStore;
  const make = (projects: string[] = []) =>
    createAgent({ transport, store, roots: { ...testContext(dir).paths, projects, discoverProjects: false } });

  async function retract(agent: ReturnType<typeof make>, remaining: EnforcedArtifact[]) {
    transport.set = { artifacts: remaining };
    await agent.sync();
    return agent.sync();
  }

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "retract-tidy-"));
    home = join(dir, "home");
    mkdirSync(home, { recursive: true });
    transport = new FakeTransport();
    store = new MemoryStore();
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  it("removes the folders it created for a skill, and keeps the tool's root", async () => {
    transport.set = { artifacts: [skill()] };
    const agent = make();
    await agent.sync();
    assert.equal(existsSync(join(home, SKILL_DIR, "SKILL.md")), true);

    await retract(agent, []);

    assert.equal(existsSync(join(home, ".claude", "skills")), false, "skills/ and skills/review/ were ours");
    assert.equal(existsSync(join(home, ".claude")), true, "the tool root is never removed");
  });

  it("keeps a folder that existed before it wrote there", async () => {
    mkdirSync(join(home, ".claude", "skills"), { recursive: true });
    transport.set = { artifacts: [skill()] };
    const agent = make();
    await agent.sync();

    await retract(agent, []);

    assert.equal(existsSync(join(home, SKILL_DIR)), false);
    assert.equal(existsSync(join(home, ".claude", "skills")), true, "not created by the agent");
  });

  it("keeps the folder of a file left in place, and removes it once that file is removed", async () => {
    transport.set = { artifacts: [skill()] };
    const agent = make();
    await agent.sync();
    const helper = join(home, SKILL_DIR, "run.sh");
    writeFileSync(helper, "#!/bin/sh\necho mine\n");

    await retract(agent, []);
    assert.equal(existsSync(helper), true);

    writeFileSync(helper, HELPER);
    await agent.sync();
    assert.equal(existsSync(join(home, ".claude", "skills")), false);
    assert.equal(existsSync(join(home, ".claude")), true);
  });

  describe("in a checkout", () => {
    let repo: string;
    const git = (...args: string[]): string =>
      execFileSync("git", ["-C", repo, ...args], { encoding: "utf8", env: gitProbeEnv() });
    const exclude = (): string => readFileSync(join(repo, ".git", "info", "exclude"), "utf8");
    const repoRule = (id: string, installPath: string): EnforcedArtifact => ({
      ...rule(),
      artifactId: id,
      type: "ARTIFACT_TYPE_RULE",
      scope: "ARTIFACT_SCOPE_REPO_LOCAL",
      repoFullName: "example-org/example-repo",
      installPath,
    });

    beforeEach(() => {
      repo = realpathSync(mkdtempSync(join(tmpdir(), "retract-tidy-checkout-")));
      git("init", "-q");
      git("remote", "add", "origin", "git@git.example.com:example-org/example-repo.git");
      mkdirSync(join(repo, ".git", "info"), { recursive: true });
      writeFileSync(join(repo, ".git", "info", "exclude"), "# mine\n*.log\n");
    });

    afterEach(() => {
      rmSync(repo, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
    });

    it("removes its exclude lines and folders, and keeps the engineer's lines", async () => {
      transport.set = { artifacts: [repoRule("rr1", ".claude/rules/team/org.md"), repoRule("rr2", ".claude/rules/keep.md")] };
      const agent = make([repo]);
      await agent.sync();
      assert.match(exclude(), /^\.claude\/rules\/team\/org\.md$/m);

      await retract(agent, [repoRule("rr2", ".claude/rules/keep.md")]);

      assert.equal(existsSync(join(repo, ".claude", "rules", "team")), false);
      assert.equal(existsSync(join(repo, ".claude", "rules", "keep.md")), true);
      assert.equal(exclude(), "# mine\n*.log\n.claude/rules/keep.md\n", "only the retracted artifact's line goes");
      assert.equal(git("status", "--porcelain"), "");
    });

    it("keeps the exclude line of a file left in place, and drops it once the file is gone", async () => {
      transport.set = { artifacts: [repoRule("rr1", ".claude/rules/org.md")] };
      const agent = make([repo]);
      await agent.sync();
      const placed = join(repo, ".claude", "rules", "org.md");
      writeFileSync(placed, "# my own edits\n");

      await retract(agent, []);
      assert.equal(existsSync(placed), true);
      assert.match(exclude(), /^\.claude\/rules\/org\.md$/m, "an edited file stays hidden from git");

      rmSync(placed);
      await agent.sync();
      assert.equal(exclude(), "# mine\n*.log\n");
      assert.equal(existsSync(join(repo, ".claude", "rules")), false);
      assert.equal(existsSync(join(repo, ".claude")), true);
    });
  });
});
