import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";
import { parseDuration } from "../src/cli/main.js";
import {
  type CommandRunner,
  LEGACY_SERVICE_LABEL,
  installService,
  launcherPath,
  renderLaunchdPlist,
  renderLauncher,
  renderSystemdUnit,
  serviceArgs,
  serviceFilePath,
  servicePlatform,
  uninstallService,
} from "../src/cli/service.js";

const CLI = fileURLToPath(new URL("../src/cli/main.js", import.meta.url));

const spec = {
  shell: "/bin/zsh",
  cliPath: "/opt/harness agent/main.js",
  server: "https://hcp.example.com/?a=1&b=2",
  interval: "5m",
  stateDir: "/home/dev/.config/harness-agent",
  extraArgs: ["--no-discover"],
};

function cli(args: string[], env: Record<string, string> = {}) {
  return spawnSync(process.execPath, [CLI, ...args], {
    encoding: "utf8",
    env: { PATH: process.env["PATH"] ?? "", HOME: process.env["HOME"] ?? "", ...env },
  });
}

describe("parseDuration", () => {
  it("accepts plain seconds and unit suffixes", () => {
    assert.equal(parseDuration("90"), 90_000);
    assert.equal(parseDuration("30s"), 30_000);
    assert.equal(parseDuration("5m"), 300_000);
    assert.equal(parseDuration("1h"), 3_600_000);
  });

  it("rejects garbage and sub-second intervals", () => {
    assert.throws(() => parseDuration("soon"));
    assert.throws(() => parseDuration("500ms"));
  });
});

const escapeRe = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

describe("service units", () => {
  it("run `watch` through the launcher without embedding the token", () => {
    const args = serviceArgs(spec);
    assert.equal(args[0], "/home/dev/.config/harness-agent/bin/harness-control-plane-agent");
    assert.deepEqual(args.slice(1, 3), ["watch", "--server"]);
    assert.ok(!args.some((a) => a.includes("token")));
  });

  it("the launcher resolves node from the login shell's PATH at start instead of pinning a binary", () => {
    const launcher = renderLauncher(spec);
    assert.match(launcher, /^#!\/bin\/sh\n/);
    assert.match(launcher, /^exec '\/bin\/zsh' -lc 'exec node "\$0" "\$@"' '\/opt\/harness agent\/main\.js' "\$@"$/m);
    const pinned = new RegExp(escapeRe(process.execPath));
    assert.doesNotMatch(launcher, pinned);
    assert.doesNotMatch(renderSystemdUnit(spec), pinned);
    assert.doesNotMatch(renderLaunchdPlist(spec), pinned);
  });

  it("the launcher quotes a path containing a single quote", () => {
    assert.match(renderLauncher({ ...spec, cliPath: "/opt/it's/main.js" }), /'\/opt\/it'\\''s\/main\.js'/);
  });

  it("the launcher finds whatever node the login PATH names when it runs", () => {
    const dir = mkdtempSync(join(tmpdir(), "svc-path-"));
    try {
      // The login profile, not the unit, supplies PATH, as on a machine whose node moves on upgrade.
      writeFileSync(join(dir, ".profile"), `PATH="${dirname(process.execPath)}:/usr/bin:/bin"; export PATH\n`);
      const script = join(dir, "argv.mjs");
      writeFileSync(script, "console.log(JSON.stringify(process.argv.slice(2)));\n");
      const launcher = join(dir, "launcher");
      writeFileSync(launcher, renderLauncher({ ...spec, shell: "/bin/sh", cliPath: script }), { mode: 0o755 });
      const res = spawnSync(launcher, ["watch", "--server", "a b"], { encoding: "utf8", env: { HOME: dir, PATH: "/nonexistent" } });
      assert.equal(res.status, 0, res.stderr);
      assert.deepEqual(JSON.parse(res.stdout.trim().split("\n").at(-1)!), ["watch", "--server", "a b"]);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("launchd plist runs the launcher, escapes XML and logs under the state dir", () => {
    const plist = renderLaunchdPlist(spec);
    const launcher = "/home/dev/.config/harness-agent/bin/harness-control-plane-agent";
    assert.match(plist, /<key>Label<\/key>\n {2}<string>harness-control-plane-agent<\/string>/);
    assert.match(plist, new RegExp(`<key>Program</key>\\n  <string>${escapeRe(launcher)}</string>`));
    assert.match(plist, new RegExp(`<key>ProgramArguments</key>\\n  <array>\\n    <string>${escapeRe(launcher)}</string>\\n    <string>watch</string>`));
    assert.doesNotMatch(plist, /zsh/);
    assert.match(plist, /<string>https:\/\/hcp\.example\.com\/\?a=1&amp;b=2<\/string>/);
    assert.match(plist, /<string>\/home\/dev\/\.config\/harness-agent\/agent\.log<\/string>/);
  });

  it("systemd unit quotes every argument and escapes specifiers", () => {
    const unit = renderSystemdUnit({ ...spec, server: "https://hcp.example.com/%h$x" });
    assert.match(unit, /^Description=harness-control-plane-agent: /m);
    assert.match(unit, /^ExecStart="\/home\/dev\/\.config\/harness-agent\/bin\/harness-control-plane-agent" "watch" /m);
    assert.match(unit, /"https:\/\/hcp\.example\.com\/%%h\$\$x"/);
    assert.match(unit, /WantedBy=default\.target/);
  });

  it("unit files live in the per-user locations", () => {
    assert.equal(serviceFilePath("launchd", "/Users/dev"), "/Users/dev/Library/LaunchAgents/harness-control-plane-agent.plist");
    assert.match(serviceFilePath("systemd", "/home/dev"), /systemd\/user\/harness-control-plane-agent\.service$/);
    assert.equal(servicePlatform("darwin"), "launchd");
    assert.equal(servicePlatform("linux"), "systemd");
    assert.equal(servicePlatform("win32"), null);
  });
});

describe("service install and uninstall", () => {
  // Never touch the real launchctl/systemctl or the real home: every run uses a temp home and a fake runner.
  function withHome(fn: (home: string, calls: string[][], runner: CommandRunner) => void) {
    const home = mkdtempSync(join(tmpdir(), "svc-home-"));
    const prevXdg = process.env["XDG_CONFIG_HOME"];
    process.env["XDG_CONFIG_HOME"] = join(home, ".config");
    const calls: string[][] = [];
    const runner: CommandRunner = (cmd, args) => {
      calls.push([cmd, ...args]);
      return { ok: true, output: "" };
    };
    try {
      fn(home, calls, runner);
    } finally {
      if (prevXdg === undefined) delete process.env["XDG_CONFIG_HOME"];
      else process.env["XDG_CONFIG_HOME"] = prevXdg;
      rmSync(home, { recursive: true, force: true });
    }
  }

  for (const kind of ["launchd", "systemd"] as const) {
    it(`${kind}: install replaces a service under the old label and writes an executable launcher`, () => {
      withHome((home, calls, runner) => {
        const stateDir = join(home, "state");
        const legacy = serviceFilePath(kind, home, LEGACY_SERVICE_LABEL);
        mkdirSync(dirname(legacy), { recursive: true });
        writeFileSync(legacy, "old");
        const lines = installService(kind, { ...spec, stateDir }, home, runner);
        assert.ok(!existsSync(legacy), "old unit removed");
        assert.ok(lines.includes(`removed ${legacy}`));
        const launcher = launcherPath(stateDir);
        assert.equal(statSync(launcher).mode & 0o777, 0o755);
        assert.equal(readFileSync(launcher, "utf8"), renderLauncher({ ...spec, stateDir }));
        const unit = readFileSync(serviceFilePath(kind, home), "utf8");
        assert.ok(unit.includes(launcher));
        const flat = calls.map((c) => c.join(" "));
        if (kind === "launchd") {
          assert.ok(flat.some((c) => /^launchctl bootout gui\/\d+\/harness-agent$/.test(c)), flat.join("\n"));
          assert.ok(flat.some((c) => c.startsWith("launchctl bootstrap ") && c.endsWith("harness-control-plane-agent.plist")));
        } else {
          assert.ok(flat.includes("systemctl --user disable --now harness-agent.service"), flat.join("\n"));
          assert.ok(flat.includes("systemctl --user enable --now harness-control-plane-agent.service"));
        }
      });
    });

    it(`${kind}: uninstall removes the launcher, the unit and a leftover old-label unit`, () => {
      withHome((home, calls, runner) => {
        const stateDir = join(home, "state");
        installService(kind, { ...spec, stateDir }, home, runner);
        const legacy = serviceFilePath(kind, home, LEGACY_SERVICE_LABEL);
        writeFileSync(legacy, "old");
        calls.length = 0;
        const lines = uninstallService(kind, stateDir, home, runner);
        assert.ok(!existsSync(serviceFilePath(kind, home)));
        assert.ok(!existsSync(legacy));
        assert.ok(!existsSync(launcherPath(stateDir)));
        assert.ok(!existsSync(dirname(launcherPath(stateDir))), "empty bin dir removed");
        assert.ok(existsSync(stateDir), "state dir kept");
        assert.ok(lines.includes(`removed ${legacy}`));
        const flat = calls.map((c) => c.join(" "));
        const stops = kind === "launchd"
          ? [/^launchctl bootout gui\/\d+\/harness-control-plane-agent$/, /^launchctl bootout gui\/\d+\/harness-agent$/]
          : [/^systemctl --user disable --now harness-control-plane-agent\.service$/, /^systemctl --user disable --now harness-agent\.service$/];
        for (const re of stops) assert.ok(flat.some((c) => re.test(c)), flat.join("\n"));
      });
    });
  }
});

describe("harness-agent CLI", () => {
  it("prints usage and exits 64 with no command", () => {
    const res = cli([]);
    assert.equal(res.status, 64);
    assert.match(res.stdout, /usage: harness-agent/);
  });

  it("sync without a server is a usage error", () => {
    const res = cli(["sync"]);
    assert.equal(res.status, 64);
    assert.match(res.stderr, /--server/);
  });

  it("sync to a plain-HTTP remote server is a usage error unless --insecure-http is given", () => {
    const dir = mkdtempSync(join(tmpdir(), "cli-"));
    try {
      const env = { HARNESS_AGENT_SERVER: "http://hcp.example.invalid", HARNESS_AGENT_TOKEN: "t" };
      const refused = cli(["sync", "--state-dir", dir, "--no-discover"], env);
      assert.equal(refused.status, 64);
      assert.match(refused.stderr, /--insecure-http/);
      const allowed = cli(["sync", "--state-dir", dir, "--no-discover", "--insecure-http"], env);
      assert.equal(allowed.status, 2, allowed.stderr); // past the check; the host does not resolve
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("sync against an unreachable server exits 2 and writes nothing", () => {
    const dir = mkdtempSync(join(tmpdir(), "cli-"));
    try {
      const res = cli(["sync", "--state-dir", dir, "--no-discover"], {
        HARNESS_AGENT_SERVER: "http://127.0.0.1:9",
        HARNESS_AGENT_TOKEN: "t",
      });
      assert.equal(res.status, 2, res.stderr);
      assert.match(res.stdout, /enforced set unavailable/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("status lists files the agent left in place, with the reason", () => {
    const dir = mkdtempSync(join(tmpdir(), "cli-"));
    try {
      writeFileSync(
        join(dir, "state.json"),
        JSON.stringify({
          version: 1,
          retained: [
            { artifactId: "sk1", ideType: "IDE_TYPE_CLAUDE_CODE", repoFullName: "", targetKey: "", root: "/home/dev",
              sourcePath: ".claude/skills/lint/helper.sh", installedHash: "h", reason: "left in place: modified since installed", since: "2026-10-15T00:00:00.000Z" },
          ],
        }),
      );
      const res = cli(["status", "--state-dir", dir]);
      assert.equal(res.status, 0, res.stderr);
      assert.match(res.stdout, /kept +\/home\/dev\/\.claude\/skills\/lint\/helper\.sh: left in place: modified since installed/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("status on an empty state dir says so", () => {
    const dir = mkdtempSync(join(tmpdir(), "cli-"));
    try {
      const res = cli(["status", "--state-dir", dir]);
      assert.equal(res.status, 0);
      assert.match(res.stdout, /no state yet/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("install-service --print prints the unit and installs nothing", { skip: servicePlatform() === null }, () => {
    const dir = mkdtempSync(join(tmpdir(), "cli-"));
    try {
      const res = cli(["install-service", "--print", "--server", "https://hcp.example.com", "--state-dir", dir], {
        HOME: dir,
      });
      assert.equal(res.status, 0, res.stderr);
      assert.match(res.stdout, /watch/);
      assert.match(res.stdout, new RegExp(escapeRe(join(dir, "bin", "harness-control-plane-agent"))));
      assert.match(res.stdout, /^#!\/bin\/sh$/m);
      assert.ok(!existsSync(join(dir, "bin")), "launcher not written");
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
