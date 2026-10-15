import { spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import { dirname, isAbsolute, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";

interface Reply {
  ok: boolean;
  data?: string;
  created?: string;
  exists?: boolean;
  removed?: boolean;
  error?: string;
  code?: string;
}

// Both the built package and the compiled test tree use the same bundled native helper.
const helperCandidates = [
  new URL("../native/fs-helper", import.meta.url),
  new URL("../../../dist/native/fs-helper", import.meta.url),
];

/** Directory-handle-relative operations; the configured root and its ancestors are trusted. */
export function anchoredOperation(
  operation: "read" | "stat" | "write" | "append" | "remove" | "rmdir",
  root: string,
  path: string,
  options: { data?: Uint8Array; executable?: boolean } = {},
): Reply {
  const rootAbs = resolve(root);
  const name = relative(rootAbs, resolve(path));
  if (name === "" || name === ".." || name.startsWith("../") || isAbsolute(name)) {
    throw new Error(`filesystem path is not below its root: ${path}`);
  }
  const helper = helperCandidates.map((candidate) => fileURLToPath(candidate)).find((candidate) => existsSync(candidate));
  if (helper === undefined) {
    throw new Error("native filesystem helper is missing; rebuild the agent with pnpm --filter @harness-control-plane/agent build");
  }
  const result = spawnSync(helper, [], {
    input: JSON.stringify({ operation, root: rootAbs, path: name, ...options, data: options.data === undefined ? undefined : Buffer.from(options.data).toString("base64") }),
    encoding: "utf8",
    maxBuffer: 24 * 1024 * 1024,
    timeout: 10_000,
    windowsHide: true,
  });
  if (result.error !== undefined) throw result.error;
  if (result.status !== 0) throw new Error(`native filesystem helper failed: ${result.stderr.trim() || result.signal || result.status}`);
  const reply = JSON.parse(result.stdout) as Reply;
  if (reply.ok !== true) {
    const reason = reply.error ?? "anchored filesystem operation failed";
    const error = new Error(`${reason.includes("escapes from parent") ? "filesystem path escapes root: " : ""}${reason}: ${path}`) as NodeJS.ErrnoException;
    error.code = reply.code;
    throw error;
  }
  return reply;
}

export function readAnchoredFile(root: string, path: string): Buffer {
  return Buffer.from(anchoredOperation("read", root, path).data ?? "", "base64");
}

export function anchoredPathExists(root: string, path: string): boolean {
  try {
    return anchoredOperation("stat", root, path).exists === true;
  } catch (error) {
    if (["ENOENT", "ENOTDIR"].includes((error as NodeJS.ErrnoException).code ?? "")) return false;
    // Unknown state must not be treated as an empty slot that may be overwritten.
    return true;
  }
}

/** Standalone callers explicitly trust the containing directory; engine callers pass their scope root. */
export function trustedParent(path: string): string {
  return dirname(resolve(path));
}
