import { applyHookMerge, applyHookRemoval, type HookMergeResult, type HookMergeSpec } from "./hookMerge.js";

// Codex's hooks.json uses the same nested matcher-group shape as Claude Code's settings.json, so the
// same engine applies. A Codex entry's extra `statusMessage` survives because identity is `command`.

export function applyCodexHookMerge(hooksJsonPath: string, spec: HookMergeSpec, root?: string): HookMergeResult {
  return applyHookMerge(hooksJsonPath, spec, root);
}

export function applyCodexHookRemoval(hooksJsonPath: string, spec: HookMergeSpec, root?: string): HookMergeResult {
  return applyHookRemoval(hooksJsonPath, spec, root);
}
