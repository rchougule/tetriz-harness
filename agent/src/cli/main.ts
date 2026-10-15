#!/usr/bin/env node
import { mkdirSync, readFileSync, realpathSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";
import { createAgent } from "../agent.js";
import type { Logger, LogFields } from "../ports.js";
import { JsonFileStore, defaultStateDir } from "../store/jsonFileStore.js";
import { checkServerUrl, ConnectTransport } from "../transport/connect.js";
import type { EnforcementState, SyncResult } from "../types.js";
import {
  defaultServiceShell,
  installService,
  launcherPath,
  renderLauncher,
  renderService,
  serviceFilePath,
  servicePlatform,
  uninstallService,
} from "./service.js";

const USAGE = `usage: harness-agent <command> [options]

commands:
  sync                 run one reconcile pass
  watch                run a pass every --interval
  status               print the last stored state
  install-service      install a launchd (macOS) or systemd user (Linux) service running \`watch\`
  uninstall-service    stop and remove that service

options:
  --server <url>       control plane URL        (env HARNESS_AGENT_SERVER); https://, or http:// to this machine
  --insecure-http      allow http:// to another host; the token crosses the network unencrypted
  --token <token>      device token             (env HARNESS_AGENT_TOKEN, else <state-dir>/token)
  --state-dir <dir>    state directory          (env HARNESS_AGENT_STATE_DIR, default ~/.config/harness-agent)
  --interval <dur>     watch interval, e.g. 30s, 5m, 1h (default 5m)
  --project <dir>      extra checkout for repo-local artifacts (repeatable)
  --no-discover        do not discover projects from agent session history
  --print              install-service: print the launcher and unit instead of installing them
  --json               machine-readable output
  --verbose            log every event to stderr
  -h, --help           show this help

exit codes (sync): 0 ok, 1 apply failures, 2 server unreachable or pass aborted, 64 usage
`;

const EXIT_OK = 0;
const EXIT_APPLY_FAILED = 1;
const EXIT_UNAVAILABLE = 2;
const EXIT_USAGE = 64;

class UsageError extends Error {}

/** "90", "30s", "5m", "1h" → milliseconds. */
export function parseDuration(v: string): number {
  const m = /^(\d+)(ms|s|m|h)?$/.exec(v.trim());
  if (!m) throw new UsageError(`invalid duration: ${v}`);
  const n = Number(m[1]);
  const unit = m[2] ?? "s";
  const ms = unit === "ms" ? n : unit === "s" ? n * 1000 : unit === "m" ? n * 60_000 : n * 3_600_000;
  if (ms < 1000) throw new UsageError(`interval too short: ${v}`);
  return ms;
}

function stderrLogger(verbose: boolean): Logger {
  const emit = (level: string) => (event: string, fields?: LogFields) => {
    process.stderr.write(`${JSON.stringify({ time: new Date().toISOString(), level, event, ...fields })}\n`);
  };
  const quiet = () => {};
  return {
    debug: verbose ? emit("debug") : quiet,
    info: verbose ? emit("info") : quiet,
    warn: emit("warn"),
    error: emit("error"),
  };
}

function readTokenFile(path: string): string | undefined {
  try {
    const t = readFileSync(path, "utf8").trim();
    return t === "" ? undefined : t;
  } catch {
    return undefined;
  }
}

function summarize(result: SyncResult): string {
  if (!result.fetched) return `sync: enforced set unavailable (${result.fetchError ?? "unknown error"}); nothing written`;
  if (result.paused) return `sync: enforcement paused by the server; nothing written${result.reported ? "; reported" : ""}`;
  const counts = countStatuses(result.state);
  const parts = [
    `sync: ${result.artifacts} artifact(s), ${result.targets} target(s)`,
    `in_sync ${counts.in_sync}, pending ${counts.pending}, failed ${counts.failed}, ended ${counts.ended}`,
    `${result.applyFailures.length} apply failure(s)`,
  ];
  if (result.passError) parts.push(`pass aborted: ${result.passError}`);
  if (result.reportError) parts.push(`report failed: ${result.reportError}`);
  else if (result.reported) parts.push("reported");
  return parts.join("; ");
}

function countStatuses(state: EnforcementState): Record<"in_sync" | "pending" | "failed" | "ended", number> {
  const counts = { in_sync: 0, pending: 0, failed: 0, ended: 0 };
  for (const item of state.items) counts[item.status]++;
  return counts;
}

function exitCodeFor(result: SyncResult): number {
  if (!result.fetched || result.passError || result.reportError) return EXIT_UNAVAILABLE;
  if (result.applyFailures.length > 0) return EXIT_APPLY_FAILED;
  return EXIT_OK;
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolveSleep) => {
    const t = setTimeout(resolveSleep, ms);
    signal.addEventListener("abort", () => {
      clearTimeout(t);
      resolveSleep();
    }, { once: true });
  });
}

export async function main(argv: string[]): Promise<number> {
  const { values, positionals } = parseArgs({
    args: argv,
    allowPositionals: true,
    options: {
      server: { type: "string" },
      token: { type: "string" },
      "state-dir": { type: "string" },
      interval: { type: "string" },
      project: { type: "string", multiple: true },
      "no-discover": { type: "boolean" },
      "insecure-http": { type: "boolean" },
      print: { type: "boolean" },
      json: { type: "boolean" },
      verbose: { type: "boolean" },
      help: { type: "boolean", short: "h" },
    },
  });
  const command = positionals[0];
  if (values.help || command === undefined || command === "help") {
    process.stdout.write(USAGE);
    return command === undefined && !values.help ? EXIT_USAGE : EXIT_OK;
  }

  const stateDir = values["state-dir"] ?? process.env["HARNESS_AGENT_STATE_DIR"] ?? defaultStateDir();
  const tokenPath = join(stateDir, "token");
  const server = values.server ?? process.env["HARNESS_AGENT_SERVER"];
  const token = values.token ?? process.env["HARNESS_AGENT_TOKEN"] ?? readTokenFile(tokenPath);
  const log = stderrLogger(values.verbose === true);
  const store = new JsonFileStore(stateDir);

  const insecureHttp = values["insecure-http"] === true;
  const requireServer = (): string => {
    if (!server) throw new UsageError("--server (or HARNESS_AGENT_SERVER) is required");
    try {
      checkServerUrl(server, insecureHttp);
    } catch (err) {
      throw new UsageError(err instanceof Error ? err.message : String(err));
    }
    return server;
  };

  const makeAgent = () => {
    const url = requireServer();
    if (!token) throw new UsageError("--token (or HARNESS_AGENT_TOKEN) is required");
    return createAgent({
      transport: new ConnectTransport({ server: url, token, logger: log, allowInsecureHttp: insecureHttp }),
      store,
      logger: log,
      roots: { projects: values.project ?? [], discoverProjects: values["no-discover"] !== true },
    });
  };

  const print = (result: SyncResult) => {
    process.stdout.write(values.json ? `${JSON.stringify(result)}\n` : `${summarize(result)}\n`);
    for (const f of values.json ? [] : result.applyFailures) {
      process.stdout.write(`  failed ${f.artifactId}${f.repository ? ` (${f.repository})` : ""}: ${f.reason}\n`);
    }
    for (const r of values.json ? [] : result.retained) process.stdout.write(`  kept   ${join(r.root, r.sourcePath)}: ${r.reason}\n`);
  };

  switch (command) {
    case "sync": {
      const result = await makeAgent().sync();
      print(result);
      return exitCodeFor(result);
    }
    case "watch": {
      const intervalMs = parseDuration(values.interval ?? "5m");
      const agent = makeAgent();
      const stop = new AbortController();
      const onSignal = () => stop.abort();
      process.once("SIGINT", onSignal);
      process.once("SIGTERM", onSignal);
      while (!stop.signal.aborted) {
        try {
          print(await agent.sync());
        } catch (err) {
          // A store or disk error on one pass must not end the service.
          log.error("sync_failed", { error: err instanceof Error ? err.message : String(err) });
        }
        await sleep(intervalMs, stop.signal);
      }
      return EXIT_OK;
    }
    case "status": {
      const loaded = await store.load();
      const agent = createAgent({
        transport: { getEnforcedSet: unavailable, getDownloads: unavailable, report: unavailable },
        store,
      });
      const { state, lastSync } = await agent.status();
      if (values.json) {
        process.stdout.write(`${JSON.stringify({ stateFile: store.path, lastSync, state })}\n`);
        return EXIT_OK;
      }
      if (loaded === null) {
        process.stdout.write(`no state yet at ${store.path}\n`);
        return EXIT_OK;
      }
      process.stdout.write(
        lastSync
          ? `last sync ${lastSync.finishedAt}: ${lastSync.fetched ? "fetched" : `unavailable (${lastSync.fetchError ?? "?"})`}, ${lastSync.applyFailures} apply failure(s)\n`
          : "no sync recorded\n",
      );
      for (const item of state.items) {
        const where = item.targets.map((t) => t.root ?? "~").join(", ");
        process.stdout.write(`  ${item.status.padEnd(7)} ${item.name} [${item.ideType}] ${item.installPath} @ ${where}\n`);
        if (item.failureReason) process.stdout.write(`          ${item.failureReason}\n`);
      }
      for (const r of loaded.retained) process.stdout.write(`  kept    ${join(r.root, r.sourcePath)}: ${r.reason}\n`);
      return EXIT_OK;
    }
    case "install-service": {
      const kind = servicePlatform();
      if (kind === null) throw new UsageError(`install-service supports macOS and Linux, not ${process.platform}`);
      requireServer();
      const extraArgs = [
        ...(values["no-discover"] ? ["--no-discover"] : []),
        ...(insecureHttp ? ["--insecure-http"] : []),
        ...(values.project ?? []).flatMap((p) => ["--project", resolve(p)]),
      ];
      const spec = {
        shell: defaultServiceShell(),
        cliPath: realpathSync(fileURLToPath(import.meta.url)),
        server: requireServer(),
        interval: values.interval ?? "5m",
        stateDir,
        extraArgs,
      };
      parseDuration(spec.interval);
      if (values.print) {
        process.stdout.write(`# ${launcherPath(stateDir)}\n${renderLauncher(spec)}\n`);
        process.stdout.write(`# ${serviceFilePath(kind)}\n${renderService(kind, spec)}`);
        return EXIT_OK;
      }
      if (!token) throw new UsageError("--token (or HARNESS_AGENT_TOKEN) is required");
      mkdirSync(stateDir, { recursive: true, mode: 0o700 });
      writeFileSync(tokenPath, `${token}\n`, { mode: 0o600 });
      for (const line of installService(kind, spec)) process.stdout.write(`${line}\n`);
      return EXIT_OK;
    }
    case "uninstall-service": {
      const kind = servicePlatform();
      if (kind === null) throw new UsageError(`uninstall-service supports macOS and Linux, not ${process.platform}`);
      for (const line of uninstallService(kind, stateDir)) process.stdout.write(`${line}\n`);
      return EXIT_OK;
    }
    default:
      throw new UsageError(`unknown command: ${command}`);
  }
}

function unavailable(): never {
  throw new Error("offline");
}

function isEntryPoint(): boolean {
  const arg = process.argv[1];
  if (arg === undefined) return false;
  try {
    return realpathSync(arg) === realpathSync(fileURLToPath(import.meta.url));
  } catch {
    return false;
  }
}

if (isEntryPoint()) {
  main(process.argv.slice(2)).then(
    (code) => process.exit(code),
    (err: unknown) => {
      if (err instanceof UsageError || (err as { code?: string })?.code?.startsWith("ERR_PARSE_ARGS")) {
        process.stderr.write(`harness-agent: ${(err as Error).message}\n\n${USAGE}`);
        process.exit(EXIT_USAGE);
      }
      process.stderr.write(`harness-agent: ${err instanceof Error ? err.message : String(err)}\n`);
      process.exit(EXIT_UNAVAILABLE);
    },
  );
}
