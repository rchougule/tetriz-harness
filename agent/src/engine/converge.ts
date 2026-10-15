import { createHash } from "node:crypto";
import { existsSync } from "node:fs";
import { anchoredPathExists, readAnchoredFile, trustedParent } from "./anchoredFs.js";
import { resolve } from "node:path";
import {
  IDE_CURSOR,
  SCOPE_REPO_LOCAL,
  SCOPE_USER_GLOBAL,
  TYPE_HOOK,
  type EnforcedArtifact,
  type EnforcedSet,
} from "../contract.js";
import type { Transport } from "../ports.js";
import {
  activationScopeOf,
  isHookArtifact,
  resolveArtifactFiles,
  type ConvergenceTarget,
  type EnforcedFile,
  type HookArtifact,
  type TargetedArtifact,
} from "./artifacts.js";
import type { EngineContext } from "./context.js";
import { downloadArtifactBodies, type DownloadFailureKind, type FetchImpl } from "./download.js";
import {
  createdDirsFor,
  installRootForIde,
  removeEmptyCreatedDirs,
  removePlacedFile,
  resolveInstallPath,
  writeAtomic,
} from "./fsops.js";
import { buildWriteGates, gateVerdict, releaseGitExcludes, writePathsFor } from "./gitGate.js";
import { activateHook, activationFilePath, deactivateHook, ideSupportsHookActivation } from "./hookActivation.js";
import type { HookMergeResult, HookMergeSpec } from "./hookMerge.js";
import { hasLiveSession } from "./liveSession.js";
import { canonicalRepoKey, checkoutsForRepo } from "./repoCheckouts.js";
import {
  classifyReconcileKind,
  clearFailure,
  compositeKey,
  forgetArtifact,
  markConverged,
  previewOf,
  queueApplyFailure,
  readConsecutiveEmptyReads,
  readEnforcementSnapshot,
  recordReconcileFailure,
  recordReconcileSuccess,
  writeConsecutiveEmptyReads,
  writeEnforcementSnapshot,
  RETAINED_REASON,
  type RetainedFile,
  type SnapshotEntry,
} from "./state.js";

// The convergence loop. User-global artifacts resolve to one sentinel target (home, or the Codex home);
// a repo-local artifact fans out to one target per local checkout. All per-target state keys on
// (artifactId, targetKey), so N checkouts of one artifact never clobber each other.
//
// Leaving the set vs expiring: an artifact absent from the read is a removal and runs the retract
// path; one served `expired` is frozen — no write, revert or activation change — and keeps its snapshot
// entry, so a later removal still retracts with proof of what this agent placed.

type SnapshotHook = NonNullable<SnapshotEntry["hook"]>;

function live(ctx: EngineContext, ideType: string): boolean {
  return hasLiveSession(ctx.paths, ideType, ctx.now().getTime());
}

/**
 * What this agent did during the current pass, before the snapshot records it: the files it wrote and
 * the activation entry each hook owns after this pass's attempt. Ownership is proven only by a write or
 * by the previous snapshot, never by finding matching bytes on disk.
 */
interface PassRecord {
  written: Map<string, Map<string, string>>;
  /** Files left in place by an earlier retract or prune whose target names them again: still ours. */
  reclaimed: Map<string, Map<string, string>>;
  hookOwner: Map<string, SnapshotHook | null>;
}

const passRecords = new WeakMap<EngineContext, PassRecord>();

function passRecord(ctx: EngineContext): PassRecord {
  let record = passRecords.get(ctx);
  if (record === undefined) {
    record = { written: new Map(), reclaimed: new Map(), hookOwner: new Map() };
    passRecords.set(ctx, record);
  }
  return record;
}

function recordPath(byKey: Map<string, Map<string, string>>, key: string, installPath: string, hash: string): void {
  const paths = byKey.get(key) ?? new Map<string, string>();
  paths.set(installPath, hash);
  byKey.set(key, paths);
}

function recordWritten(ctx: EngineContext, key: string, installPath: string, hash: string): void {
  recordPath(passRecord(ctx).written, key, installPath, hash);
}

/** Paths this agent can prove it wrote for (artifactId, targetKey): the previous snapshot, plus this pass's writes. */
function ownedPaths(ctx: EngineContext, artifactId: string, targetKey: string): Set<string> {
  const key = compositeKey(artifactId, targetKey);
  const prev = readEnforcementSnapshot(ctx).find((e) => compositeKey(e.artifactId, e.targetKey ?? "") === key);
  const owned = new Set(prev === undefined ? [] : placedPathsOf(prev));
  for (const path of passRecord(ctx).written.get(key)?.keys() ?? []) owned.add(path);
  for (const path of passRecord(ctx).reclaimed.get(key)?.keys() ?? []) owned.add(path);
  return owned;
}

/** A dangling symlink or unreadable file still occupies the slot; unknown state is never absent. */
function pathExists(path: string, root: string): boolean {
  return anchoredPathExists(root, path);
}

export const COLLISION_REASON = "install path holds a file this agent did not write; move it away to let the agent install";

function userGlobalTarget(ctx: EngineContext, a: EnforcedArtifact): ConvergenceTarget {
  return { targetKey: "", root: installRootForIde(a.ideType, a.type, ctx.paths) };
}

/** Accepts bare artifacts (→ the user-global sentinel) as well as already-targeted ones. */
function normalizeTargeted(
  ctx: EngineContext,
  items: ReadonlyArray<EnforcedArtifact | TargetedArtifact>,
): TargetedArtifact[] {
  return items.map((it) =>
    "artifact" in it ? it : { artifact: it, target: userGlobalTarget(ctx, it) },
  );
}

export function sha256File(path: string, root: string = trustedParent(path)): string | null {
  try {
    return createHash("sha256").update(readAnchoredFile(root, path)).digest("hex");
  } catch {
    return null; // missing / unreadable reads as "not installed"
  }
}

function readPreview(path: string, root: string): string | null {
  try {
    return previewOf(readAnchoredFile(root, path));
  } catch {
    return null;
  }
}

export function toSnapshotEntry(
  ctx: EngineContext,
  a: EnforcedArtifact,
  prevPlaced: string[] = [],
  target: ConvergenceTarget = userGlobalTarget(ctx, a),
  prevPlacedHash = "",
  prevHashes: Readonly<Record<string, string>> = {},
  prevHook: SnapshotHook | null = null,
): SnapshotEntry {
  const root = target.root;
  const key = compositeKey(a.artifactId, target.targetKey);
  const written = passRecord(ctx).written.get(key) ?? new Map<string, string>();
  const reclaimed = passRecord(ctx).reclaimed.get(key) ?? new Map<string, string>();
  const prevOwned = new Set([...prevPlaced, ...reclaimed.keys()]);
  const ownedHashes: Readonly<Record<string, string>> = { ...Object.fromEntries(reclaimed), ...prevHashes };
  const desired = resolveArtifactFiles(a);
  const desiredPaths = new Set(desired.map((f) => f.installPath));
  const primaryFile = desired.find((f) => f.isPrimary) ?? desired[0];
  // Placed = files this agent can prove it wrote that are still on disk: written this pass, or recorded
  // before. A desired file already at the enforced bytes that the agent never wrote is only present:
  // reported, but never pruned, retracted or overwritten. Previously placed paths dropped from the set
  // stay placed while on disk (a prune the guard deferred), unless the prune left them to the engineer.
  const placedHashes: Record<string, string> = {};
  const placedPaths: string[] = [];
  const presentPaths: string[] = [];
  for (const f of desired) {
    const abs = resolveInstallPath(root, f.installPath);
    if (abs === null || !pathExists(abs, root)) continue;
    const writtenHash = written.get(f.installPath);
    if (writtenHash !== undefined) {
      placedPaths.push(f.installPath);
      placedHashes[f.installPath] = writtenHash;
      continue;
    }
    const onDisk = sha256File(abs, root);
    if (prevOwned.has(f.installPath)) {
      placedPaths.push(f.installPath);
      // What this agent wrote there before; a legacy entry without the map only knows matching bytes.
      const h = ownedHashes[f.installPath] ?? (onDisk === f.hash ? f.hash : undefined);
      if (h !== undefined) placedHashes[f.installPath] = h;
      continue;
    }
    if (onDisk === f.hash) presentPaths.push(f.installPath);
  }
  for (const p of prevPlaced) {
    if (desiredPaths.has(p)) continue;
    const abs = resolveInstallPath(root, p);
    if (abs === null || !pathExists(abs, root)) continue;
    if (isRetained(ctx, a.artifactId, target.targetKey, p)) continue;
    placedPaths.push(p);
    const h = prevHashes[p];
    if (h !== undefined) placedHashes[p] = h;
  }
  const primaryPlacedHash = primaryFile !== undefined ? placedHashes[primaryFile.installPath] : undefined;
  const entry: SnapshotEntry = {
    artifactId: a.artifactId,
    ideType: a.ideType,
    type: a.type,
    name: a.name,
    scope: a.scope,
    installPath: a.installPath,
    primaryHash: a.primaryHash,
    placedHash: primaryPlacedHash ?? (prevPlacedHash || a.primaryHash),
    isHook: isHookArtifact(a),
    placedPaths,
    placedHashes,
  };
  if (presentPaths.length > 0) entry.presentPaths = presentPaths;
  if (target.targetKey !== "") entry.targetKey = target.targetKey;
  if (a.scope === SCOPE_REPO_LOCAL && a.repoFullName !== "") entry.repoFullName = a.repoFullName;
  if (isHookArtifact(a)) {
    // The activation entry this agent owns: what this pass's attempt left, else what it owned before.
    // A desired identity is never recorded on its own, so a removal cannot strip a hand-written match.
    const record = passRecord(ctx).hookOwner;
    const owned = record.has(key) ? record.get(key)! : prevHook;
    if (owned !== null) entry.hook = owned;
  }
  return entry;
}

function isRetained(ctx: EngineContext, artifactId: string, targetKey: string, sourcePath: string): boolean {
  return ctx.state.retained.some((r) => r.artifactId === artifactId && r.targetKey === targetKey && r.sourcePath === sourcePath);
}

function validEnforceUntil(v: string | undefined): string | undefined {
  return v !== undefined && v.length <= 64 && Number.isFinite(Date.parse(v)) ? v : undefined;
}

/** Re-mark `entry` from this pass's served artifact, so a stale mark cannot outlive a re-enforce. */
export function withExpiryMark(entry: SnapshotEntry, artifact: EnforcedArtifact): SnapshotEntry {
  const next: SnapshotEntry = { ...entry };
  delete next.expired;
  delete next.enforceUntil;
  if (artifact.expired === true) {
    next.expired = true;
    const until = validEnforceUntil(artifact.enforceUntil);
    if (until !== undefined) next.enforceUntil = until;
  }
  return next;
}

/**
 * The snapshot entry one (artifact, target) contributes to this pass. A live artifact re-derives it
 * from disk. An expired one keeps its previous entry plus the mark, and gets none if it had none: an
 * entry is proof of placement, and a removal uses that proof to delete a matching hook entry from a
 * co-owned settings file — recording placement that never happened could strip a hand-written one.
 */
export function snapshotEntryForCycle(
  ctx: EngineContext,
  t: TargetedArtifact,
  prev: SnapshotEntry | undefined,
): SnapshotEntry | null {
  const { artifact, target } = t;
  if (artifact.expired === true) return prev === undefined ? null : withExpiryMark(prev, artifact);
  return toSnapshotEntry(
    ctx,
    artifact,
    prev === undefined ? [] : (prev.placedPaths ?? [prev.installPath]),
    target,
    prev === undefined ? "" : (prev.placedHash ?? prev.primaryHash),
    prev === undefined ? {} : installedHashes(prev),
    prev?.hook ?? null,
  );
}

/**
 * Reconcile one artifact's whole file set against its target. The live-session guard gates the whole
 * artifact; the ledger and status track only the primary. `markConvergedOnMatch` records an
 * already-on-disk set as converged — the pass sets it; the single-file backstop leaves it off.
 */
export function reconcileArtifactFiles(
  ctx: EngineContext,
  artifact: EnforcedArtifact,
  bodies: Map<string, Buffer>,
  failures: Map<string, DownloadFailureKind>,
  opts: {
    markConvergedOnMatch?: boolean;
    priorHook?: SnapshotHook | null;
    /** Where the prior activation entry was placed, when it may differ from this artifact's. */
    priorPlacement?: PriorPlacement;
    trackedPaths?: ReadonlySet<string>;
    /** Tracked paths whose working copy matches git's index: git already holds those bytes. */
    cleanTrackedPaths?: ReadonlySet<string>;
    onCreatedDirs?: (dirs: string[]) => void;
  } = {},
  target: ConvergenceTarget = userGlobalTarget(ctx, artifact),
): void {
  // The pass never sends an expired artifact here; this guards the exported entry point.
  if (artifact.expired === true) return;
  const root = target.root;
  const targetKey = target.targetKey;
  const files = resolveArtifactFiles(artifact);
  const gitTracked = files.some((f) => opts.trackedPaths?.has(f.installPath) === true);

  // An escaping path is a contract violation: fail the whole artifact rather than place a partial set.
  type Resolved = { file: EnforcedFile; absPath: string; localHash: string | null };
  const resolved: Resolved[] = [];
  for (const file of files) {
    const absPath = resolveInstallPath(root, file.installPath);
    if (absPath === null) {
      const reason = `install_path escapes root (through ".." or a symlink): ${file.installPath}`;
      recordReconcileFailure(ctx, artifact.artifactId, reason, targetKey);
      queueApplyFailure(ctx, artifact, reason);
      return;
    }
    resolved.push({ file, absPath, localHash: sha256File(absPath, root) });
  }

  const primary = resolved.find((r) => r.file.isPrimary) ?? resolved[0];
  if (primary === undefined) return;
  const prior = priorActivationOf(ctx, artifact, target, opts.priorHook ?? null, opts.priorPlacement);
  const changed = resolved.filter((r) => r.localHash !== r.file.hash);
  if (changed.length === 0) {
    if (opts.markConvergedOnMatch) {
      markConverged(ctx, artifact, readPreview(primary.absPath, root) ?? "", targetKey, gitTracked);
      // Re-assert activation even when the script is unchanged: an engineer may have deleted the entry,
      // or a matcher/timeout edit must land — neither moves the script hash. Idempotent when correct.
      if (isHookArtifact(artifact) && ideSupportsHookActivation(artifact.ideType) && !live(ctx, artifact.ideType)) {
        const result = applyHookActivationState(ctx, artifact, target, prior);
        if (!result.ok) {
          if (artifact.inactive === true) {
            // An entry we could not remove keeps a disabled hook firing: a real, actionable divergence.
            const reason = result.reason ?? "hook deactivation failed";
            recordReconcileFailure(ctx, artifact.artifactId, reason, targetKey);
            queueApplyFailure(ctx, artifact, reason);
          } else {
            // Best-effort: a co-owned file can be malformed for unrelated reasons; escalating would flap
            // an already-active hook to failed and report every pass.
            ctx.log.warn("hook_activation_reassert_failed", {
              artifactId: artifact.artifactId,
              targetKey,
              reason: result.reason,
            });
          }
        }
      }
    }
    return;
  }

  // A file this agent never wrote is someone's own: never replaced, whatever its bytes. The one exception
  // is a tracked path whose working copy git already holds, which the team committed for the org to manage.
  const owned = ownedPaths(ctx, artifact.artifactId, targetKey);
  const collisions = changed.filter(
    (r) => pathExists(r.absPath, root) && !owned.has(r.file.installPath) && opts.cleanTrackedPaths?.has(r.file.installPath) !== true,
  );
  if (collisions.length > 0) {
    const reason = `${COLLISION_REASON}: ${collisions.map((r) => r.file.installPath).join(", ")}`;
    recordReconcileFailure(ctx, artifact.artifactId, reason, targetKey);
    queueApplyFailure(ctx, artifact, reason);
    return;
  }

  // Checked before the guard so an integrity failure is reported even during a session.
  for (const r of changed) {
    if (!bodies.has(r.file.fileId)) {
      const kind = failures.get(r.file.fileId) ?? "download";
      const reason =
        kind === "hash"
          ? "content hash mismatch: downloaded body failed sha256 verification"
          : "download failed: artifact body could not be fetched";
      recordReconcileFailure(ctx, artifact.artifactId, reason, targetKey);
      queueApplyFailure(ctx, artifact, reason);
      return;
    }
  }

  // Skip the guard only when every file is absent, so the write purely creates. Hooks are excluded by
  // type: landing one activates it mid-session (a settings merge, or a tool auto-discovering the file).
  const skipGuard = artifact.type !== TYPE_HOOK && resolved.every((r) => !existsSync(r.absPath));

  if (!skipGuard && live(ctx, artifact.ideType)) {
    ctx.log.debug("guard_active_skip", { artifactId: artifact.artifactId, ideType: artifact.ideType, targetKey });
    return;
  }

  try {
    const primaryPreviousPreview = primary.localHash !== null ? readPreview(primary.absPath, root) : null;
    for (const r of changed) {
      const firstCreated = writeAtomic(r.absPath, bodies.get(r.file.fileId)!, { executable: r.file.executable, root });
      recordWritten(ctx, compositeKey(artifact.artifactId, targetKey), r.file.installPath, r.file.hash);
      opts.onCreatedDirs?.(createdDirsFor(root, firstCreated, r.absPath));
    }
    if (changed.some((r) => r.file.isPrimary)) {
      const kind = classifyReconcileKind(ctx, artifact.artifactId, primary.localHash, targetKey);
      recordReconcileSuccess(
        ctx,
        artifact.artifactId,
        {
          name: artifact.name,
          ideType: artifact.ideType,
          installPath: artifact.installPath,
          appliedHash: artifact.primaryHash,
          kind,
          previousPreview: primaryPreviousPreview,
          newPreview: previewOf(bodies.get(primary.file.fileId)!),
          gitTracked,
        },
        targetKey,
      );
      ctx.log.info("artifact_reconciled", { artifactId: artifact.artifactId, kind, files: changed.length, targetKey });
    } else {
      markConverged(ctx, artifact, readPreview(primary.absPath, root) ?? "", targetKey, gitTracked);
      ctx.log.info("artifact_helpers_reconciled", { artifactId: artifact.artifactId, files: changed.length, targetKey });
    }

    // On the write path a failed activation is escalated: a fresh placement that cannot activate is actionable.
    if (isHookArtifact(artifact) && ideSupportsHookActivation(artifact.ideType)) {
      const result = applyHookActivationState(ctx, artifact, target, prior);
      if (!result.ok) {
        const reason = `hook activation: ${result.reason}`;
        recordReconcileFailure(ctx, artifact.artifactId, reason, targetKey);
        queueApplyFailure(ctx, artifact, reason);
      }
    }
  } catch (err) {
    const reason = `write failed: ${err instanceof Error ? err.message : String(err)}`;
    recordReconcileFailure(ctx, artifact.artifactId, reason, targetKey);
    queueApplyFailure(ctx, artifact, reason);
  }
}

/** The declared (event, matcher) plus the server-derived command — also the identity a removal keys on. */
function hookSpecOf(artifact: HookArtifact): HookMergeSpec {
  return {
    event: artifact.hookConfig.event,
    matcher: artifact.hookConfig.matcher,
    command: artifact.hookCommand,
    timeoutSeconds: artifact.hookConfig.timeoutSeconds || undefined,
  };
}

/** Where a recorded activation entry lives: the script path and scope its activation file derives from. */
export interface PriorPlacement {
  installPath: string;
  scope: string;
  root: string;
}

interface PriorActivation extends PriorPlacement {
  hook: SnapshotHook;
}

function priorActivationOf(
  ctx: EngineContext,
  artifact: EnforcedArtifact,
  target: ConvergenceTarget,
  hook: SnapshotHook | null,
  placement: PriorPlacement | undefined,
): PriorActivation | null {
  if (hook === null) return null;
  return { hook, ...(placement ?? { installPath: artifact.installPath, scope: artifact.scope, root: target.root }) };
}

function identityOf(spec: HookMergeSpec): SnapshotHook {
  return { event: spec.event, matcher: spec.matcher ?? "", command: spec.command };
}

/** Same entry in the same activation file: what this agent recorded is exactly what it would write now. */
function samePlacedEntry(prior: PriorActivation, artifact: HookArtifact, root: string): boolean {
  const spec = hookSpecOf(artifact);
  if (prior.hook.event !== spec.event || prior.hook.matcher !== (spec.matcher ?? "") || prior.hook.command !== spec.command) return false;
  const was = activationFilePath(artifact.ideType, prior.root, prior.installPath, activationScopeOf(prior.scope));
  const now = activationFilePath(artifact.ideType, root, artifact.installPath, activationScopeOf(artifact.scope));
  return was !== null && was === now;
}

/**
 * Bring a placed hook's activation entry to its desired state: merged when active, retracted when
 * `inactive`. Only an entry this agent recorded placing is ever retracted: a team's own hand-written
 * entry can match (event, matcher, command). When the event, matcher, command or location changes, the
 * old owned entry is retracted before the new one goes in, so a hook never keeps firing under its old
 * identity. Records the entry this agent owns afterwards for the snapshot.
 */
function applyHookActivationState(
  ctx: EngineContext,
  artifact: HookArtifact,
  target: ConvergenceTarget,
  prior: PriorActivation | null,
): HookMergeResult {
  const key = compositeKey(artifact.artifactId, target.targetKey);
  const owners = passRecord(ctx).hookOwner;
  const spec = hookSpecOf(artifact);
  const unchanged = prior !== null && samePlacedEntry(prior, artifact, target.root);
  owners.set(key, prior?.hook ?? null);

  if (prior !== null && (artifact.inactive === true || !unchanged)) {
    const removed = deactivateHook(artifact.ideType, prior.root, prior.installPath, prior.hook, activationScopeOf(prior.scope));
    if (!removed.ok) return { ok: false, reason: `retract previous activation: ${removed.reason}` };
    owners.set(key, null);
  }
  if (artifact.inactive === true) {
    if (prior === null) ctx.log.warn("inactive_hook_retract_skipped_unproven", { artifactId: artifact.artifactId, root: target.root, event: spec.event });
    return { ok: true, changed: prior !== null };
  }

  const result = activateHook(artifact.ideType, target.root, artifact.installPath, spec, activationScopeOf(artifact.scope));
  if (!result.ok) return result;
  // An entry the merge found already there is ours only if the snapshot says so.
  owners.set(key, result.added === true || unchanged ? identityOf(spec) : null);
  return result;
}

/** Single-file backstop: reconcile against one primary body; a matching hash is a pure no-op. */
export function reconcileOneArtifact(
  ctx: EngineContext,
  artifact: EnforcedArtifact,
  body: Buffer | undefined,
  downloadFailure: DownloadFailureKind = "download",
): void {
  const files = resolveArtifactFiles(artifact);
  const primary = files.find((f) => f.isPrimary) ?? files[0];
  if (primary === undefined) return;
  const bodies = new Map<string, Buffer>();
  const failures = new Map<string, DownloadFailureKind>();
  if (body !== undefined) bodies.set(primary.fileId, body);
  else failures.set(primary.fileId, downloadFailure);
  reconcileArtifactFiles(ctx, artifact, bodies, failures, { markConvergedOnMatch: false });
}

/** A still-enforced artifact's current (scope, repo), to tell a move from a discovery miss. */
export interface EnforcedDesire {
  scope: string;
  repoFullName: string;
}

/** An entry recorded before repoFullName existed cannot prove a move, so it reads as same-repo. */
function isSameRepoTarget(cur: EnforcedDesire, recorded: string | undefined): boolean {
  if (cur.scope !== SCOPE_REPO_LOCAL) return false;
  if (recorded === undefined) return true;
  return canonicalRepoKey(cur.repoFullName) === canonicalRepoKey(recorded);
}

/** A hook whose activation entry this agent recorded placing: its retract edits a co-owned file. */
function retractEditsActivation(entry: SnapshotEntry): boolean {
  return entry.isHook === true && entry.hook != null && ideSupportsHookActivation(entry.ideType);
}

function placedPathsOf(entry: SnapshotEntry): string[] {
  return entry.placedPaths ?? [entry.installPath];
}

/** A retract that edits an activation file or deletes placed files: what the floor protects. */
function retractMutatesDisk(entry: SnapshotEntry): boolean {
  return retractEditsActivation(entry) || placedPathsOf(entry).length > 0;
}

/** What this agent wrote at each placed path; an entry recorded without the map knows only its primary. */
export function installedHashes(entry: SnapshotEntry): Record<string, string> {
  return entry.placedHashes ?? { [entry.installPath]: entry.placedHash ?? entry.primaryHash };
}

const RETAINED_UNREMOVABLE = "left in place: could not be removed";

/** Segments at the top of `root` that are the tool's own directory: `.claude/` under home, none in the Codex home. */
function toolRootDepth(ctx: EngineContext, root: string): number {
  return resolve(root) === resolve(ctx.paths.codexHome) ? 0 : 1;
}

function existingDirs(root: string, dirs: Iterable<string>): string[] {
  return [...new Set(dirs)]
    .filter((d) => {
      const abs = resolveInstallPath(root, d);
      return abs !== null && existsSync(abs);
    })
    .sort();
}

function retain(
  ctx: EngineContext,
  entry: SnapshotEntry,
  root: string,
  sourcePath: string,
  installedHash: string,
  reason: string,
  dropped = false,
): void {
  ctx.state.retained.push({
    ...(dropped ? { dropped: true } : {}),
    artifactId: entry.artifactId,
    ideType: entry.ideType,
    repoFullName: entry.repoFullName ?? "",
    targetKey: entry.targetKey ?? "",
    root,
    sourcePath,
    installedHash,
    reason,
    since: ctx.now().toISOString(),
  });
}

/**
 * Delete each placed file of a retracted target that still holds exactly what this agent wrote. A file
 * the engineer changed (or one with no recorded hash) is theirs now: it is left and retained, so it
 * keeps being reported until it is gone.
 */
function removeRetractedFiles(ctx: EngineContext, entry: SnapshotEntry): void {
  const root = entryRoot(ctx, entry);
  if (root === "") return;
  const hashes = installedHashes(entry);
  const retainedFrom = ctx.state.retained.length;
  for (const path of placedPathsOf(entry)) {
    const abs = resolveInstallPath(root, path);
    if (abs === null || !existsSync(abs)) continue;
    const installed = hashes[path];
    if (installed === undefined || sha256File(abs, root) !== installed) {
      retain(ctx, entry, root, path, installed ?? "", RETAINED_REASON);
      continue;
    }
    if (removePlacedFile(root, path)) {
      ctx.log.info("retracted_file_removed", { artifactId: entry.artifactId, path });
    } else {
      retain(ctx, entry, root, path, installed, RETAINED_UNREMOVABLE);
    }
  }
  const created = entry.createdDirs ?? [];
  removeEmptyCreatedDirs(root, created, toolRootDepth(ctx, root));
  // A file left in place keeps its folders; they go with it once it is removed.
  for (const r of ctx.state.retained.slice(retainedFrom)) {
    const above = existingDirs(root, created.filter((d) => r.sourcePath.startsWith(`${d}/`)));
    if (above.length > 0) r.createdDirs = above;
  }
}

/**
 * Re-examine files left behind by earlier retracts. One the engineer deleted, or one whose target is
 * enforced again, is dropped; one back at the installed bytes is removed once no session is live.
 */
export function reconcileRetained(
  ctx: EngineContext,
  liveKeys: ReadonlySet<string>,
  livePaths: ReadonlyMap<string, ReadonlySet<string>> = new Map(),
): void {
  const keep: RetainedFile[] = [];
  for (const r of ctx.state.retained) {
    const key = compositeKey(r.artifactId, r.targetKey);
    // A target enforced again manages its files once more; a helper dropped from a live artifact does
    // not come back with it unless the set names the path again.
    if (liveKeys.has(key) && (r.dropped !== true || livePaths.get(key)?.has(r.sourcePath) === true)) {
      if (r.installedHash !== "") recordPath(passRecord(ctx).reclaimed, key, r.sourcePath, r.installedHash);
      continue;
    }
    const abs = resolveInstallPath(r.root, r.sourcePath);
    if (abs === null || !existsSync(abs)) {
      removeEmptyCreatedDirs(r.root, r.createdDirs ?? [], toolRootDepth(ctx, r.root));
      continue;
    }
    if (r.installedHash === "" || sha256File(abs, r.root) !== r.installedHash) {
      keep.push({ ...r, reason: RETAINED_REASON });
      continue;
    }
    if (live(ctx, r.ideType)) {
      keep.push(r);
      continue;
    }
    if (removePlacedFile(r.root, r.sourcePath)) {
      ctx.log.info("retracted_file_removed", { artifactId: r.artifactId, path: r.sourcePath });
      removeEmptyCreatedDirs(r.root, r.createdDirs ?? [], toolRootDepth(ctx, r.root));
    } else {
      keep.push({ ...r, reason: RETAINED_UNREMOVABLE });
    }
  }
  ctx.state.retained = keep;
}

/**
 * Handle (artifact, target)s in the previous snapshot but absent from `currentKeys`. A genuinely
 * removed target is forgotten; for a hook exactly its recorded activation entry is retracted, and
 * every placed file still holding what this agent wrote is deleted (a changed one is retained). A
 * repo-local target that is still desired (same repo) and whose checkout still exists is a discovery
 * miss: it is deferred and returned, so the pass carries it into the rewritten snapshot and a later
 * true removal still has an entry to retract. A removal while the tool has a live session is deferred
 * the same way.
 *
 * `deferRetracts` is the blast-radius floor: disk-mutating retracts are held back one pass so a
 * transient empty or partial read cannot strip files and hooks fleet-wide. Must run against the
 * previous snapshot.
 */
export function reconcileRemovedArtifacts(
  ctx: EngineContext,
  currentKeys: Set<string>,
  enforcedById: Map<string, EnforcedDesire>,
  deferRetracts = false,
): SnapshotEntry[] {
  const deferred: SnapshotEntry[] = [];
  for (const entry of readEnforcementSnapshot(ctx)) {
    const entryTargetKey = entry.targetKey ?? "";
    if (currentKeys.has(compositeKey(entry.artifactId, entryTargetKey))) continue;

    if (entryTargetKey !== "") {
      const cur = enforcedById.get(entry.artifactId);
      const stillDesiredHere = cur !== undefined && isSameRepoTarget(cur, entry.repoFullName);
      if (stillDesiredHere && existsSync(entryTargetKey)) {
        // A `.git` entry (dir, or file for a worktree) means a real checkout discovery missed: defer
        // and leave any failure standing, since a real divergence must survive until re-attempted.
        if (existsSync(resolve(entryTargetKey, ".git"))) {
          deferred.push(entry);
          continue;
        }
        // No longer a checkout. A hook with a live activation entry needs a teardown we cannot run
        // here, so keep its record; anything else has nothing to tear down and is forgotten.
        if (entry.isHook === true && entry.hook != null) {
          deferred.push(entry);
          continue;
        }
        forgetArtifact(ctx, entry.artifactId, entryTargetKey);
        continue;
      }
    }

    if (deferRetracts && retractMutatesDisk(entry)) {
      ctx.log.debug("retract_deferred", { artifactId: entry.artifactId, targetKey: entryTargetKey, reason: "empty_response_floor" });
      deferred.push(entry);
      continue;
    }

    if (retractMutatesDisk(entry) && live(ctx, entry.ideType)) {
      ctx.log.debug("retract_deferred", { artifactId: entry.artifactId, targetKey: entryTargetKey, reason: "live_session" });
      deferred.push(entry);
      continue;
    }

    if (entry.isHook && entry.hook && ideSupportsHookActivation(entry.ideType)) {
      const result = deactivateHook(
        entry.ideType,
        entryRoot(ctx, entry),
        entry.installPath,
        { event: entry.hook.event, matcher: entry.hook.matcher, command: entry.hook.command },
        activationScopeOf(entry.scope),
      );
      if (!result.ok) {
        // Keep the whole record, script included, so the next pass retries the teardown: forgetting it
        // would leave the entry firing with nothing left to prove it was ours.
        ctx.log.warn("hook_removal_failed", { artifactId: entry.artifactId, targetKey: entryTargetKey, reason: result.reason });
        deferred.push(entry);
        continue;
      }
      if (result.changed) ctx.log.info("hook_removed", { artifactId: entry.artifactId });
    }
    removeRetractedFiles(ctx, entry);

    forgetArtifact(ctx, entry.artifactId, entryTargetKey);
  }
  return deferred;
}

/**
 * Delete files dropped from a still-enforced artifact's set (a helper removed from a skill). Only paths
 * this agent recorded placing are touched, and only while they still hold exactly what it wrote: a
 * helper the engineer edited (or one with no recorded hash) is left and retained, as a retract does.
 * A whole artifact removal is `reconcileRemovedArtifacts`' job. Must run against the previous snapshot.
 */
export function pruneDroppedFiles(ctx: EngineContext, current: ReadonlyArray<EnforcedArtifact | TargetedArtifact>): void {
  const targeted = normalizeTargeted(ctx, current);
  const currentByKey = new Map(targeted.map((t) => [compositeKey(t.artifact.artifactId, t.target.targetKey), t]));
  for (const prev of readEnforcementSnapshot(ctx)) {
    const now = currentByKey.get(compositeKey(prev.artifactId, prev.targetKey ?? ""));
    if (now === undefined) continue;
    if (now.artifact.expired === true) continue;
    // Never mutate a live tree; the prune resumes once no session is live.
    if (live(ctx, now.artifact.ideType)) continue;
    const currentPaths = new Set(resolveArtifactFiles(now.artifact).map((f) => f.installPath));
    const root = now.target.root;
    const hashes = installedHashes(prev);
    for (const path of placedPathsOf(prev)) {
      if (currentPaths.has(path)) continue;
      const abs = resolveInstallPath(root, path);
      if (abs === null || !pathExists(abs, root)) continue;
      if (isRetained(ctx, prev.artifactId, prev.targetKey ?? "", path)) continue;
      const installed = hashes[path];
      if (installed === undefined || sha256File(abs, root) !== installed) {
        retain(ctx, prev, root, path, installed ?? "", RETAINED_REASON, true);
        ctx.log.info("dropped_file_retained", { artifactId: prev.artifactId, path });
        continue;
      }
      if (removePlacedFile(root, path)) ctx.log.info("dropped_file_pruned", { artifactId: prev.artifactId, path });
      else retain(ctx, prev, root, path, installed, RETAINED_UNREMOVABLE, true);
    }
  }
}

/**
 * Consecutive floor-triggering reads absorbed before retracts progress. 1 = hold only the first; a
 * confirmed second is a genuine "admin removed it" and converges rather than deferring forever.
 */
const EMPTY_RESPONSE_DEFER_CYCLES = 1;

/** Pure floor decision; the streak is clamped so a permanently empty org cannot grow it unbounded. */
export function emptyResponseFloorDecision(
  isEmpty: boolean,
  prevHadArtifacts: boolean,
  prevEmptyStreak: number,
): { deferRetracts: boolean; nextEmptyStreak: number } {
  const nextEmptyStreak = isEmpty ? Math.min(prevEmptyStreak + 1, EMPTY_RESPONSE_DEFER_CYCLES + 1) : 0;
  const deferRetracts = isEmpty && prevHadArtifacts && nextEmptyStreak <= EMPTY_RESPONSE_DEFER_CYCLES;
  return { deferRetracts, nextEmptyStreak };
}

/**
 * The floor's trigger: a previous entry whose retract would delete files or edit a co-owned activation
 * file is absent this pass. Broader than "the read was empty", so a partial drop with the same blast
 * radius is caught.
 */
export function diskRetracting(prevSnapshot: SnapshotEntry[], currentKeys: Set<string>): boolean {
  return prevSnapshot.some(
    (e) => retractMutatesDisk(e) && !currentKeys.has(compositeKey(e.artifactId, e.targetKey ?? "")),
  );
}

function priorPlacementOf(ctx: EngineContext, prev: SnapshotEntry | undefined): PriorPlacement | undefined {
  return prev === undefined ? undefined : { installPath: prev.installPath, scope: prev.scope, root: entryRoot(ctx, prev) };
}

/** The root a snapshot entry was placed under, as reconcileArtifactFiles wrote against. */
export function entryRoot(ctx: EngineContext, entry: SnapshotEntry): string {
  return entry.scope === SCOPE_REPO_LOCAL ? (entry.targetKey ?? "") : installRootForIde(entry.ideType, entry.type, ctx.paths);
}

export interface PassInput {
  set: EnforcedSet;
  transport: Pick<Transport, "getDownloads">;
  /** canonical repo key → checkout top-levels; called only when a repo-local artifact is served. */
  resolveCheckouts: () => Promise<Map<string, string[]>>;
  fetchImpl?: FetchImpl;
}

export interface PassOutcome {
  artifacts: number;
  targets: number;
}

/** One reconcile pass against a freshly read enforced set. Mutates `ctx.state`; persistence is the caller's. */
export async function runPass(ctx: EngineContext, input: PassInput): Promise<PassOutcome> {
  passRecords.delete(ctx);
  const artifacts = input.set.artifacts;
  const home = ctx.paths.home;
  // A Cursor artifact is skipped on a machine without Cursor, so no ~/.cursor state is created there.
  const hasCursor = existsSync(resolve(home, ".cursor"));

  const repoCheckouts = artifacts.some((a) => a.scope === SCOPE_REPO_LOCAL)
    ? await input.resolveCheckouts()
    : new Map<string, string[]>();

  const targeted: TargetedArtifact[] = [];
  for (const artifact of artifacts) {
    if (artifact.ideType === IDE_CURSOR && !hasCursor) continue;
    if (artifact.scope === SCOPE_USER_GLOBAL) {
      targeted.push({ artifact, target: userGlobalTarget(ctx, artifact) });
    } else if (artifact.scope === SCOPE_REPO_LOCAL) {
      // A repo with no local checkout resolves to zero targets: nothing to converge here.
      for (const topLevel of checkoutsForRepo(repoCheckouts, artifact.repoFullName)) {
        targeted.push({ artifact, target: { targetKey: topLevel, root: topLevel } });
      }
    }
  }
  const keyOf = (t: TargetedArtifact): string => compositeKey(t.artifact.artifactId, t.target.targetKey);
  // Every machine-touching step reads `liveTargets`; `targeted` (expired included) feeds only the
  // removal comparison and the snapshot rewrite.
  const liveTargets = targeted.filter((t) => t.artifact.expired !== true);
  const servedById = new Map(artifacts.map((a) => [a.artifactId, a]));
  const enforcedById = new Map<string, EnforcedDesire>(
    artifacts.map((a) => [a.artifactId, { scope: a.scope, repoFullName: a.repoFullName }]),
  );

  const prevSnapshot = readEnforcementSnapshot(ctx);
  const currentKeys = new Set(targeted.map(keyOf));
  const isRetracting = diskRetracting(prevSnapshot, currentKeys);
  const { deferRetracts: retractFloor, nextEmptyStreak } = emptyResponseFloorDecision(
    isRetracting,
    prevSnapshot.length > 0,
    readConsecutiveEmptyReads(ctx),
  );
  writeConsecutiveEmptyReads(ctx, nextEmptyStreak);
  if (retractFloor) {
    ctx.log.info("retract_floor_engaged", { streak: nextEmptyStreak, snapshotSize: prevSnapshot.length });
  } else if (isRetracting) {
    ctx.log.info("retract_floor_progressing", { streak: nextEmptyStreak });
  }
  reconcileRetained(
    ctx,
    new Set(liveTargets.map(keyOf)),
    new Map(liveTargets.map((t) => [keyOf(t), new Set(resolveArtifactFiles(t.artifact).map((f) => f.installPath))])),
  );
  const deferredEntries = reconcileRemovedArtifacts(ctx, currentKeys, enforcedById, retractFloor);
  pruneDroppedFiles(ctx, liveTargets);
  // Nothing is retried for a frozen target, so a failure recorded before the window closed would read
  // "needs attention" forever.
  for (const { artifact, target } of targeted) {
    if (artifact.expired === true) clearFailure(ctx, artifact.artifactId, target.targetKey);
  }

  const prevByKey = new Map(prevSnapshot.map((e) => [compositeKey(e.artifactId, e.targetKey ?? ""), e]));
  const prevHookByKey = new Map(
    prevSnapshot.flatMap((e) => (e.hook ? [[compositeKey(e.artifactId, e.targetKey ?? ""), e.hook] as const] : [])),
  );

  // The gate is established before the fetch so a gated-out target costs no download.
  const writeGates = await buildWriteGates(liveTargets, ctx.log, ctx.state.gitExcludes);
  const gated = liveTargets.map((t) => ({ ...t, verdict: gateVerdict(writeGates, t) }));

  // One fetch per distinct file id, across every target.
  const toFetch = new Map<string, string>();
  for (const { artifact, target, verdict } of gated) {
    if (verdict.kind !== "write") continue;
    for (const file of resolveArtifactFiles(artifact)) {
      const absPath = resolveInstallPath(target.root, file.installPath);
      if (absPath === null) continue; // reconcileArtifactFiles records the failure
      if (sha256File(absPath, target.root) !== file.hash) toFetch.set(file.fileId, file.hash);
    }
  }

  const { bodies, failed } =
    toFetch.size > 0
      ? await downloadArtifactBodies(toFetch, input.transport, ctx.log, input.fetchImpl)
      : { bodies: new Map<string, Buffer>(), failed: new Map<string, DownloadFailureKind>() };

  const createdByKey = new Map<string, string[]>();
  for (const { artifact, target, verdict } of gated) {
    if (verdict.kind === "defer") {
      // Usually a persistent git condition; this line answers "why is this not applying".
      ctx.log.warn("write_gate_deferred", { artifactId: artifact.artifactId, targetKey: target.targetKey, root: target.root });
      continue;
    }
    reconcileArtifactFiles(
      ctx,
      artifact,
      bodies,
      failed,
      {
        markConvergedOnMatch: true,
        priorHook: prevHookByKey.get(keyOf({ artifact, target })) ?? null,
        priorPlacement: priorPlacementOf(ctx, prevByKey.get(keyOf({ artifact, target }))),
        trackedPaths: writeGates.get(target.root)?.trackedPaths,
        cleanTrackedPaths: writeGates.get(target.root)?.cleanTrackedPaths,
        onCreatedDirs: (dirs) => {
          const key = keyOf({ artifact, target });
          createdByKey.set(key, [...(createdByKey.get(key) ?? []), ...dirs]);
        },
      },
      target,
    );
  }

  // Written after the reconcile loop so placedPaths reflects this pass's writes. Deferred entries are
  // carried forward (re-marked from this read) so a later true removal still has one to retract.
  writeEnforcementSnapshot(ctx, [
    ...targeted.flatMap((t) => {
      const prev = prevByKey.get(keyOf(t));
      const entry = snapshotEntryForCycle(ctx, t, prev);
      if (entry === null) return [];
      const dirs = existingDirs(t.target.root, [...(prev?.createdDirs ?? []), ...(createdByKey.get(keyOf(t)) ?? [])]);
      if (dirs.length > 0) entry.createdDirs = dirs;
      else delete entry.createdDirs;
      return [entry];
    }),
    ...deferredEntries.map((e) => {
      const served = servedById.get(e.artifactId);
      return served === undefined ? e : withExpiryMark(e, served);
    }),
  ]);

  // After the writes and retracts, so a line goes only once its path is gone and no live target needs it.
  const claimed = new Map<string, Set<string>>();
  for (const { artifact, target } of liveTargets) {
    if (artifact.scope !== SCOPE_REPO_LOCAL) continue;
    const set = claimed.get(target.root) ?? new Set<string>();
    for (const path of writePathsFor(artifact)) set.add(path);
    claimed.set(target.root, set);
  }
  releaseGitExcludes(ctx.state.gitExcludes, claimed, ctx.log);

  return { artifacts: artifacts.length, targets: targeted.length };
}
