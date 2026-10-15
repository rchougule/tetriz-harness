import { SCOPE_REPO_LOCAL, type EnforcedArtifact } from "../contract.js";
import type { ActivationScope } from "./hookActivation.js";

/**
 * One convergence target. User-global is the single sentinel (targetKey ""); repo-local is one per
 * checkout, keyed on and rooted at its git top-level, so N checkouts keep independent state.
 */
export interface ConvergenceTarget {
  targetKey: string;
  root: string;
}

export interface TargetedArtifact {
  artifact: EnforcedArtifact;
  target: ConvergenceTarget;
}

/** The normalized per-file unit the reconcile loop works on; installPath is scope-root-relative. */
export interface EnforcedFile {
  installPath: string;
  fileId: string;
  hash: string;
  executable: boolean;
  isPrimary: boolean;
}

export type HookArtifact = EnforcedArtifact & { hookConfig: NonNullable<EnforcedArtifact["hookConfig"]> };

/**
 * A hook whose activation merges into a co-owned settings/hooks file. A hook-typed artifact with no
 * command (a self-contained hook file) is not one; gate that case by type instead.
 */
export function isHookArtifact(a: EnforcedArtifact): a is HookArtifact {
  return a.hookCommand !== "" && a.hookConfig !== undefined;
}

export function activationScopeOf(scope: string): ActivationScope {
  return scope === SCOPE_REPO_LOCAL ? "repo" : "user";
}

/**
 * The file set to converge. Without a `files` list the scalar primary is the whole artifact, so an
 * older server still converges. The primary is the entry whose destination equals installPath.
 */
export function resolveArtifactFiles(a: EnforcedArtifact): EnforcedFile[] {
  if (a.files && a.files.length > 0) {
    return a.files.map((f) => ({
      installPath: f.relativePath,
      fileId: f.fileId,
      hash: f.hash,
      executable: f.executable,
      isPrimary: f.relativePath === a.installPath,
    }));
  }
  return [
    {
      installPath: a.installPath,
      fileId: a.primaryFileId,
      hash: a.primaryHash,
      executable: isHookArtifact(a),
      isPrimary: true,
    },
  ];
}
