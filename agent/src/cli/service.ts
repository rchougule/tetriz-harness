import { spawnSync } from "node:child_process";
import { chmodSync, existsSync, mkdirSync, rmdirSync, rmSync, writeFileSync } from "node:fs";
import { homedir, platform } from "node:os";
import { dirname, isAbsolute, join } from "node:path";

// macOS Login Items names a background item after its executable, so the launcher carries this name too.
export const SERVICE_LABEL = "harness-control-plane-agent";
// Installs from before the rename used this label and unit name; install and uninstall remove them.
export const LEGACY_SERVICE_LABEL = "harness-agent";

export interface ServiceSpec {
  /** Login shell whose PATH supplies `node` each time the service starts. */
  shell: string;
  /** Absolute CLI entry point. */
  cliPath: string;
  server: string;
  interval: string;
  stateDir: string;
  extraArgs?: string[];
}

export type ServicePlatform = "launchd" | "systemd";

export function servicePlatform(p: NodeJS.Platform = platform()): ServicePlatform | null {
  if (p === "darwin") return "launchd";
  if (p === "linux") return "systemd";
  return null;
}

/** The user's login shell, else /bin/sh; a shell path survives node upgrades where a node path may not. */
export function defaultServiceShell(env: NodeJS.ProcessEnv = process.env): string {
  const sh = env["SHELL"];
  return sh !== undefined && isAbsolute(sh) ? sh : "/bin/sh";
}

export function launcherPath(stateDir: string): string {
  return join(stateDir, "bin", SERVICE_LABEL);
}

function shQuote(s: string): string {
  return `'${s.replace(/'/g, `'\\''`)}'`;
}

/**
 * `node` is looked up on the login shell's PATH at every start, not pinned, so a version manager or
 * package upgrade that moves the binary does not break the service.
 */
export function renderLauncher(spec: ServiceSpec): string {
  return `#!/bin/sh
exec ${shQuote(spec.shell)} -lc 'exec node "$0" "$@"' ${shQuote(spec.cliPath)} "$@"
`;
}

/** The token is not embedded: it is read from the state dir, so the unit file holds no secret. */
export function serviceArgs(spec: ServiceSpec): string[] {
  return [
    launcherPath(spec.stateDir),
    "watch",
    "--server",
    spec.server,
    "--interval",
    spec.interval,
    "--state-dir",
    spec.stateDir,
    ...(spec.extraArgs ?? []),
  ];
}

function xmlEscape(s: string): string {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

export function renderLaunchdPlist(spec: ServiceSpec): string {
  const args = serviceArgs(spec)
    .map((a) => `    <string>${xmlEscape(a)}</string>`)
    .join("\n");
  const logPath = xmlEscape(join(spec.stateDir, "agent.log"));
  return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${SERVICE_LABEL}</string>
  <key>Program</key>
  <string>${xmlEscape(launcherPath(spec.stateDir))}</string>
  <key>ProgramArguments</key>
  <array>
${args}
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ThrottleInterval</key>
  <integer>30</integer>
  <key>StandardOutPath</key>
  <string>${logPath}</string>
  <key>StandardErrorPath</key>
  <string>${logPath}</string>
</dict>
</plist>
`;
}

/** systemd splits ExecStart on whitespace; quote every argument and escape what the quotes do not cover. */
function systemdQuote(arg: string): string {
  return `"${arg.replace(/\\/g, "\\\\").replace(/"/g, '\\"').replace(/%/g, "%%").replace(/\$/g, "$$$$")}"`;
}

export function renderSystemdUnit(spec: ServiceSpec): string {
  return `[Unit]
Description=harness-control-plane-agent: harness control plane sync
After=network-online.target

[Service]
Type=simple
ExecStart=${serviceArgs(spec).map(systemdQuote).join(" ")}
Restart=on-failure
RestartSec=30

[Install]
WantedBy=default.target
`;
}

export function serviceFilePath(kind: ServicePlatform, home: string = homedir(), label: string = SERVICE_LABEL): string {
  return kind === "launchd"
    ? join(home, "Library", "LaunchAgents", `${label}.plist`)
    : join(process.env["XDG_CONFIG_HOME"] || join(home, ".config"), "systemd", "user", `${label}.service`);
}

export function renderService(kind: ServicePlatform, spec: ServiceSpec): string {
  return kind === "launchd" ? renderLaunchdPlist(spec) : renderSystemdUnit(spec);
}

export type CommandRunner = (cmd: string, args: string[]) => { ok: boolean; output: string };

function run(cmd: string, args: string[]): { ok: boolean; output: string } {
  const res = spawnSync(cmd, args, { encoding: "utf8" });
  return { ok: res.status === 0, output: `${res.stdout ?? ""}${res.stderr ?? ""}`.trim() || (res.error?.message ?? "") };
}

function uid(): number {
  return process.getuid?.() ?? 0;
}

/** Stop the service under `label` and delete its unit file; a "not loaded" error from the stop is expected. */
function removeUnit(kind: ServicePlatform, label: string, home: string, runner: CommandRunner): string | null {
  if (kind === "launchd") {
    runner("launchctl", ["bootout", `gui/${uid()}/${label}`]);
  } else {
    runner("systemctl", ["--user", "disable", "--now", `${label}.service`]);
  }
  const path = serviceFilePath(kind, home, label);
  if (!existsSync(path)) return null;
  rmSync(path, { force: true });
  return path;
}

/** Write the launcher and unit and start it. Returns human-readable lines describing what happened. */
export function installService(
  kind: ServicePlatform,
  spec: ServiceSpec,
  home: string = homedir(),
  runner: CommandRunner = run,
): string[] {
  const lines: string[] = [];
  const legacy = removeUnit(kind, LEGACY_SERVICE_LABEL, home, runner);
  if (legacy !== null) lines.push(`removed ${legacy}`);
  const path = serviceFilePath(kind, home);
  mkdirSync(dirname(path), { recursive: true });
  mkdirSync(spec.stateDir, { recursive: true, mode: 0o700 });
  const launcher = launcherPath(spec.stateDir);
  mkdirSync(dirname(launcher), { recursive: true, mode: 0o700 });
  writeFileSync(launcher, renderLauncher(spec), { mode: 0o755 });
  chmodSync(launcher, 0o755);
  lines.push(`wrote ${launcher}`);
  writeFileSync(path, renderService(kind, spec), { mode: 0o644 });
  lines.push(`wrote ${path}`);
  if (kind === "launchd") {
    const domain = `gui/${uid()}`;
    // Re-install replaces a running copy; a "not loaded" error here is expected.
    runner("launchctl", ["bootout", `${domain}/${SERVICE_LABEL}`]);
    const res = runner("launchctl", ["bootstrap", domain, path]);
    lines.push(res.ok ? `loaded ${SERVICE_LABEL}` : `could not load (${res.output}); run: launchctl bootstrap ${domain} ${path}`);
  } else {
    const reload = runner("systemctl", ["--user", "daemon-reload"]);
    const enable = reload.ok ? runner("systemctl", ["--user", "enable", "--now", `${SERVICE_LABEL}.service`]) : reload;
    lines.push(
      enable.ok
        ? `enabled ${SERVICE_LABEL}.service`
        : `could not enable (${enable.output}); run: systemctl --user enable --now ${SERVICE_LABEL}.service`,
    );
  }
  return lines;
}

export function uninstallService(
  kind: ServicePlatform,
  stateDir: string,
  home: string = homedir(),
  runner: CommandRunner = run,
): string[] {
  const lines: string[] = [];
  const removed = removeUnit(kind, SERVICE_LABEL, home, runner);
  lines.push(removed !== null ? `removed ${removed}` : `no service file at ${serviceFilePath(kind, home)}`);
  const legacy = removeUnit(kind, LEGACY_SERVICE_LABEL, home, runner);
  if (legacy !== null) lines.push(`removed ${legacy}`);
  const launcher = launcherPath(stateDir);
  if (existsSync(launcher)) {
    rmSync(launcher, { force: true });
    lines.push(`removed ${launcher}`);
    try {
      rmdirSync(dirname(launcher));
    } catch {
      // Something else lives in bin/; leave it.
    }
  }
  if (kind === "systemd") runner("systemctl", ["--user", "daemon-reload"]);
  return lines;
}
