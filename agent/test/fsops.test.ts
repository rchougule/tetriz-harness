import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync, readFileSync, readdirSync, existsSync, mkdirSync, writeFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { homedir } from "node:os";
import { fileURLToPath } from "node:url";
import { defaultPaths } from "../src/engine/context.js";
import { installRootForIde, resolveInstallPath, writeAtomic } from "../src/engine/fsops.js";

const ROOTS = { home: "/Users/eng", codexHome: "/tmp/custom-codex-home" };

describe("installRootForIde — Codex hooks anchor on the Codex home; everything else on home", () => {
  it("non-Codex ide uses the supplied home root", () => {
    assert.equal(installRootForIde("IDE_TYPE_CLAUDE_CODE", "ARTIFACT_TYPE_SKILL", ROOTS), "/Users/eng");
    assert.equal(installRootForIde("IDE_TYPE_CURSOR", "ARTIFACT_TYPE_RULE", ROOTS), "/Users/eng");
  });

  it("a Codex hook resolves against the Codex home", () => {
    const root = installRootForIde("IDE_TYPE_CODEX", "ARTIFACT_TYPE_HOOK", ROOTS);
    assert.equal(root, "/tmp/custom-codex-home");
    assert.equal(resolveInstallPath(root, "hooks/measure.sh"), "/tmp/custom-codex-home/hooks/measure.sh");
  });

  it("a Codex user-global skill (.agents/…) anchors on home, not the Codex home", () => {
    const root = installRootForIde("IDE_TYPE_CODEX", "ARTIFACT_TYPE_SKILL", ROOTS);
    assert.equal(root, "/Users/eng");
    assert.equal(
      resolveInstallPath(root, ".agents/skills/code-review/SKILL.md"),
      "/Users/eng/.agents/skills/code-review/SKILL.md",
    );
  });

  it("a Codex user-global CLAUDE.md lands under home, not nested inside the Codex home", () => {
    const root = installRootForIde("IDE_TYPE_CODEX", "ARTIFACT_TYPE_CLAUDE_MD", ROOTS);
    assert.equal(resolveInstallPath(root, ".claude/CLAUDE.md"), "/Users/eng/.claude/CLAUDE.md");
    assert.notEqual(resolveInstallPath(root, ".claude/CLAUDE.md"), "/tmp/custom-codex-home/.claude/CLAUDE.md");
  });

  it("defaultPaths: an unset Codex home falls back to $CODEX_HOME, then <home>/.codex", () => {
    const saved = process.env["CODEX_HOME"];
    try {
      delete process.env["CODEX_HOME"];
      assert.equal(defaultPaths({ home: "/Users/eng" }).codexHome, join("/Users/eng", ".codex"));
      process.env["CODEX_HOME"] = "/tmp/env-codex-home";
      assert.equal(defaultPaths({ home: "/Users/eng" }).codexHome, "/tmp/env-codex-home");
      assert.equal(defaultPaths({ home: "/Users/eng", codexHome: "/x" }).codexHome, "/x", "explicit wins");
      assert.equal(defaultPaths().home, homedir());
    } finally {
      if (saved === undefined) delete process.env["CODEX_HOME"];
      else process.env["CODEX_HOME"] = saved;
    }
  });
});

interface PathCase {
  ide: string;
  scope: string;
  type: string;
  home: string;
  codex_home?: string;
  repo_root?: string;
  install_path?: string;
  absolute_path?: string;
  ts_absolute_path?: string;
  error?: string;
}

describe("install-path conformance with the server's derivation (shared fixture)", () => {
  const fixture = JSON.parse(
    readFileSync(fileURLToPath(new URL("../../../testdata/install-paths.json", import.meta.url)), "utf8"),
  ) as { cases: PathCase[] };
  // Rows the server rejects never reach an agent; only placements are the agent's to agree with.
  const placements = fixture.cases.filter((c) => c.error === undefined);

  it("the fixture has placements to check", () => {
    assert.ok(placements.length > 20, `expected many placement rows, got ${placements.length}`);
  });

  for (const c of placements) {
    const label = `${c.ide} ${c.scope} ${c.type}${c.codex_home ? " (codex home set)" : ""}`;
    it(`${label} → ${c.absolute_path}`, () => {
      const roots = { home: c.home, codexHome: c.codex_home ?? join(c.home, ".codex") };
      const root = c.scope.endsWith("REPO_LOCAL") ? c.repo_root! : installRootForIde(c.ide, c.type, roots);
      const abs = resolveInstallPath(root, c.install_path!);
      assert.equal(abs, c.absolute_path);
      if (c.ts_absolute_path !== undefined) assert.notEqual(abs, c.ts_absolute_path, "the old path-based rule");
    });
  }
});

describe("resolveInstallPath — path containment (defense in depth)", () => {
  it("resolves a normal relative install path under root", () => {
    const abs = resolveInstallPath("/Users/eng", ".claude/skills/foo/SKILL.md");
    assert.equal(abs, "/Users/eng/.claude/skills/foo/SKILL.md");
  });

  it("rejects a '..' escape", () => {
    assert.equal(resolveInstallPath("/Users/eng", "../../etc/passwd"), null);
  });

  it("rejects an absolute install path", () => {
    assert.equal(resolveInstallPath("/Users/eng", "/etc/passwd"), null);
  });

  it("rejects an install path that resolves to root itself (empty relative)", () => {
    assert.equal(resolveInstallPath("/Users/eng", "."), null);
  });
});

describe("writeAtomic — stage-then-rename", () => {
  let dir: string;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "write-atomic-"));
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
  });

  it("creates parent directories and writes the content", () => {
    const target = join(dir, "nested", "deep", "CLAUDE.md");
    writeAtomic(target, Buffer.from("hello"));
    assert.equal(readFileSync(target, "utf8"), "hello");
  });

  it("leaves no temp file behind on success", () => {
    const target = join(dir, "settings.json");
    writeAtomic(target, Buffer.from("{}"));
    const entries = readdirSync(dir);
    assert.deepEqual(entries, ["settings.json"]);
  });

  it("overwrites an existing file", () => {
    const target = join(dir, "CLAUDE.md");
    mkdirSync(dir, { recursive: true });
    writeFileSync(target, "old");
    writeAtomic(target, Buffer.from("new"));
    assert.equal(readFileSync(target, "utf8"), "new");
    assert.equal(existsSync(target), true);
  });

  it("sets the execute bit when executable is requested (hook script)", () => {
    const target = join(dir, "hooks", "measure.sh");
    writeAtomic(target, Buffer.from("#!/bin/sh\necho hi\n"), { executable: true });
    assert.equal(statSync(target).mode & 0o111, 0o111);
  });

  it("leaves a non-executable file without the execute bit by default", () => {
    const target = join(dir, "CLAUDE.md");
    writeAtomic(target, Buffer.from("# doc"));
    assert.equal(statSync(target).mode & 0o111, 0);
  });
});
