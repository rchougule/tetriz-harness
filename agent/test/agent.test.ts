import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { createAgent, type AgentOptions } from "../src/agent.js";
import type { DownloadRef, EnforcedArtifact, EnforcedSet, StateReport } from "../src/contract.js";
import { gitProbeEnv } from "../src/engine/gitGate.js";
import { MemoryStore } from "../src/store/memoryStore.js";
import type { Transport } from "../src/ports.js";
import { sha256, testContext } from "./helpers.js";
import { AGENT_VERSION } from "../src/version.js";

const RULE_BODY = "# org rules\nbe careful\n";
const HOOK_SCRIPT = "#!/bin/sh\necho guard\n";
const HOOK_COMMAND = '"$HOME"/.claude/hooks/guard.sh';

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

function hook(): EnforcedArtifact {
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
    hookConfig: { event: "PreToolUse", matcher: "Bash", timeoutSeconds: 30 },
    hookCommand: HOOK_COMMAND,
  };
}

/** An in-memory control plane: serves `set`, inlines `bodies`, records every call. */
class FakeTransport implements Transport {
  set: EnforcedSet = { artifacts: [] };
  bodies = new Map<string, string>([
    ["f-rule", RULE_BODY],
    ["f-hook", HOOK_SCRIPT],
  ]);
  failRead: Error | null = null;
  failDownloads: Error | null = null;
  failReport: Error | null = null;
  reads = 0;
  downloadCalls: string[][] = [];
  reports: StateReport[] = [];

  async getEnforcedSet(): Promise<EnforcedSet> {
    this.reads++;
    if (this.failRead) throw this.failRead;
    return structuredClone(this.set);
  }

  async getDownloads(fileIds: string[]): Promise<DownloadRef[]> {
    this.downloadCalls.push(fileIds);
    if (this.failDownloads) throw this.failDownloads;
    return fileIds.flatMap((fileId) => {
      const body = this.bodies.get(fileId);
      return body === undefined ? [] : [{ fileId, bytes: new TextEncoder().encode(body) }];
    });
  }

  async report(state: StateReport): Promise<void> {
    if (this.failReport) throw this.failReport;
    this.reports.push(structuredClone(state));
  }
}

describe("createAgent — one pass end to end", () => {
  let dir: string;
  let home: string;
  let transport: FakeTransport;
  let store: MemoryStore;

  const make = (extra: Partial<AgentOptions> = {}) =>
    createAgent({ transport, store, roots: { ...testContext(dir).paths, discoverProjects: false }, ...extra });

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "agent-"));
    home = join(dir, "home");
    mkdirSync(home, { recursive: true });
    transport = new FakeTransport();
    store = new MemoryStore();
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  it("installs the enforced set, persists state, and emits updated", async () => {
    transport.set = { artifacts: [rule(), hook()] };
    const agent = make();
    const seen: string[][] = [];
    agent.on("updated", (state) => seen.push(state.items.map((i) => i.status)));

    const result = await agent.sync();

    assert.equal(result.fetched, true);
    assert.equal(result.artifacts, 2);
    assert.equal(result.targets, 2);
    assert.deepEqual(result.applyFailures, []);
    assert.equal(readFileSync(join(home, ".claude", "CLAUDE.md"), "utf8"), RULE_BODY);
    const settings = JSON.parse(readFileSync(join(home, ".claude", "settings.json"), "utf8"));
    assert.equal(settings.hooks.PreToolUse[0].hooks[0].command, HOOK_COMMAND);
    assert.deepEqual(seen, [["in_sync", "in_sync"]]);
    const saved = await store.load();
    assert.equal(saved?.snapshot.length, 2);
    assert.equal(saved?.history.length, 1);
  });

  it("reports a write failure under home as ~/…, and keeps the full path locally", async () => {
    // A file where the rule's directory should be makes the write fail with the path in its error.
    writeFileSync(join(home, ".claude"), "not a directory\n");
    transport.set = { artifacts: [rule()] };

    const result = await make().sync();

    const [failure] = transport.reports[0]!.applyFailures;
    assert.match(failure!.reason, /^write failed: /);
    assert.ok(!failure!.reason.includes(dir), `no absolute path leaves the machine: ${failure!.reason}`);
    assert.match(failure!.reason, /~\/\.claude/);
    assert.ok(result.applyFailures[0]!.reason.includes(home), "the local result keeps the full path");
  });

  it("a second pass with everything on disk downloads nothing", async () => {
    transport.set = { artifacts: [rule()] };
    const agent = make();
    await agent.sync();
    transport.downloadCalls = [];

    const result = await agent.sync();

    assert.deepEqual(transport.downloadCalls, []);
    assert.equal(result.state.items[0]?.status, "in_sync");
  });

  it("an unreadable enforced set writes nothing and records the miss", async () => {
    transport.set = { artifacts: [rule()] };
    transport.failRead = new Error("connection refused");
    const agent = make();

    const result = await agent.sync();

    assert.equal(result.fetched, false);
    assert.match(result.fetchError ?? "", /connection refused/);
    assert.equal(existsSync(join(home, ".claude", "CLAUDE.md")), false);
    assert.equal(transport.reports.length, 0);
    assert.equal((await agent.status()).lastSync?.fetched, false);
  });

  it("a malformed enforced set is treated as unreadable", async () => {
    transport.set = { nope: true } as unknown as EnforcedSet;
    const result = await make().sync();
    assert.equal(result.fetched, false);
  });

  it("every answered read is followed by a full report of the managed files", async () => {
    transport.set = { artifacts: [rule(), hook()] };

    await make().sync();

    assert.equal(transport.reports.length, 1);
    const report = transport.reports[0]!;
    assert.match(report.deviceId, /^[0-9a-f-]{36}$/);
    assert.equal(report.agentVersion, AGENT_VERSION);
    assert.deepEqual(report.knownRepositories, []);
    assert.deepEqual(report.applyFailures, []);
    assert.deepEqual(
      report.installed.sort((a, b) => a.sourcePath.localeCompare(b.sourcePath)),
      [
        { ide: "IDE_TYPE_CLAUDE_CODE", repo: "", checkout: "", sourcePath: ".claude/CLAUDE.md", contentHash: sha256(RULE_BODY) },
        { ide: "IDE_TYPE_CLAUDE_CODE", repo: "", checkout: "", sourcePath: ".claude/hooks/guard.sh", contentHash: sha256(HOOK_SCRIPT) },
      ],
    );
  });

  it("the device id is minted once and kept across passes", async () => {
    transport.set = { artifacts: [] };
    const agent = make();
    await agent.sync();
    await agent.sync();
    assert.equal(transport.reports.length, 2);
    assert.equal(transport.reports[0]!.deviceId, transport.reports[1]!.deviceId);
    assert.equal((await store.load())?.deviceId, transport.reports[0]!.deviceId);
  });

  it("apply failures ride the full report with their placement and hash", async () => {
    transport.set = { artifacts: [rule()] };
    transport.bodies.delete("f-rule");

    const result = await make().sync();

    assert.equal(result.applyFailures.length, 1);
    assert.equal(transport.reports.length, 1);
    assert.deepEqual(transport.reports[0]!.installed, []);
    const failure = transport.reports[0]!.applyFailures[0]!;
    assert.equal(failure.artifactId, "r1");
    assert.equal(failure.repository, "");
    assert.equal(failure.contentHash, sha256(RULE_BODY));
    assert.match(failure.reason, /download failed/);
  });

  it("a paused set writes and retracts nothing but still reports", async () => {
    transport.set = { artifacts: [rule(), hook()] };
    const agent = make();
    await agent.sync();
    const settingsPath = join(home, ".claude", "settings.json");
    const settingsBefore = readFileSync(settingsPath, "utf8");
    const snapshotBefore = (await store.load())?.snapshot;
    const downloadsBefore = transport.downloadCalls.length;

    transport.set = { artifacts: [], paused: true };
    const paused = await agent.sync();
    await agent.sync();

    assert.equal(paused.paused, true);
    assert.equal(paused.fetched, true);
    assert.equal(transport.downloadCalls.length, downloadsBefore);
    assert.equal(readFileSync(join(home, ".claude", "CLAUDE.md"), "utf8"), RULE_BODY);
    assert.equal(readFileSync(settingsPath, "utf8"), settingsBefore);
    assert.deepEqual((await store.load())?.snapshot, snapshotBefore);
    assert.equal(transport.reports.at(-1)!.installed.length, 2);
  });

  it("a failed report is surfaced but the pass still persists", async () => {
    transport.set = { artifacts: [rule()] };
    transport.failReport = new Error("503");

    const result = await make().sync();

    assert.equal(result.reported, false);
    assert.match(result.reportError ?? "", /503/);
    assert.equal((await store.load())?.snapshot.length, 1);
  });

  it("an aborted download request keeps prior state and retries next pass", async () => {
    transport.set = { artifacts: [rule()] };
    transport.failDownloads = new Error("HTTP 500");
    const agent = make();

    const first = await agent.sync();
    assert.match(first.passError ?? "", /HTTP 500/);
    assert.equal(existsSync(join(home, ".claude", "CLAUDE.md")), false);

    transport.failDownloads = null;
    const second = await agent.sync();
    assert.equal(second.passError, undefined);
    assert.equal(readFileSync(join(home, ".claude", "CLAUDE.md"), "utf8"), RULE_BODY);
  });

  it("concurrent sync calls share one pass", async () => {
    transport.set = { artifacts: [rule()] };
    const agent = make();
    const [a, b] = await Promise.all([agent.sync(), agent.sync()]);
    assert.equal(a, b);
    assert.equal(transport.reads, 1);
  });

  it("a hook dropped from the set is retracted only after the floor's one-pass hold", async () => {
    transport.set = { artifacts: [rule(), hook()] };
    const agent = make();
    await agent.sync();
    const settingsPath = join(home, ".claude", "settings.json");

    // First read without the hook: held, in case the read is a transient partial answer.
    transport.set = { artifacts: [rule()] };
    await agent.sync();
    assert.equal(JSON.parse(readFileSync(settingsPath, "utf8")).hooks.PreToolUse.length, 1, "held one pass");

    // Confirmed on the next read: retracted, and the unmodified script goes with it.
    await agent.sync();
    assert.equal(JSON.parse(readFileSync(settingsPath, "utf8")).hooks?.PreToolUse, undefined, "retracted");
    assert.equal(existsSync(join(home, ".claude", "hooks", "guard.sh")), false, "unmodified script removed");
    assert.deepEqual((await agent.status()).state.items.map((i) => i.artifactId), ["r1"]);
  });

  it("a transient partial read that recovers never retracts", async () => {
    transport.set = { artifacts: [rule(), hook()] };
    const agent = make();
    await agent.sync();
    transport.set = { artifacts: [rule()] };
    await agent.sync();
    transport.set = { artifacts: [rule(), hook()] };
    await agent.sync();
    const settings = JSON.parse(readFileSync(join(home, ".claude", "settings.json"), "utf8"));
    assert.equal(settings.hooks.PreToolUse.length, 1);
  });

  it("a Cursor artifact is skipped on a machine without Cursor", async () => {
    transport.set = {
      artifacts: [rule({ artifactId: "c1", ideType: "IDE_TYPE_CURSOR", installPath: ".cursor/rules/org.mdc" })],
    };
    const result = await make().sync();
    assert.equal(result.targets, 0);
    assert.equal(existsSync(join(home, ".cursor")), false);
  });
});

describe("createAgent — repo-local artifacts converge into configured checkouts", () => {
  let dir: string;
  let repo: string;

  const git = (root: string, ...args: string[]): string =>
    execFileSync("git", ["-C", root, ...args], { encoding: "utf8", env: gitProbeEnv() });

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "agent-repo-"));
    mkdirSync(join(dir, "home"), { recursive: true });
    repo = realpathSync(mkdtempSync(join(tmpdir(), "agent-checkout-")));
    git(repo, "init", "-q");
    git(repo, "remote", "add", "origin", "https://token@git.example.com/example-org/example-repo.git");
    mkdirSync(join(repo, "src"), { recursive: true });
    writeFileSync(join(repo, "src", "main.txt"), "x\n");
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
    rmSync(repo, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  it("writes into the checkout, excluded from git status", async () => {
    const transport = new FakeTransport();
    transport.set = {
      artifacts: [
        rule({
          artifactId: "rr1",
          type: "ARTIFACT_TYPE_RULE",
          scope: "ARTIFACT_SCOPE_REPO_LOCAL",
          repoFullName: "example-org/example-repo",
          installPath: ".claude/rules/org.md",
        }),
      ],
    };
    const agent = createAgent({
      transport,
      store: new MemoryStore(),
      // A subdirectory resolves up to its checkout.
      roots: { ...testContext(dir).paths, projects: [join(repo, "src")], discoverProjects: false },
    });

    const result = await agent.sync();

    assert.equal(result.targets, 1);
    assert.equal(readFileSync(join(repo, ".claude", "rules", "org.md"), "utf8"), RULE_BODY);
    assert.doesNotMatch(git(repo, "status", "--porcelain"), /\.claude/);
    assert.deepEqual(result.state.items[0]?.targets.map((t) => t.root), [repo]);
    assert.equal(transport.reports[0]?.installed[0]?.repo, "example-org/example-repo");
  });

  it("reports a write failure inside a checkout relative to its repository", async () => {
    mkdirSync(join(repo, ".claude"), { recursive: true });
    writeFileSync(join(repo, ".claude", "rules"), "not a directory\n");
    const transport = new FakeTransport();
    transport.set = {
      artifacts: [
        rule({
          artifactId: "rr1",
          type: "ARTIFACT_TYPE_RULE",
          scope: "ARTIFACT_SCOPE_REPO_LOCAL",
          repoFullName: "example-org/example-repo",
          installPath: ".claude/rules/org.md",
        }),
      ],
    };
    const agent = createAgent({
      transport,
      store: new MemoryStore(),
      roots: { ...testContext(dir).paths, projects: [repo], discoverProjects: false },
    });

    await agent.sync();

    const [failure] = transport.reports[0]!.applyFailures;
    assert.match(failure!.reason, /^write failed: /);
    assert.ok(!failure!.reason.includes(repo), `no checkout path leaves the machine: ${failure!.reason}`);
    assert.match(failure!.reason, /example-org\/example-repo\/\.claude\/rules/);
  });

  it("reports repository names lowercased, whatever case the server or the remote uses", async () => {
    git(repo, "remote", "set-url", "origin", "https://git.example.com/Example-Org/Example-Repo.git");
    const transport = new FakeTransport();
    transport.set = {
      artifacts: [
        rule({
          artifactId: "rr1",
          type: "ARTIFACT_TYPE_RULE",
          scope: "ARTIFACT_SCOPE_REPO_LOCAL",
          repoFullName: "EXAMPLE-ORG/example-repo",
          installPath: ".claude/rules/org.md",
        }),
        rule({
          artifactId: "rr2",
          type: "ARTIFACT_TYPE_RULE",
          scope: "ARTIFACT_SCOPE_REPO_LOCAL",
          repoFullName: "Example-Org/Example-Repo",
          installPath: ".claude/rules/missing.md",
          primaryFileId: "f-missing",
        }),
      ],
    };
    const agent = createAgent({
      transport,
      store: new MemoryStore(),
      roots: { ...testContext(dir).paths, projects: [repo], discoverProjects: false },
    });

    await agent.sync();

    const report = transport.reports[0]!;
    assert.deepEqual(report.knownRepositories, ["example-org/example-repo"]);
    assert.deepEqual(report.installed.map((i) => i.repo), ["example-org/example-repo"]);
    assert.deepEqual(report.applyFailures.map((f) => [f.artifactId, f.repository]), [["rr2", "example-org/example-repo"]]);
  });

  it("two checkouts of one repository report side by side under distinct checkout keys", async () => {
    const second = realpathSync(mkdtempSync(join(tmpdir(), "agent-checkout-2-")));
    try {
      git(second, "init", "-q");
      git(second, "remote", "add", "origin", "git@git.example.com:example-org/example-repo.git");
      const transport = new FakeTransport();
      transport.set = {
        artifacts: [
          rule({
            artifactId: "rr1",
            type: "ARTIFACT_TYPE_RULE",
            scope: "ARTIFACT_SCOPE_REPO_LOCAL",
            repoFullName: "example-org/example-repo",
            installPath: ".claude/rules/org.md",
          }),
          rule(),
        ],
      };
      const agent = createAgent({
        transport,
        store: new MemoryStore(),
        roots: { ...testContext(dir).paths, projects: [repo, second], discoverProjects: false },
      });

      await agent.sync();

      const rows = transport.reports[0]!.installed.filter((i) => i.repo !== "");
      assert.equal(rows.length, 2);
      const keys = rows.map((r) => r.checkout);
      assert.equal(new Set(keys).size, 2, "distinct per checkout");
      for (const k of keys) {
        assert.match(k, /^[0-9a-f]{16}$/);
        assert.ok(!repo.includes(k) && !second.includes(k), "an opaque key, not a path");
      }
      assert.deepEqual(transport.reports[0]!.installed.filter((i) => i.repo === "").map((i) => i.checkout), [""]);

      // Stable across passes, so the server's report identity does not churn.
      await agent.sync();
      assert.deepEqual(transport.reports[1]!.installed.filter((i) => i.repo !== "").map((r) => r.checkout).sort(), [...keys].sort());
    } finally {
      rmSync(second, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
    }
  });

  it("a repo with no local checkout resolves to no targets", async () => {
    const transport = new FakeTransport();
    transport.set = {
      artifacts: [
        rule({ scope: "ARTIFACT_SCOPE_REPO_LOCAL", repoFullName: "example-org/other-repo", installPath: "CLAUDE.md" }),
      ],
    };
    const agent = createAgent({
      transport,
      store: new MemoryStore(),
      roots: { ...testContext(dir).paths, projects: [repo], discoverProjects: false },
    });
    const result = await agent.sync();
    assert.equal(result.targets, 0);
    assert.equal(existsSync(join(repo, "CLAUDE.md")), false);
  });
});
