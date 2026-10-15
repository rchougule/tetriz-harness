import { FAILURE_REASON_MAX, type ApplyFailure } from "../contract.js";
import type { Clock } from "../ports.js";
import { reportedRepository } from "./repoCheckouts.js";
import type {
  EnforcementDiff,
  EnforcementItem,
  EnforcementItemStatus,
  EnforcementState,
  ReconcileKind,
} from "../types.js";

// The agent's own memory of what it last converged to (to tell an "updated" definition from a
// "reverted" local edit), plus the failures queued for the next report. Persisted whole by a Store.

export const STATE_VERSION = 1;

// A failure must recur this many consecutive passes before it reads as "persistent".
const PERSISTENT_FAILURE_STREAK = 2;

// Display bound for the before/after preview, not a security boundary.
const PREVIEW_MAX_BYTES = 4000;

const HISTORY_MAX = 50;

// Joins artifactId and targetKey; NUL appears in neither an id nor a filesystem path.
const TARGET_KEY_DELIM = "\u0000";

export interface SnapshotEntry {
  artifactId: string;
  ideType: string;
  type: string;
  name: string;
  scope: string;
  installPath: string;
  primaryHash: string;
  isHook: boolean;
  /** "" or absent is the user-global target; a repo-local entry carries the checkout top-level. */
  targetKey?: string;
  /** The repo a repo-local target was placed for, to tell a discovery miss from a move. */
  repoFullName?: string;
  /** Scope-root-relative paths this agent placed; absent means [installPath]. */
  placedPaths?: string[];
  /** Desired paths already holding the enforced bytes that this agent never wrote: reported, never removed. */
  presentPaths?: string[];
  /** Hash of the primary body last actually written; differs from primaryHash while a write is deferred. */
  placedHash?: string;
  /** placedPaths → the hash this agent wrote there; a retract deletes a path only while it still matches. */
  placedHashes?: Record<string, string>;
  /** Root-relative directories this agent created for the placement; a retract removes them once empty. */
  createdDirs?: string[];
  /** For a hook: the activation entry identity, so a removal retracts exactly that entry. */
  hook?: { event: string; matcher: string; command: string };
  expired?: boolean;
  enforceUntil?: string;
}

export interface LedgerEntry {
  name: string;
  ideType: string;
  installPath: string;
  appliedHash: string;
  kind: ReconcileKind;
  reconciledAt: string;
  previousPreview: string | null;
  newPreview: string;
  /** Recomputed every pass from git's answer, never carried forward. */
  gitTracked?: boolean;
}

export interface FailureEntry {
  reason: string;
  since: string;
  streak: number;
}

export interface SyncRecord {
  startedAt: string;
  finishedAt: string;
  fetched: boolean;
  fetchError?: string;
  passError?: string;
  artifacts: number;
  targets: number;
  applyFailures: number;
  reported: boolean;
  reportError?: string;
}

/**
 * A file of a retracted artifact left on disk because it no longer holds what this agent wrote. It is
 * reported until it is gone, so the server keeps the removal pending with a reason.
 */
export interface RetainedFile {
  artifactId: string;
  ideType: string;
  /** "owner/repo" of a repo-local placement; "" for user-global. */
  repoFullName: string;
  /** "" for user-global, else the checkout top-level. */
  targetKey: string;
  root: string;
  sourcePath: string;
  /** What this agent wrote there; "" when it was never recorded. */
  installedHash: string;
  reason: string;
  since: string;
  /** The placement's created directories above this file, removed once it is gone. */
  createdDirs?: string[];
  /** Dropped from a still-enforced artifact's file set, so the artifact staying live does not clear it. */
  dropped?: boolean;
}

/**
 * Lines this agent appended to one git exclude file, and the checkouts that resolve to it (linked
 * worktrees share one). Only these lines are ever removed; a line already present was not ours.
 */
export interface GitExcludeRecord {
  file: string;
  roots: string[];
  lines: string[];
}

export const RETAINED_REASON = "left in place: modified since installed";

export interface AgentState {
  version: number;
  snapshot: SnapshotEntry[];
  retained: RetainedFile[];
  gitExcludes: GitExcludeRecord[];
  ledger: Record<string, LedgerEntry>;
  failures: Record<string, FailureEntry>;
  pendingApplyFailures: ApplyFailure[];
  consecutiveEmptyReads: number;
  /** Random per-state-dir id sent with every report; an attribute, never an identity. */
  deviceId?: string;
  history: SyncRecord[];
}

export interface StateContext {
  state: AgentState;
  now: Clock;
}

export function emptyState(): AgentState {
  return {
    version: STATE_VERSION,
    snapshot: [],
    retained: [],
    gitExcludes: [],
    ledger: {},
    failures: {},
    pendingApplyFailures: [],
    consecutiveEmptyReads: 0,
    history: [],
  };
}

const isRecord = (v: unknown): v is Record<string, unknown> =>
  v !== null && typeof v === "object" && !Array.isArray(v);

const isStringArray = (v: unknown): v is string[] => Array.isArray(v) && v.every((x) => typeof x === "string");

function isGitExcludeRecord(v: unknown): v is GitExcludeRecord {
  return isRecord(v) && typeof v["file"] === "string" && isStringArray(v["roots"]) && isStringArray(v["lines"]);
}

/** Field-by-field recovery of a loaded document: a malformed field falls back to its empty value. */
export function normalizeState(raw: unknown): AgentState {
  const out = emptyState();
  if (!isRecord(raw)) return out;
  if (Array.isArray(raw["snapshot"])) out.snapshot = raw["snapshot"] as SnapshotEntry[];
  if (Array.isArray(raw["retained"])) out.retained = raw["retained"] as RetainedFile[];
  if (Array.isArray(raw["gitExcludes"])) out.gitExcludes = (raw["gitExcludes"] as unknown[]).filter(isGitExcludeRecord);
  if (isRecord(raw["ledger"])) out.ledger = raw["ledger"] as Record<string, LedgerEntry>;
  if (isRecord(raw["failures"])) out.failures = raw["failures"] as Record<string, FailureEntry>;
  if (Array.isArray(raw["pendingApplyFailures"])) out.pendingApplyFailures = raw["pendingApplyFailures"] as ApplyFailure[];
  const streak = raw["consecutiveEmptyReads"];
  if (typeof streak === "number" && Number.isInteger(streak) && streak >= 0) out.consecutiveEmptyReads = streak;
  if (typeof raw["deviceId"] === "string" && raw["deviceId"] !== "") out.deviceId = raw["deviceId"];
  if (Array.isArray(raw["history"])) out.history = (raw["history"] as SyncRecord[]).slice(-HISTORY_MAX);
  return out;
}

export function appendHistory(state: AgentState, record: SyncRecord): void {
  state.history.push(record);
  if (state.history.length > HISTORY_MAX) state.history.splice(0, state.history.length - HISTORY_MAX);
}

export function previewOf(content: Buffer | string): string {
  const s = typeof content === "string" ? content : content.toString("utf8");
  return s.length > PREVIEW_MAX_BYTES ? s.slice(0, PREVIEW_MAX_BYTES) : s;
}

/** The user-global target ("") keys on the bare artifactId. */
export function compositeKey(artifactId: string, targetKey: string): string {
  return targetKey === "" ? artifactId : `${artifactId}${TARGET_KEY_DELIM}${targetKey}`;
}

// ── Snapshot ────────────────────────────────────────────────────────────────

// Copied in and out so callers get value semantics, as from a serialized store.

/** Replaced wholesale: every read answers with the full current set. */
export function writeEnforcementSnapshot(ctx: StateContext, entries: SnapshotEntry[]): void {
  ctx.state.snapshot = structuredClone(entries);
}

export function readEnforcementSnapshot(ctx: StateContext): SnapshotEntry[] {
  return structuredClone(ctx.state.snapshot);
}

// ── Consecutive empty-read counter (the blast-radius floor) ─────────────────

export function readConsecutiveEmptyReads(ctx: StateContext): number {
  return ctx.state.consecutiveEmptyReads;
}

export function writeConsecutiveEmptyReads(ctx: StateContext, n: number): void {
  ctx.state.consecutiveEmptyReads = n;
}

// ── Ledger ──────────────────────────────────────────────────────────────────

export function readLedgerEntry(ctx: StateContext, artifactId: string, targetKey = ""): LedgerEntry | null {
  const entry = ctx.state.ledger[compositeKey(artifactId, targetKey)];
  return entry === undefined ? null : { ...entry };
}

/**
 * updated iff the file right before this write is exactly what this agent last applied; any other
 * prior content reads as reverted (a local edit), and no prior file as installed.
 */
export function classifyReconcileKind(
  ctx: StateContext,
  artifactId: string,
  localHashBeforeWrite: string | null,
  targetKey = "",
): ReconcileKind {
  if (localHashBeforeWrite === null) return "installed";
  const entry = readLedgerEntry(ctx, artifactId, targetKey);
  return entry !== null && entry.appliedHash === localHashBeforeWrite ? "updated" : "reverted";
}

export function recordReconcileSuccess(
  ctx: StateContext,
  artifactId: string,
  entry: Omit<LedgerEntry, "reconciledAt">,
  targetKey = "",
): void {
  ctx.state.ledger[compositeKey(artifactId, targetKey)] = { ...entry, reconciledAt: ctx.now().toISOString() };
  clearFailure(ctx, artifactId, targetKey);
}

/**
 * Record bytes that already match as converged, so a file that was already right (or was fixed by
 * hand) stops reading pending/failed. Idempotent: the reconciled time does not tick once recorded.
 */
export function markConverged(
  ctx: StateContext,
  artifact: { artifactId: string; name: string; ideType: string; installPath: string; primaryHash: string },
  newPreview: string,
  targetKey = "",
  gitTracked = false,
): void {
  const key = compositeKey(artifact.artifactId, targetKey);
  const hadFailure = key in ctx.state.failures;
  const existing = ctx.state.ledger[key] ?? null;
  if (
    !hadFailure &&
    existing !== null &&
    existing.appliedHash === artifact.primaryHash &&
    (existing.gitTracked === true) === gitTracked
  ) {
    return;
  }
  ctx.state.ledger[key] = {
    name: artifact.name,
    ideType: artifact.ideType,
    installPath: artifact.installPath,
    appliedHash: artifact.primaryHash,
    kind: existing?.kind ?? "installed",
    // Keep the last real action's timestamp when only the tracked-state flag moved.
    reconciledAt:
      existing !== null && existing.appliedHash === artifact.primaryHash && !hadFailure
        ? existing.reconciledAt
        : ctx.now().toISOString(),
    previousPreview: existing?.previousPreview ?? null,
    newPreview,
    gitTracked,
  };
  clearFailure(ctx, artifact.artifactId, targetKey);
}

/** Stop managing one (artifact, target). */
export function forgetArtifact(ctx: StateContext, artifactId: string, targetKey = ""): void {
  delete ctx.state.ledger[compositeKey(artifactId, targetKey)];
  clearFailure(ctx, artifactId, targetKey);
}

// ── Failures ────────────────────────────────────────────────────────────────

/** Records the failure; returns whether the reason differs from the one already stored. */
export function recordReconcileFailure(ctx: StateContext, artifactId: string, reason: string, targetKey = ""): boolean {
  const key = compositeKey(artifactId, targetKey);
  const existing = ctx.state.failures[key];
  const reasonChanged = existing?.reason !== reason;
  ctx.state.failures[key] = {
    reason,
    since: existing?.since ?? ctx.now().toISOString(),
    streak: (existing?.streak ?? 0) + 1,
  };
  return reasonChanged;
}

export function clearFailure(ctx: StateContext, artifactId: string, targetKey = ""): void {
  delete ctx.state.failures[compositeKey(artifactId, targetKey)];
}

// ── Pending apply failures (sent with the next report) ─────────────────────

export function queueApplyFailure(
  ctx: StateContext,
  artifact: { artifactId: string; repoFullName: string; primaryHash: string },
  reason: string,
): void {
  ctx.state.pendingApplyFailures.push({
    artifactId: artifact.artifactId,
    repository: reportedRepository(artifact.repoFullName),
    contentHash: artifact.primaryHash,
    reason: reason.slice(0, FAILURE_REASON_MAX),
  });
}

/** Read-and-clear. */
export function takePendingApplyFailures(ctx: StateContext): ApplyFailure[] {
  const queue = ctx.state.pendingApplyFailures;
  ctx.state.pendingApplyFailures = [];
  return queue;
}

// ── View-model ──────────────────────────────────────────────────────────────

function itemStatus(s: SnapshotEntry, ledgerEntry: LedgerEntry | null, failure: FailureEntry | null): EnforcementItemStatus {
  if (s.expired === true) return "ended";
  if (failure !== null) return "failed";
  if (ledgerEntry !== null && ledgerEntry.appliedHash === s.primaryHash) return "in_sync";
  return "pending";
}

function itemDiff(ledgerEntry: LedgerEntry | null): EnforcementDiff | null {
  if (ledgerEntry === null) return null;
  return {
    before: ledgerEntry.previousPreview,
    after: ledgerEntry.newPreview,
    truncated: ledgerEntry.newPreview.length >= PREVIEW_MAX_BYTES,
  };
}

// Worst status wins the rollup. `ended` never competes: a group is ended only when every target is,
// and otherwise ended targets are left out, so a stale mark cannot hide a live target's failure.
const STATUS_RANK: Record<EnforcementItemStatus, number> = { in_sync: 0, pending: 1, failed: 2, ended: 0 };

/**
 * Join snapshot, ledger and failures into one row per artifactId. Repo-local targets roll up:
 * worst status wins, the most recently reconciled target represents name/path/diff.
 */
export function buildEnforcementState(ctx: StateContext): EnforcementState {
  const snapshot = readEnforcementSnapshot(ctx);
  const ledger = ctx.state.ledger;
  const failures = ctx.state.failures;

  const order: string[] = [];
  const groups = new Map<string, SnapshotEntry[]>();
  for (const s of snapshot) {
    let g = groups.get(s.artifactId);
    if (g === undefined) {
      g = [];
      groups.set(s.artifactId, g);
      order.push(s.artifactId);
    }
    g.push(s);
  }

  const items: EnforcementItem[] = order.map((artifactId) => {
    const subs = groups.get(artifactId)!.map((s) => {
      const key = compositeKey(s.artifactId, s.targetKey ?? "");
      const ledgerEntry = ledger[key] ?? null;
      const failure = failures[key] ?? null;
      return { s, ledgerEntry, failure, status: itemStatus(s, ledgerEntry, failure) };
    });

    const rep = subs.reduce((best, cur) =>
      (cur.ledgerEntry?.reconciledAt ?? "") > (best.ledgerEntry?.reconciledAt ?? "") ? cur : best,
    );
    const ended = subs.every((x) => x.s.expired === true);
    const liveSubs = ended ? subs : subs.filter((x) => x.status !== "ended");
    const status = ended
      ? "ended"
      : liveSubs.reduce<EnforcementItemStatus>(
          (acc, cur) => (STATUS_RANK[cur.status] > STATUS_RANK[acc] ? cur.status : acc),
          liveSubs[0]!.status,
        );
    const lastReconciledAt = subs.reduce<string | null>((acc, cur) => {
      const t = cur.ledgerEntry?.reconciledAt ?? null;
      return t !== null && (acc === null || t > acc) ? t : acc;
    }, null);

    return {
      artifactId,
      name: rep.s.name,
      ideType: rep.s.ideType,
      installPath: rep.s.installPath,
      status,
      enforceUntil: ended ? (subs.find((x) => x.s.enforceUntil !== undefined)?.s.enforceUntil ?? null) : null,
      lastReconciledAt,
      lastReconcileKind: rep.ledgerEntry?.kind ?? null,
      gitTracked: subs.some((x) => x.ledgerEntry?.gitTracked === true),
      targets: subs.map(({ s, ledgerEntry, failure, status: targetStatus }) => ({
        root: s.targetKey ? s.targetKey : null,
        repoFullName: s.repoFullName ?? null,
        placedPaths: s.placedPaths ?? [s.installPath],
        status: targetStatus,
        lastReconciledAt: ledgerEntry?.reconciledAt ?? null,
        failureReason: failure?.reason ?? null,
        gitTracked: ledgerEntry?.gitTracked === true,
      })),
      diff: itemDiff(rep.ledgerEntry),
      failureReason: subs.find((x) => x.failure !== null)?.failure?.reason ?? null,
      persistentFailure: subs.some((x) => x.failure !== null && x.failure.streak >= PERSISTENT_FAILURE_STREAK),
    };
  });

  const lastCycleAt = items.reduce<string | null>(
    (acc, i) => (i.lastReconciledAt !== null && (acc === null || i.lastReconciledAt > acc) ? i.lastReconciledAt : acc),
    null,
  );

  return { items, lastCycleAt };
}
