import { dirname, join } from "node:path";
import { IDE_CLAUDE_CODE, IDE_CODEX, IDE_CURSOR } from "../contract.js";
import { resolveInstallPath } from "./fsops.js";
import { applyHookMerge, applyHookRemoval, type HookMergeResult, type HookMergeSpec } from "./hookMerge.js";
import { applyCodexHookMerge, applyCodexHookRemoval } from "./codexHookMerge.js";
import { applyCursorHookMerge, applyCursorHookRemoval } from "./cursorHookMerge.js";

// A hook's script is placed like any file; its activation entry lives in a tool-specific file that
// sits beside the placed `hooks/` dir, derived from the script path so activation always lands next to
// wherever the script was written, at either scope.

type Merger = (activationPath: string, spec: HookMergeSpec, root?: string) => HookMergeResult;

export type ActivationScope = "repo" | "user";

interface IdeActivation {
  basename: (scope: ActivationScope) => string;
  merge: Merger;
  remove: Merger;
}

const ACTIVATION_BY_IDE: Record<string, IdeActivation> = {
  // A checkout's settings.json is usually committed, so a repo-scoped write goes to the local file;
  // ~/.claude/settings.json is in nobody's repo and is the file read at user scope.
  [IDE_CLAUDE_CODE]: {
    basename: (scope) => (scope === "repo" ? "settings.local.json" : "settings.json"),
    merge: applyHookMerge,
    remove: applyHookRemoval,
  },
  [IDE_CODEX]: { basename: () => "hooks.json", merge: applyCodexHookMerge, remove: applyCodexHookRemoval },
  [IDE_CURSOR]: { basename: () => "hooks.json", merge: applyCursorHookMerge, remove: applyCursorHookRemoval },
};

export function ideSupportsHookActivation(ideType: string): boolean {
  return ideType in ACTIVATION_BY_IDE;
}

/** Absolute activation file path, or null for an unsupported ide or a path escaping `root`. */
export function activationFilePath(
  ideType: string,
  root: string,
  scriptInstallPath: string,
  scope: ActivationScope,
): string | null {
  const rel = activationRelPath(ideType, scriptInstallPath, scope);
  if (rel === null) return null;
  return resolveInstallPath(root, rel);
}

/** The activation file relative to the scope root, for reasoning about it before it exists. */
export function activationRelPath(ideType: string, scriptInstallPath: string, scope: ActivationScope): string | null {
  const ide = ACTIVATION_BY_IDE[ideType];
  if (ide === undefined) return null;
  return join(dirname(dirname(scriptInstallPath)), ide.basename(scope));
}

/** An unsupported ide is a no-op success: the script is placed, there is just nothing to activate. */
export function activateHook(
  ideType: string,
  root: string,
  scriptInstallPath: string,
  spec: HookMergeSpec,
  scope: ActivationScope,
): HookMergeResult {
  const ide = ACTIVATION_BY_IDE[ideType];
  if (ide === undefined) return { ok: true, changed: false };
  const path = activationFilePath(ideType, root, scriptInstallPath, scope);
  if (path === null) return { ok: false, reason: `hook activation path escapes root: ${scriptInstallPath}` };
  return ide.merge(path, spec, root);
}

export function deactivateHook(
  ideType: string,
  root: string,
  scriptInstallPath: string,
  spec: HookMergeSpec,
  scope: ActivationScope,
): HookMergeResult {
  const ide = ACTIVATION_BY_IDE[ideType];
  if (ide === undefined) return { ok: true, changed: false };
  const path = activationFilePath(ideType, root, scriptInstallPath, scope);
  if (path === null) return { ok: false, reason: `hook activation path escapes root: ${scriptInstallPath}` };
  return ide.remove(path, spec, root);
}
