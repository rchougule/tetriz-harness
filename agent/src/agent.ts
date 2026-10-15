import { createHash, randomUUID } from "node:crypto";
import { realpathSync } from "node:fs";
import { resolve } from "node:path";
import { FAILURE_REASON_MAX, type ApplyFailure, type EnforcedSet, type InstalledFile } from "./contract.js";
import { defaultPaths, noopLogger, type AgentPaths, type EngineContext } from "./engine/context.js";
import { entryRoot, runPass, sha256File } from "./engine/converge.js";
import type { FetchImpl } from "./engine/download.js";
import { resolveInstallPath } from "./engine/fsops.js";
import { scrubLocalPaths, type PathRoot } from "./engine/redact.js";
import { discoverProjectRoots, reportedRepository, resolveRepoCheckouts, type GitRunner } from "./engine/repoCheckouts.js";
import {
  appendHistory,
  buildEnforcementState,
  emptyState,
  normalizeState,
  takePendingApplyFailures,
  type AgentState,
  type SyncRecord,
} from "./engine/state.js";
import type { Clock, Logger, Store, Transport } from "./ports.js";
import type { EnforcementState, SyncResult } from "./types.js";
import { AGENT_VERSION } from "./version.js";

export interface AgentRoots extends Partial<AgentPaths> {
  /** Extra checkout dirs to consider for repo-local artifacts. */
  projects?: string[];
  /** Also discover projects from agent session history (default true). */
  discoverProjects?: boolean;
}

export interface AgentOptions {
  transport: Transport;
  store: Store;
  logger?: Logger;
  clock?: Clock;
  roots?: AgentRoots;
  /** Test seams. */
  fetch?: FetchImpl;
  git?: GitRunner;
}

export type UpdatedListener = (state: EnforcementState, result: SyncResult) => void;

export interface Agent {
  /** One reconcile pass. Concurrent calls share the in-flight pass. */
  sync(): Promise<SyncResult>;
  on(event: "updated", listener: UpdatedListener): () => void;
  /** The last persisted state, without running a pass. */
  status(): Promise<{ state: EnforcementState; lastSync: SyncRecord | null }>;
}

function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/**
 * Hashes of every file the snapshot records this agent placing or finding already in place, plus files
 * of retracted artifacts it left in place, as found on disk now.
 */
export function buildInstalledReport(ctx: EngineContext): InstalledFile[] {
  const seen = new Set<string>();
  const out: InstalledFile[] = [];
  const add = (ide: string, repo: string, root: string, sourcePath: string) => {
    const abs = resolveInstallPath(root, sourcePath);
    if (abs === null) return;
    const contentHash = sha256File(abs, root);
    if (contentHash === null) return;
    const item: InstalledFile = { ide, repo, checkout: repo === "" ? "" : checkoutKey(root), sourcePath, contentHash };
    const key = JSON.stringify([ide, repo, root, sourcePath]);
    if (seen.has(key)) return;
    seen.add(key);
    out.push(item);
  };
  for (const entry of ctx.state.snapshot) {
    const root = entryRoot(ctx, entry);
    if (root === "") continue;
    const repo = reportedRepository(entry.repoFullName);
    for (const sourcePath of entry.placedPaths ?? [entry.installPath]) add(entry.ideType, repo, root, sourcePath);
    for (const sourcePath of entry.presentPaths ?? []) add(entry.ideType, repo, root, sourcePath);
  }
  for (const r of ctx.state.retained) add(r.ideType, reportedRepository(r.repoFullName), r.root, r.sourcePath);
  return out;
}

/** A checkout's report key: derived from its top-level so it is stable, hashed so no local path leaves the machine. */
export function checkoutKey(topLevel: string): string {
  return createHash("sha256").update(topLevel).digest("hex").slice(0, 16);
}

/** One report failure per retracted placement with files left behind, so its removal reads pending with a reason. */
function retainedFailures(state: AgentState): ApplyFailure[] {
  const byPlacement = new Map<string, ApplyFailure>();
  for (const r of state.retained) {
    const repository = reportedRepository(r.repoFullName);
    const key = JSON.stringify([r.artifactId, repository]);
    const f = byPlacement.get(key);
    if (f === undefined) {
      byPlacement.set(key, { artifactId: r.artifactId, repository, contentHash: "", reason: `${r.reason}: ${r.sourcePath}` });
    } else if (f.reason.length < FAILURE_REASON_MAX) {
      f.reason = `${f.reason}, ${r.sourcePath}`.slice(0, FAILURE_REASON_MAX);
    }
  }
  return Array.from(byPlacement.values());
}

/**
 * The roots a reported reason is rewritten against: home, and every checkout this machine knows as
 * its repository. Each is listed under its real path too, since git answers with the resolved one.
 */
export function reportPathRoots(paths: AgentPaths, checkouts: Map<string, string[]>, state: AgentState): PathRoot[] {
  const roots: PathRoot[] = [];
  const add = (prefix: string, replacement: string) => {
    roots.push({ prefix, replacement });
    try {
      const real = realpathSync(prefix);
      if (real !== prefix) roots.push({ prefix: real, replacement });
    } catch {
      // A root that is gone can still appear in an error text by the name it had.
    }
  };
  add(paths.home, "~");
  for (const [repo, topLevels] of checkouts) for (const topLevel of topLevels) add(topLevel, repo);
  for (const e of state.snapshot) {
    const repo = reportedRepository(e.repoFullName);
    if (e.targetKey && repo !== "") add(e.targetKey, repo);
  }
  for (const r of state.retained) {
    const repo = reportedRepository(r.repoFullName);
    if (r.targetKey !== "" && repo !== "") add(r.targetKey, repo);
  }
  return roots;
}

function isEnforcedSet(v: unknown): v is EnforcedSet {
  return typeof v === "object" && v !== null && Array.isArray((v as { artifacts?: unknown }).artifacts);
}

export function createAgent(opts: AgentOptions): Agent {
  const log = opts.logger ?? noopLogger;
  const now = opts.clock ?? (() => new Date());
  const roots = opts.roots ?? {};
  const paths = defaultPaths(roots);
  const extraProjects = (roots.projects ?? []).map((p) => resolve(p));
  const listeners = new Set<UpdatedListener>();
  let inflight: Promise<SyncResult> | null = null;

  async function loadState(): Promise<AgentState> {
    const loaded = await opts.store.load();
    return loaded === null ? emptyState() : normalizeState(loaded);
  }

  async function lockedPass(): Promise<SyncResult> {
    const release = opts.store.lock ? await opts.store.lock() : null;
    try {
      return await pass();
    } finally {
      await release?.();
    }
  }

  async function pass(): Promise<SyncResult> {
    const startedAt = now().toISOString();
    const ctx: EngineContext = { state: await loadState(), log, now, paths };
    ctx.state.deviceId ??= randomUUID();

    // Resolved at most once per pass: the reconcile needs it for repo-local targets, the report for
    // known_repositories.
    let checkouts: Promise<Map<string, string[]>> | null = null;
    const resolveCheckouts = (): Promise<Map<string, string[]>> =>
      (checkouts ??= discoverProjectRoots(paths, extraProjects, { sessions: roots.discoverProjects !== false, log }).then(
        (candidates) => resolveRepoCheckouts(candidates, { git: opts.git }),
      ));

    let set: EnforcedSet | null = null;
    let fetchError: string | undefined;
    try {
      const got: unknown = await opts.transport.getEnforcedSet();
      if (isEnforcedSet(got)) set = got;
      else fetchError = "malformed enforced set";
    } catch (err) {
      fetchError = errorMessage(err);
    }

    let artifacts = 0;
    let targets = 0;
    let applyFailures: ApplyFailure[] = [];
    let reported = false;
    let reportError: string | undefined;
    let passError: string | undefined;

    const paused = set?.paused === true;
    if (set === null) {
      // No fresh desired state: no writes this pass.
      log.warn("enforced_set_unavailable", { error: fetchError });
    } else if (paused) {
      // The kill switch: nothing is written or retracted, and the empty list is not a removal.
      log.info("enforcement_paused");
    } else {
      try {
        const outcome = await runPass(ctx, {
          set,
          transport: opts.transport,
          fetchImpl: opts.fetch,
          resolveCheckouts,
        });
        artifacts = outcome.artifacts;
        targets = outcome.targets;
      } catch (err) {
        // An aborted pass keeps what it already did (retracts, queued failures, the floor streak) but
        // not the snapshot rewrite, so the next pass re-derives from the previous snapshot.
        passError = errorMessage(err);
        log.error("pass_failed", { error: passError });
      }
    }

    // Every answered read is followed by a full report: the server replaces the last one with it, so
    // a partial report would read as files having vanished.
    if (set !== null) {
      applyFailures = takePendingApplyFailures(ctx);
      try {
        const known = await resolveCheckouts();
        const pathRoots = reportPathRoots(paths, known, ctx.state);
        await opts.transport.report({
          deviceId: ctx.state.deviceId,
          agentVersion: AGENT_VERSION,
          knownRepositories: Array.from(known.keys()).sort(),
          installed: buildInstalledReport(ctx),
          applyFailures: [...applyFailures, ...retainedFailures(ctx.state)].map((f) => ({
            ...f,
            reason: scrubLocalPaths(f.reason, pathRoots).slice(0, FAILURE_REASON_MAX),
          })),
        });
        reported = true;
      } catch (err) {
        // Not retried: the next pass reports the full state again.
        reportError = errorMessage(err);
        log.warn("report_failed", { error: reportError });
      }
    }

    const finishedAt = now().toISOString();
    const record: SyncRecord = {
      startedAt,
      finishedAt,
      fetched: set !== null,
      artifacts,
      targets,
      applyFailures: applyFailures.length,
      reported,
    };
    if (fetchError !== undefined) record.fetchError = fetchError;
    if (passError !== undefined) record.passError = passError;
    if (reportError !== undefined) record.reportError = reportError;
    appendHistory(ctx.state, record);
    await opts.store.save(ctx.state);

    const state = buildEnforcementState(ctx);
    const result: SyncResult = {
      startedAt,
      finishedAt,
      fetched: set !== null,
      paused,
      artifacts,
      targets,
      applyFailures,
      retained: ctx.state.retained.map((r) => ({
        artifactId: r.artifactId,
        root: r.root,
        sourcePath: r.sourcePath,
        reason: r.reason,
      })),
      reported,
      state,
    };
    if (fetchError !== undefined) result.fetchError = fetchError;
    if (passError !== undefined) result.passError = passError;
    if (reportError !== undefined) result.reportError = reportError;

    for (const listener of listeners) {
      try {
        listener(state, result);
      } catch (err) {
        log.warn("listener_failed", { error: errorMessage(err) });
      }
    }
    return result;
  }

  return {
    sync(): Promise<SyncResult> {
      if (inflight === null) {
        inflight = lockedPass().finally(() => {
          inflight = null;
        });
      }
      return inflight;
    },
    on(event, listener) {
      if (event !== "updated") throw new Error(`unknown event: ${String(event)}`);
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    async status() {
      const state = await loadState();
      const ctx = { state, now };
      return { state: buildEnforcementState(ctx), lastSync: state.history.at(-1) ?? null };
    },
  };
}
