import type { ApplyFailure } from "./contract.js";

/**
 * in_sync: the last write matches the enforced set. pending: a mismatch has not converged yet
 * (next pass, or held back by the live-session guard). failed: the last attempt errored.
 * ended: the enforcement window closed and the agent leaves the artifact as it is.
 */
export type EnforcementItemStatus = "in_sync" | "pending" | "failed" | "ended";

export type ReconcileKind = "installed" | "updated" | "reverted";

export interface EnforcementDiff {
  /** Content just before the write, or null for a fresh install. Truncated for display. */
  before: string | null;
  after: string;
  truncated: boolean;
}

/** One place an artifact was written: the machine (root null) or one checkout. */
export interface EnforcementTarget {
  root: string | null;
  repoFullName: string | null;
  placedPaths: string[];
  status: EnforcementItemStatus;
  lastReconciledAt: string | null;
  failureReason: string | null;
  /** Git tracks the enforced path in this checkout, so the write shows in `git status`. */
  gitTracked: boolean;
}

export interface EnforcementItem {
  artifactId: string;
  name: string;
  ideType: string;
  installPath: string;
  /** Never empty. */
  targets: EnforcementTarget[];
  status: EnforcementItemStatus;
  enforceUntil: string | null;
  lastReconciledAt: string | null;
  lastReconcileKind: ReconcileKind | null;
  gitTracked: boolean;
  diff: EnforcementDiff | null;
  failureReason: string | null;
  /** Failed on consecutive passes, as opposed to a single transient miss. */
  persistentFailure: boolean;
}

export interface EnforcementState {
  items: EnforcementItem[];
  lastCycleAt: string | null;
}

export interface SyncResult {
  startedAt: string;
  finishedAt: string;
  /** False when no fresh enforced set could be read; nothing was written. */
  fetched: boolean;
  fetchError?: string;
  /** The server's kill switch was on: nothing was written or retracted. */
  paused: boolean;
  /** The pass aborted after the read (for example a body-URL request failed); retried next pass. */
  passError?: string;
  /** Artifacts served, and (artifact, target) pairs resolved on this machine. */
  artifacts: number;
  targets: number;
  /** Failures recorded during this pass (also sent to the server). */
  applyFailures: ApplyFailure[];
  /** Files left on disk, from a retraction or a file dropped from a skill, because they changed since install; reported until gone. */
  retained: Array<{ artifactId: string; root: string; sourcePath: string; reason: string }>;
  reported: boolean;
  reportError?: string;
  state: EnforcementState;
}
