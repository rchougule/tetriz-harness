import { createHash } from "node:crypto";
import { join } from "node:path";
import { noopLogger, type EngineContext } from "../src/engine/context.js";
import { compositeKey, emptyState, readEnforcementSnapshot, writeEnforcementSnapshot, type SnapshotEntry } from "../src/engine/state.js";
import type { EnforcedArtifact } from "../src/contract.js";

export function sha256(s: string): string {
  return createHash("sha256").update(s).digest("hex");
}

/**
 * A context rooted at `<dir>/home` whose every session-store probe points at a path that does not
 * exist, so the live-session guard fails open instead of seeing this machine's real sessions.
 */
export function testContext(dir: string): EngineContext {
  const none = join(dir, "no-such-session-dir");
  return {
    state: emptyState(),
    log: noopLogger,
    now: () => new Date(),
    paths: {
      home: join(dir, "home"),
      codexHome: none,
      claudeProjects: none,
      cursorDb: none,
      antigravityDb: none,
      kiroStateDb: none,
      copilotSessions: none,
    },
  };
}

/**
 * Record in the snapshot that an earlier pass placed `paths` (install path → hash written) for `a`, the
 * proof of ownership the engine needs before it replaces an existing file.
 */
export function recordPlaced(
  ctx: EngineContext,
  a: Pick<EnforcedArtifact, "artifactId" | "ideType" | "type" | "name" | "scope" | "installPath" | "primaryHash">,
  paths: Record<string, string>,
  targetKey = "",
): void {
  const entry: SnapshotEntry = {
    artifactId: a.artifactId,
    ideType: a.ideType,
    type: a.type,
    name: a.name,
    scope: a.scope,
    installPath: a.installPath,
    primaryHash: a.primaryHash,
    isHook: a.type === "ARTIFACT_TYPE_HOOK",
    placedPaths: Object.keys(paths),
    placedHashes: { ...paths },
  };
  if (targetKey !== "") entry.targetKey = targetKey;
  const key = compositeKey(a.artifactId, targetKey);
  writeEnforcementSnapshot(ctx, [
    ...readEnforcementSnapshot(ctx).filter((e) => compositeKey(e.artifactId, e.targetKey ?? "") !== key),
    entry,
  ]);
}
